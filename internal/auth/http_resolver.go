package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
)

type HTTPTokenResolver struct {
	baseURL    string
	httpClient *http.Client
}

func NewHTTPTokenResolver(baseURL string) *HTTPTokenResolver {
	return &HTTPTokenResolver{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

type verifyRequest struct {
	Token string `json:"token"`
}

type verifyResponse struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type verifyBotResponse struct {
	BotUID   string `json:"bot_uid"`
	OwnerUID string `json:"owner_uid"`
	SpaceID  string `json:"space_id"`
}

func (r *HTTPTokenResolver) ResolveUID(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", nil
	}

	body, _ := json.Marshal(verifyRequest{Token: token})
	req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/v1/auth/verify", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth verify failed: %d", resp.StatusCode)
	}

	var result verifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.UID, nil
}

// ResolveBot verifies a bot token and returns only server-resolved identity
// fields. Callers must never derive the space from a request header.
func (r *HTTPTokenResolver) ResolveBot(ctx context.Context, token string) (middleware.BotIdentity, error) {
	if token == "" {
		return middleware.BotIdentity{}, nil
	}
	body, _ := json.Marshal(map[string]string{"bot_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/auth/verify-bot", bytes.NewReader(body))
	if err != nil {
		return middleware.BotIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return middleware.BotIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return middleware.BotIdentity{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return middleware.BotIdentity{}, fmt.Errorf("bot auth verify failed: %d", resp.StatusCode)
	}
	var result verifyBotResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return middleware.BotIdentity{}, err
	}
	if result.BotUID == "" || result.OwnerUID == "" || result.SpaceID == "" {
		return middleware.BotIdentity{}, nil
	}
	return middleware.BotIdentity{BotUID: result.BotUID, OwnerUID: result.OwnerUID, SpaceID: result.SpaceID}, nil
}
