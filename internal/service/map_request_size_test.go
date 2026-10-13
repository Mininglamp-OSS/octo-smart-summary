package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise the real pre-send guard and each wrapper, without making any
// external request. A fatal guard error must never become a success marker.
func TestMapWrappersPropagateRequestTooLarge(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "oversized request reached transport", http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := NewLLMClient(srv.URL, "test", "test", 1, 4000, false, 1, nil)
	input := strings.Repeat("x", MaxRequestBodyBytes+1)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		call func() (string, int, string, error)
	}{
		{"chat", func() (string, int, string, error) {
			return client.CallMapWithModel(ctx, input, "source", 7, 1, "start", "end", "", "")
		}},
		{"chat stream", func() (string, int, string, error) {
			return client.CallMapStreamWithModel(ctx, input, "source", 7, 1, "start", "end", "", "", nil)
		}},
		{"document", func() (string, int, string, error) {
			return client.CallDocumentMapWithModel(ctx, input, "source", 7, 1, "")
		}},
		{"document stream", func() (string, int, string, error) {
			return client.CallDocumentMapStreamWithModel(ctx, input, "source", 7, 1, "", nil)
		}},
		{"mixed", func() (string, int, string, error) {
			return client.CallMixedMapWithModel(ctx, input, "source", 7, MixedMapScope{}, "", "")
		}},
		{"mixed stream", func() (string, int, string, error) {
			return client.CallMixedMapStreamWithModel(ctx, input, "source", 7, MixedMapScope{}, "", "", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, _, err := tc.call()
			if !errors.Is(err, ErrRequestTooLarge) || body != "" {
				t.Fatalf("body=%q err=%v; want empty body and ErrRequestTooLarge", body, err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("sent %d oversized requests", requests.Load())
	}
}
