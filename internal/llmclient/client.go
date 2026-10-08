// Package llmclient owns the shared OpenAI-compatible Chat Completions wire,
// transport, retry/fallback and response decoding used by Agent and Worker.
package llmclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmcompat"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

type Client struct {
	apiURL string
	apiKey string
	models []string
	http   *http.Client
}

func New(apiURL, apiKey, primaryModel string, fallbackModels []string, httpClient *http.Client) *Client {
	models := make([]string, 0, len(fallbackModels)+1)
	seen := make(map[string]bool, len(fallbackModels)+1)
	for _, model := range append([]string{primaryModel}, fallbackModels...) {
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{
		apiURL: strings.TrimRight(apiURL, "/"),
		apiKey: apiKey,
		models: models,
		http:   httpClient,
	}
}

func (c *Client) Models() []string {
	return append([]string(nil), c.models...)
}

func buildRequest(model string, req Request, stream bool) requestBody {
	thinking, kwargs := llmcompat.ThinkingConfig(model, req.EnableThinking)
	body := requestBody{
		Model:              model,
		Messages:           req.Messages,
		Temperature:        llmcompat.Temperature(model, req.Temperature),
		MaxTokens:          req.MaxTokens,
		Tools:              req.Tools,
		ChatTemplateKwargs: kwargs,
		Thinking:           thinking,
		Stream:             stream,
	}
	switch req.ToolChoice.Mode {
	case ToolChoiceAuto:
		body.ToolChoice = "auto"
	case ToolChoiceForced:
		if config.IsKimiModel(model) {
			body.ToolChoice = "auto"
		} else {
			body.ToolChoice = forcedToolChoice{
				Type:     "function",
				Function: forcedToolChoiceFunction{Name: req.ToolChoice.FunctionName},
			}
		}
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return body
}

func normalizedOptions(opts CallOptions) CallOptions {
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return opts
}

func (c *Client) Complete(ctx context.Context, req Request, opts CallOptions) (Result, error) {
	opts = normalizedOptions(opts)
	res, model, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:          c.models,
		PerModelTimeout: opts.PerModelTimeout,
		MaxAttempts:     opts.MaxAttempts,
		Path:            opts.Path,
		Backoff:         opts.Backoff,
	}, func(ctx context.Context, model string) (Response, llmfallback.Outcome, error) {
		payload, err := MarshalRequest(model, req, false)
		if err != nil {
			return Response{}, llmfallback.Terminal, fmt.Errorf("marshal request: %w", err)
		}
		resp, cancel, err := c.do(ctx, payload, opts.AttemptTimeout, false)
		if cancel != nil {
			defer cancel()
		}
		if err != nil {
			outcome, classified := classifyTransportError(ctx, err)
			return Response{}, outcome, classified
		}
		defer resp.Body.Close()
		if opts.OnHTTPStatus != nil {
			opts.OnHTTPStatus(resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return Response{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode), httpStatusError(resp)
		}
		body, err := readBounded(resp.Body, opts.MaxResponseBytes)
		if err != nil {
			return classifyReadError(ctx, err)
		}
		var decoded Response
		if err := json.Unmarshal(body, &decoded); err != nil {
			return Response{}, protocolErrorOutcome(opts), fmt.Errorf("decode LLM response: %w", err)
		}
		if len(decoded.Choices) == 0 {
			return Response{}, protocolErrorOutcome(opts), fmt.Errorf("LLM returned no choices")
		}
		if opts.ValidateResponse != nil {
			outcome, err := opts.ValidateResponse(model, decoded)
			if err != nil || outcome != llmfallback.Success {
				return decoded, outcome, err
			}
		}
		return decoded, llmfallback.Success, nil
	})
	return Result{Response: res, Model: model}, err
}

func protocolErrorOutcome(opts CallOptions) llmfallback.Outcome {
	if opts.TerminalOnProtocolError {
		return llmfallback.Terminal
	}
	return llmfallback.RetrySameModel
}

func (c *Client) Stream(ctx context.Context, req Request, opts CallOptions, onDelta func(string) error) (StreamResult, error) {
	opts = normalizedOptions(opts)
	res, model, err := llmfallback.Run(ctx, llmfallback.Config{
		Models:          c.models,
		PerModelTimeout: opts.PerModelTimeout,
		MaxAttempts:     opts.MaxAttempts,
		Path:            opts.Path,
		Backoff:         opts.Backoff,
	}, func(ctx context.Context, model string) (StreamResult, llmfallback.Outcome, error) {
		payload, err := MarshalRequest(model, req, true)
		if err != nil {
			return StreamResult{}, llmfallback.Terminal, fmt.Errorf("marshal request: %w", err)
		}
		resp, cancel, err := c.do(ctx, payload, opts.AttemptTimeout, true)
		if cancel != nil {
			defer cancel()
		}
		if err != nil {
			outcome, classified := classifyTransportError(ctx, err)
			return StreamResult{}, outcome, classified
		}
		defer resp.Body.Close()
		if opts.OnHTTPStatus != nil {
			opts.OnHTTPStatus(resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return StreamResult{}, llmfallback.ClassifyNonOKStatus(resp.StatusCode), httpStatusError(resp)
		}
		result, outcome, err := decodeStream(resp.Body, opts.MaxResponseBytes, onDelta)
		if err != nil {
			if ctx.Err() != nil {
				return result, llmfallback.Terminal, ctx.Err()
			}
			var tooLarge *responseTooLargeError
			if errors.As(err, &tooLarge) {
				return result, llmfallback.Terminal, err
			}
			return result, outcome, err
		}
		if opts.ValidateStream != nil {
			outcome, err = opts.ValidateStream(model, result)
			if result.Emitted && outcome != llmfallback.Success {
				outcome = llmfallback.Terminal
			}
			if err != nil || outcome != llmfallback.Success {
				return result, outcome, err
			}
		}
		return result, llmfallback.Success, nil
	})
	res.Model = model
	return res, err
}

func (c *Client) do(ctx context.Context, payload []byte, timeout time.Duration, stream bool) (*http.Response, context.CancelFunc, error) {
	reqCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, nil, &requestBuildError{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.http.Do(req)
	return resp, cancel, err
}

func classifyTransportError(parent context.Context, err error) (llmfallback.Outcome, error) {
	if parent.Err() != nil {
		return llmfallback.Terminal, parent.Err()
	}
	var buildErr *requestBuildError
	if errors.As(err, &buildErr) {
		return llmfallback.Terminal, err
	}
	return llmfallback.RetrySameModel, fmt.Errorf("LLM transport: %w", err)
}

func classifyReadError(parent context.Context, err error) (Response, llmfallback.Outcome, error) {
	if parent.Err() != nil {
		return Response{}, llmfallback.Terminal, parent.Err()
	}
	var tooLarge *responseTooLargeError
	if errors.As(err, &tooLarge) {
		return Response{}, llmfallback.Terminal, err
	}
	return Response{}, llmfallback.RetrySameModel, fmt.Errorf("read LLM response: %w", err)
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
	return &llmfallback.HTTPError{
		StatusCode: resp.StatusCode,
		Err: fmt.Errorf("LLM API error: status=%d body=%s", resp.StatusCode,
			llmfallback.SafeTextForLog(string(body), 200)),
	}
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, &responseTooLargeError{Limit: limit}
	}
	return body, nil
}

func decodeStream(r io.Reader, maxBytes int64, onDelta func(string) error) (StreamResult, llmfallback.Outcome, error) {
	var result StreamResult
	terminalSeen := false
	limited := &io.LimitedReader{R: r, N: maxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			terminalSeen = true
			break
		}
		var chunk Response
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return result, streamFailureOutcome(result.Emitted), fmt.Errorf("decode LLM stream chunk: %w", err)
		}
		if chunk.Usage.TotalTokens > 0 {
			result.Usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			terminalSeen = true
			result.FinishReason = choice.FinishReason
		}
		result.ReasoningSize += len(choice.Delta.ReasoningContent) + len(choice.Delta.Reasoning)
		if choice.Delta.Content == "" {
			continue
		}
		result.Content += choice.Delta.Content
		if onDelta != nil {
			result.Emitted = true
			if err := onDelta(choice.Delta.Content); err != nil {
				return result, llmfallback.Terminal, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, streamFailureOutcome(result.Emitted), fmt.Errorf("read LLM stream: %w", err)
	}
	if limited.N == 0 {
		return result, streamFailureOutcome(result.Emitted), &responseTooLargeError{Limit: maxBytes}
	}
	if !terminalSeen {
		return result, streamFailureOutcome(result.Emitted), fmt.Errorf("LLM stream ended without terminal marker")
	}
	return result, llmfallback.Success, nil
}

func streamFailureOutcome(emitted bool) llmfallback.Outcome {
	if emitted {
		return llmfallback.Terminal
	}
	return llmfallback.RetrySameModel
}
