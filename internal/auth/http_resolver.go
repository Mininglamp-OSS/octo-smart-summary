package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
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
