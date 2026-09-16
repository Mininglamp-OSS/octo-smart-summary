package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestDocumentPreviewLimiter covers the admission gate's own contract.
func TestDocumentPreviewLimiter(t *testing.T) {
	t.Run("caps concurrent slots per user", func(t *testing.T) {
		l := newDocumentSummaryLimiter(2)
		r1, ok1 := l.acquire("u1")
		_, ok2 := l.acquire("u1")
		_, ok3 := l.acquire("u1")
		if !ok1 || !ok2 {
			t.Fatalf("first two acquisitions should succeed, got %v %v", ok1, ok2)
		}
		if ok3 {
			t.Error("third concurrent acquisition should be rejected")
		}
		// Releasing one frees exactly one slot.
		r1()
		if _, ok := l.acquire("u1"); !ok {
			t.Error("a slot should be available after release")
		}
	})

	t.Run("users do not share a budget", func(t *testing.T) {
		l := newDocumentSummaryLimiter(1)
		if _, ok := l.acquire("u1"); !ok {
			t.Fatal("u1 should be admitted")
		}
		if _, ok := l.acquire("u2"); !ok {
			t.Error("u2 must not be blocked by u1's in-flight request")
		}
	})

	t.Run("release is idempotent", func(t *testing.T) {
		// A double release would hand out phantom capacity, which is worse than the
		// unbounded case it replaces because it would look enforced.
		l := newDocumentSummaryLimiter(1)
		rel, _ := l.acquire("u1")
		rel()
		rel()
		rel()
		if _, ok := l.acquire("u1"); !ok {
			t.Fatal("slot should be free")
		}
		if _, ok := l.acquire("u1"); ok {
			t.Error("double release handed out extra capacity")
		}
	})

	t.Run("does not leak map entries", func(t *testing.T) {
		l := newDocumentSummaryLimiter(2)
		for i := 0; i < 100; i++ {
			rel, _ := l.acquire(strings.Repeat("u", i+1))
			rel()
		}
		l.mu.Lock()
		n := len(l.inFlight)
		l.mu.Unlock()
		if n != 0 {
			t.Errorf("limiter retained %d entries after all releases; the map is keyed by "+
				"user id and would grow for the process lifetime", n)
		}
	})

	t.Run("is race free", func(t *testing.T) {
		l := newDocumentSummaryLimiter(4)
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if rel, ok := l.acquire("u1"); ok {
					rel()
				}
			}()
		}
		wg.Wait()
		l.mu.Lock()
		n := l.inFlight["u1"]
		l.mu.Unlock()
		if n != 0 {
			t.Errorf("in-flight count did not return to zero: %d", n)
		}
	})

	t.Run("zero max disables the gate", func(t *testing.T) {
		l := newDocumentSummaryLimiter(0)
		for i := 0; i < 10; i++ {
			if _, ok := l.acquire("u1"); !ok {
				t.Fatal("a zero limit must not reject")
			}
		}
	})
}

// TestStreamDocumentPreview_InFlightCapRejects drives the gate through the real
// handler: the endpoint is directly callable and, on the inline path, resolves
// document_id against nothing, so this is the only server-side control standing
// between one authenticated account and unbounded fan-out of LLM completions.
func TestStreamDocumentPreview_InFlightCapRejects(t *testing.T) {
	prev := documentSummaryLimiterInstance
	documentSummaryLimiterInstance = newDocumentSummaryLimiter(1)
	t.Cleanup(func() { documentSummaryLimiterInstance = prev })

	// Occupy the single slot for the user the test harness authenticates as.
	release, ok := documentSummaryLimiterInstance.acquire("u")
	if !ok {
		t.Fatal("failed to occupy the slot")
	}
	defer release()

	h := &AgentSummaryHandler{llmApiURL: "http://127.0.0.1:1/v1", llmModel: "m", llmTimeout: 1}
	w := runPreviewRecorder(t, h, `{"document_id":"d1","content":"正文"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("want HTTP 429 when the per-user cap is reached, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Error("a 429 from the admission gate should carry Retry-After")
	}
	// No completion may be bought for a rejected request.
	if strings.Contains(w.Body.String(), "event: start") {
		t.Error("a rejected request must not open an SSE stream")
	}
}

// TestStreamDocumentPreview_SlotIsReleased pins the other direction: a completed
// request must free its slot, or the endpoint bricks itself after N requests.
func TestStreamDocumentPreview_SlotIsReleased(t *testing.T) {
	prev := documentSummaryLimiterInstance
	documentSummaryLimiterInstance = newDocumentSummaryLimiter(1)
	t.Cleanup(func() { documentSummaryLimiterInstance = prev })

	h := &AgentSummaryHandler{llmApiURL: "http://127.0.0.1:1/v1", llmModel: "m", llmTimeout: 1}
	for i := 0; i < 3; i++ {
		w := runPreviewRecorder(t, h, `{"document_id":"d1","content":"正文"}`)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rejected; the previous request did not release its slot", i+1)
		}
	}
}

// TestStreamDocumentPreview_InvalidRequestDoesNotConsumeSlot pins the gate's
// placement: it is taken after validation, so a caller cannot exhaust another
// request's capacity with malformed input that never reaches the model.
func TestStreamDocumentPreview_InvalidRequestDoesNotConsumeSlot(t *testing.T) {
	l := newDocumentSummaryLimiter(1)
	prev := documentSummaryLimiterInstance
	documentSummaryLimiterInstance = l
	t.Cleanup(func() { documentSummaryLimiterInstance = prev })

	gin.SetMode(gin.TestMode)
	h := &AgentSummaryHandler{llmApiURL: "http://llm.local", llmModel: "m"}
	for _, body := range []string{"{ not json", `{"version":"v1"}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/summaries/document/preview", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("space_id", "sp")
		c.Set("user_id", "u")
		h.StreamDocumentPreview(c)
	}
	l.mu.Lock()
	n := l.inFlight["u"]
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("rejected requests consumed %d slots", n)
	}
}
