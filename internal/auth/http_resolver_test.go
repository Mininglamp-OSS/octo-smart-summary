package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func botResolverWithResponse(t *testing.T, payload map[string]string) *HTTPTokenResolver {
	t.Helper()
	r := NewHTTPTokenResolver("http://octo.test")
	r.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1/auth/verify-bot" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if body["bot_token"] != "bf_secret" {
			t.Fatalf("body=%v", body)
		}
		encoded, _ := json.Marshal(payload)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header)}, nil
	})
	return r
}

func TestHTTPTokenResolverResolveBotContract(t *testing.T) {
	resolver := botResolverWithResponse(t, map[string]string{"bot_uid": "bot", "owner_uid": "owner", "space_id": "space"})
	got, err := resolver.ResolveBot(context.Background(), "bf_secret")
	if err != nil || got.BotUID != "bot" || got.OwnerUID != "owner" || got.SpaceID != "space" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestHTTPTokenResolverResolveBotFailsClosed(t *testing.T) {
	resolver := botResolverWithResponse(t, map[string]string{"bot_uid": "bot", "space_id": "space"})
	got, err := resolver.ResolveBot(context.Background(), "bf_secret")
	if err != nil || got.BotUID != "" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
