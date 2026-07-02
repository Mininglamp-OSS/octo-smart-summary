package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// gzipRows is a test helper: marshal rows to ndjson, gzip, return (bytes, hexSha256).
func gzipRows(t *testing.T, rows []BatchMessageRow) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal row: %v", err)
		}
		gw.Write(b)
		gw.Write([]byte("\n"))
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestSubmit_Success202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/batch" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok123" {
			t.Errorf("auth header = %q", got)
		}
		var body batchSubmitRequest
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Scope.Channels) != 2 {
			t.Errorf("channels len = %d", len(body.Scope.Channels))
		}
		if body.IsDeleted != 0 {
			t.Errorf("is_deleted = %d, want 0", body.IsDeleted)
		}
		w.Header().Set("X-Request-Id", "rid-1")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(batchSubmitResponse{TaskID: "ost_abc", RequestID: "rid-1"})
	}))
	defer srv.Close()

	c := NewOctoSearchBatchClient(srv.URL, "tok123")
	taskID, err := c.Submit(context.Background(), []string{"a@b", "grp1"}, 100, 200)
	if err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if taskID != "ost_abc" {
		t.Errorf("taskID = %q, want ost_abc", taskID)
	}
}

func TestSubmit_BaseURLTrailingSlashTrimmed(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(batchSubmitResponse{TaskID: "x"})
	}))
	defer srv.Close()

	c := NewOctoSearchBatchClient(srv.URL+"/", "t") // trailing slash
	if _, err := c.Submit(context.Background(), []string{"c"}, 1, 2); err != nil {
		t.Fatalf("Submit err: %v", err)
	}
	if gotPath != "/v1/messages/batch" {
		t.Errorf("path = %q, want /v1/messages/batch (no // doubling)", gotPath)
	}
}

func TestSubmit_TypedErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"400", http.StatusBadRequest, ErrBadRequest},
		{"401", http.StatusUnauthorized, ErrUnauthorized},
		{"404", http.StatusNotFound, ErrTaskNotFound},
		{"413", http.StatusRequestEntityTooLarge, ErrSingleTaskTooLarge},
		{"422", 422, ErrTimeRangeTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(`{"error_code":"x"}`))
			}))
			defer srv.Close()
			c := NewOctoSearchBatchClient(srv.URL, "t")
			_, err := c.Submit(context.Background(), []string{"a"}, 1, 2)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Submit %s: err = %v, want errors.Is(%v)", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestSubmit_413NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()
	c := NewOctoSearchBatchClient(srv.URL, "t")
	_, err := c.Submit(context.Background(), []string{"a"}, 1, 2)
	if !errors.Is(err, ErrSingleTaskTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("4xx must not retry: got %d calls, want 1", n)
	}
}

func TestSubmit_500RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(batchSubmitResponse{TaskID: "ost_ok"})
	}))
	defer srv.Close()

	// shrink backoff for the test by cancelling-free fast path: 500 backoff is
	// ~1s,2s — acceptable for a unit test (<5s). Use a generous ctx.
	c := NewOctoSearchBatchClient(srv.URL, "t")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	taskID, err := c.Submit(ctx, []string{"a"}, 1, 2)
	if err != nil {
		t.Fatalf("Submit err after retries: %v", err)
	}
	if taskID != "ost_ok" {
		t.Errorf("taskID = %q", taskID)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("calls = %d, want 3 (2 fails + 1 ok)", n)
	}
}

func TestSubmit_500ExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewOctoSearchBatchClient(srv.URL, "t")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.Submit(ctx, []string{"a"}, 1, 2)
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	// 5xx exhaustion must NOT be a typed sentinel (caller isolates it).
	if errors.Is(err, ErrBadRequest) || errors.Is(err, ErrUnauthorized) {
		t.Errorf("5xx exhaustion should be plain error, got %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != maxServerRetries+1 {
		t.Errorf("calls = %d, want %d", n, maxServerRetries+1)
	}
}

func TestPoll_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/messages/batch/ost_1" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(BatchStatus{
			Status:      "completed",
			ActualCount: 5,
			Parts:       []Part{{URL: "u", SHA256: "s", ChannelIDs: []string{"a"}, MessageCount: 5}},
		})
	}))
	defer srv.Close()
	c := NewOctoSearchBatchClient(srv.URL, "t")
	st, err := c.Poll(context.Background(), "ost_1")
	if err != nil {
		t.Fatalf("Poll err: %v", err)
	}
	if st.Status != "completed" || !st.IsTerminal() || st.ActualCount != 5 {
		t.Errorf("status = %+v", st)
	}
	if len(st.Parts) != 1 || st.Parts[0].MessageCount != 5 {
		t.Errorf("parts = %+v", st.Parts)
	}
}

func TestBatchStatus_IsTerminal(t *testing.T) {
	terminal := []string{"completed", "partial", "failed", "cancelled"}
	nonTerminal := []string{"queued", "running", ""}
	for _, s := range terminal {
		if !(&BatchStatus{Status: s}).IsTerminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if (&BatchStatus{Status: s}).IsTerminal() {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}

func TestDelete_Idempotent(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("method = %s", r.Method)
			}
			w.WriteHeader(status)
		}))
		c := NewOctoSearchBatchClient(srv.URL, "t")
		if err := c.Delete(context.Background(), "ost_x"); err != nil {
			t.Errorf("Delete on %d should be nil, got %v", status, err)
		}
		srv.Close()
	}
}

func TestDownloadAndParse_Success(t *testing.T) {
	rows := []BatchMessageRow{
		{MessageSeq: 1, FromUID: "u1", ChannelID: "a@b", Timestamp: 100, Payload: "hi"},
		{MessageSeq: 2, FromUID: "u2", ChannelID: "a@b", Timestamp: 200, Payload: "yo"},
	}
	gz, sum := gzipRows(t, rows)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gz)
	}))
	defer srv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	got, err := c.DownloadAndParse(context.Background(), []Part{{URL: srv.URL, SHA256: sum}})
	if err != nil {
		t.Fatalf("DownloadAndParse err: %v", err)
	}
	if len(got) != 2 || got[0].Payload != "hi" || got[1].MessageSeq != 2 {
		t.Errorf("rows = %+v", got)
	}
}

func TestDownloadAndParse_BadPartSkipped(t *testing.T) {
	goodRows := []BatchMessageRow{{MessageSeq: 1, ChannelID: "a", Payload: "ok"}}
	goodGz, goodSum := gzipRows(t, goodRows)
	badGz, _ := gzipRows(t, []BatchMessageRow{{MessageSeq: 9, Payload: "bad"}})

	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(goodGz) }))
	defer goodSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(badGz) }))
	defer badSrv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	// bad part declares a wrong sha256 -> skipped; good part survives.
	parts := []Part{
		{URL: badSrv.URL, SHA256: "deadbeef"},
		{URL: goodSrv.URL, SHA256: goodSum},
	}
	got, err := c.DownloadAndParse(context.Background(), parts)
	if err != nil {
		t.Fatalf("partial bad should return nil err, got %v", err)
	}
	if len(got) != 1 || got[0].Payload != "ok" {
		t.Errorf("want only good rows, got %+v", got)
	}
}

func TestDownloadAndParse_AllBadReturnsError(t *testing.T) {
	badGz, _ := gzipRows(t, []BatchMessageRow{{MessageSeq: 1}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(badGz) }))
	defer srv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	parts := []Part{{URL: srv.URL, SHA256: "wronghash"}}
	_, err := c.DownloadAndParse(context.Background(), parts)
	if err == nil {
		t.Fatal("all-parts-bad must return error, not silent empty")
	}
}

func TestDownloadAndParse_EmptyPartsNil(t *testing.T) {
	c := NewOctoSearchBatchClient("http://unused", "t")
	got, err := c.DownloadAndParse(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("empty parts -> (nil,nil), got (%v,%v)", got, err)
	}
}

func TestDownloadAndParse_MissingSHA256ReturnsError(t *testing.T) {
	rows := []BatchMessageRow{{MessageSeq: 1, Payload: "x"}}
	gz, _ := gzipRows(t, rows)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(gz) }))
	defer srv.Close()
	c := NewOctoSearchBatchClient("http://unused", "t")
	_, err := c.DownloadAndParse(context.Background(), []Part{{URL: srv.URL}}) // no SHA256
	if err == nil {
		t.Fatal("missing sha256 must return error")
	}
}

// TestFetchPartBytes_DisablesTransparentGzip verifies the part GET sends
// "Accept-Encoding: identity" so Go's default Transport never adds gzip and
// never transparently decompresses the response. Our part objects are gzip
// *payloads* and sha256 is computed over the raw gzip bytes; if the transport
// auto-decompressed a Content-Encoding: gzip response, ReadAll would yield
// decompressed ndjson, sha256 would never match, and parseGzipNDJSON would fail.
func TestFetchPartBytes_DisablesTransparentGzip(t *testing.T) {
	rows := []BatchMessageRow{{MessageSeq: 1, ChannelID: "a", Payload: "ok"}}
	gz, sum := gzipRows(t, rows)

	var gotAccEnc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccEnc = r.Header.Get("Accept-Encoding")
		// Mimic an object store that tags the gzip object at the HTTP layer.
		// With identity requested, Go must NOT auto-decompress; raw gz reaches us.
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz)
	}))
	defer srv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	got, err := c.DownloadAndParse(context.Background(), []Part{{URL: srv.URL, SHA256: sum}})
	if err != nil {
		t.Fatalf("DownloadAndParse err (transparent gzip likely re-enabled): %v", err)
	}
	if gotAccEnc != "identity" {
		t.Errorf("Accept-Encoding = %q, want \"identity\" (transparent gzip must be disabled)", gotAccEnc)
	}
	if len(got) != 1 || got[0].Payload != "ok" {
		t.Errorf("rows = %+v", got)
	}
}

// TestDownloadPart_MessageCountMismatchIsBadPart verifies the data-contract
// check: when the manifest declares message_count but the part's actual row
// count differs, the part is treated as corrupt. A single such part (after
// retries) makes DownloadAndParse surface an error rather than silently return
// a short result.
func TestDownloadPart_MessageCountMismatchIsBadPart(t *testing.T) {
	rows := []BatchMessageRow{
		{MessageSeq: 1, ChannelID: "a", Payload: "x"},
		{MessageSeq: 2, ChannelID: "a", Payload: "y"},
	}
	gz, sum := gzipRows(t, rows) // actually 2 rows
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(gz) }))
	defer srv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	// Manifest lies: claims 3 rows but the file holds 2 -> corrupt part.
	parts := []Part{{URL: srv.URL, SHA256: sum, MessageCount: 3}}
	_, err := c.DownloadAndParse(context.Background(), parts)
	if err == nil {
		t.Fatal("message_count mismatch (sole part) must return error, not a short silent result")
	}
}

// TestDownloadPart_MessageCountMismatchKeepsGoodSibling verifies B+ isolation:
// a row-count-mismatched part is skipped, but a healthy sibling part's rows are
// still returned (we do NOT fail the whole batch on one bad part).
func TestDownloadPart_MessageCountMismatchKeepsGoodSibling(t *testing.T) {
	goodRows := []BatchMessageRow{{MessageSeq: 1, ChannelID: "a", Payload: "ok"}}
	goodGz, goodSum := gzipRows(t, goodRows)
	badRows := []BatchMessageRow{{MessageSeq: 9, ChannelID: "b", Payload: "bad"}}
	badGz, badSum := gzipRows(t, badRows) // file has 1 row...

	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(goodGz) }))
	defer goodSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(badGz) }))
	defer badSrv.Close()

	c := NewOctoSearchBatchClient("http://unused", "t")
	parts := []Part{
		{URL: badSrv.URL, SHA256: badSum, MessageCount: 5}, // ...but manifest claims 5 -> bad, skipped
		{URL: goodSrv.URL, SHA256: goodSum, MessageCount: 1},
	}
	got, err := c.DownloadAndParse(context.Background(), parts)
	if err != nil {
		t.Fatalf("one mismatched + one good should return nil err, got %v", err)
	}
	if len(got) != 1 || got[0].Payload != "ok" {
		t.Errorf("want only good sibling rows preserved, got %+v", got)
	}
}

func TestParseGzipNDJSON_SkipsBlankLines(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	fmt.Fprint(gw, `{"message_seq":1,"payload":"a"}`+"\n\n"+`{"message_seq":2,"payload":"b"}`+"\n")
	gw.Close()
	rows, err := parseGzipNDJSON(buf.Bytes())
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if len(rows) != 2 || rows[1].MessageSeq != 2 {
		t.Errorf("rows = %+v", rows)
	}
}

// TestBackoffDelay_503Spacing verifies backoff branches on the real HTTP status
// code (not on string-matching "503" in the error text). 503 must be spaced
// >=min503Backoff even on early attempts; 500 follows plain exponential backoff.
func TestBackoffDelay_503Spacing(t *testing.T) {
	// attempt 0 for 503: base (1s) < 10s, so it must be lifted to min503Backoff.
	d := backoffDelay(0, http.StatusServiceUnavailable)
	if d < min503Backoff {
		t.Errorf("503 attempt0 delay = %v, want >= %v", d, min503Backoff)
	}

	// attempt 0 for 500: plain exponential base, must stay well under the 503 floor.
	d = backoffDelay(0, http.StatusInternalServerError)
	if d >= min503Backoff {
		t.Errorf("500 attempt0 delay = %v, want < %v", d, min503Backoff)
	}

	// A transport error (status 0) must NOT be treated as 503.
	d = backoffDelay(0, 0)
	if d >= min503Backoff {
		t.Errorf("status0 attempt0 delay = %v, want < %v (must not match 503)", d, min503Backoff)
	}

	// Guard against the old string-match bug: a 500 whose body happened to
	// contain "503" must not be spaced like a 503. We exercise this at the
	// status level — backoffDelay no longer sees the error string at all.
	d = backoffDelay(1, http.StatusInternalServerError) // base<<1 = 2s
	if d >= min503Backoff {
		t.Errorf("500 attempt1 delay = %v, want < %v", d, min503Backoff)
	}
}
