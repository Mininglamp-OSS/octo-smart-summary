package worker

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/notify"
)

// fakeNotifyDeliverer captures the SendMessage payloads so we can assert the
// failure-reason text actually reaching the IM bot has been sanitized.
type fakeNotifyDeliverer struct {
	mu        sync.Mutex
	sendCalls []notify.SendMessageRequest
}

func (f *fakeNotifyDeliverer) EnsureFriend(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeNotifyDeliverer) SendMessage(_ context.Context, _ string, msg notify.SendMessageRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendCalls = append(f.sendCalls, msg)
	return nil
}

// notifyTaskTerminal must run task.error_message through sanitizeErrorForUser
// before it reaches the IM payload. This guards against raw internal errors
// (DSN credentials, IPs, goroutine stack heads) being delivered to the user's
// DM via the failure-reason line of buildText.
//
// Regression test for PR#113 review by Jerry-Xin (2026-06-29): the task-level
// failure path previously fed *task.ErrorMessage straight to OnTaskTerminal,
// so the user DM rendered raw internal errors. The personal failure path
// already sanitized via markPersonalFailed; this brings the task path to
// parity at the single-point intercept (notifyTaskTerminal).
func TestNotifyTaskTerminal_SanitizesRawErrorBeforeDM(t *testing.T) {
	cases := []struct {
		name    string
		rawErr  string
		wantOut string // exact expected sanitized output
		mustNot []string
	}{
		{
			name:    "dsn-credentials",
			rawErr:  "save task: dial mysql user:s3cret@tcp(10.0.0.1:3306)/summary failed",
			wantOut: "AI 处理失败，请稍后重试",
			mustNot: []string{"user:s3cret", "10.0.0.1:3306", "user:s3cret@tcp"},
		},
		{
			name:    "ip-leak",
			rawErr:  "dial tcp 192.168.1.5:8443: connect: connection refused",
			wantOut: "AI 处理失败，请稍后重试",
			mustNot: []string{"192.168.1.5", "8443"},
		},
		{
			name:    "stack-trace-head",
			rawErr:  "goroutine 17 [running]:\nmain.failPipeline(0xc0001a23c0)",
			wantOut: "AI 处理失败，请稍后重试",
			mustNot: []string{"goroutine 17 [running]", "main.failPipeline", "0xc0001a23c0"},
		},
		{
			name:    "llm-api-error-mapped",
			rawErr:  "LLM API error: status=401 body=invalid key bearer-abc123",
			wantOut: "AI 服务暂时不可用，请稍后重试",
			mustNot: []string{"bearer-abc123", "status=401", "invalid key"},
		},
		{
			name:    "context-deadline-mapped",
			rawErr:  "context deadline exceeded after 30s on llm.example.com:9443",
			wantOut: "AI 处理超时，请稍后重试",
			mustNot: []string{"llm.example.com", "9443"},
		},
		{
			name:    "empty-error-message",
			rawErr:  "",
			wantOut: "", // empty errMsg → no "失败原因" line rendered (PR#113 R3 single-point sanitize)
			mustNot: []string{"失败原因"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupProcessorTestDB(t)
			// summary_notification lives in the same summary DB; AutoMigrate so
			// the real Notifier's claim INSERT succeeds.
			if err := db.AutoMigrate(&model.SummaryNotification{}); err != nil {
				t.Fatalf("automigrate notification: %v", err)
			}

			errMsg := tc.rawErr
			task := model.SummaryTask{
				TaskNo:            "TST-SAN-" + tc.name,
				Title:             "今日群聊",
				SpaceID:           "space-1",
				CreatorID:         "user-1",
				Status:            model.StatusFailed,
				TriggerType:       model.TriggerManual,
				OriginChannelType: model.OriginChannelGlobal,
				ErrorMessage:      &errMsg,
			}
			if err := db.Create(&task).Error; err != nil {
				t.Fatalf("save task: %v", err)
			}

			fake := &fakeNotifyDeliverer{}
			// Production wiring: cmd/summary-worker/main.go injects
			// worker.SanitizeErrorForUser via WithErrorSanitizer so the single
			// render-point sanitizer in notify.buildText covers both the
			// synchronous and the sweep/redeliver paths. Mirror that wiring
			// here so the test exercises the same behavior as production.
			n := notify.New(db, nil, fake, notify.Config{Enabled: true, MaxAttempts: 3}).
				WithErrorSanitizer(SanitizeErrorForUser)

			p := &Processor{db: db}
			p.SetNotifier(n)
			p.notifyTaskTerminal(task.ID, model.StatusFailed)

			if len(fake.sendCalls) != 1 {
				t.Fatalf("expected exactly 1 SendMessage call, got %d", len(fake.sendCalls))
			}
			// octo-server recognizes a plain-text bot message by "content" (type=1),
			// not "text"; assert against the server-recognized payload key.
			text, _ := fake.sendCalls[0].Payload["content"].(string)
			// The failure-reason line is "失败原因：<sanitized>" — assert the
			// sanitized whitelist mapping is what landed in the IM payload.
			if !strings.Contains(text, tc.wantOut) {
				t.Fatalf("expected DM text to contain sanitized %q, got %q", tc.wantOut, text)
			}
			for _, bad := range tc.mustNot {
				if strings.Contains(text, bad) {
					t.Fatalf("DM text leaked raw substring %q (full text: %q)", bad, text)
				}
			}
		})
	}
}

// notifyTaskTerminal must remain a no-op when notifier is unset (production
// wires nil when SUMMARY_NOTIFY_ENABLED=false). Guards against the sanitize
// change accidentally introducing a nil-deref on the disabled path.
func TestNotifyTaskTerminal_NilNotifierIsNoop(t *testing.T) {
	db := setupProcessorTestDB(t)
	errMsg := "irrelevant"
	task := model.SummaryTask{
		TaskNo:       "TST-SAN-NIL",
		SpaceID:      "space-1",
		CreatorID:    "user-1",
		Status:       model.StatusFailed,
		ErrorMessage: &errMsg,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("save task: %v", err)
	}
	p := &Processor{db: db}
	// p.notifier intentionally nil
	p.notifyTaskTerminal(task.ID, model.StatusFailed) // must not panic
}
