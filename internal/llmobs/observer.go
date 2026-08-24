package llmobs

import (
	"context"
	"log/slog"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// Observer is the production llmfallback.Observer: it feeds the metric set and
// emits one structured log record per model switch and per fully-exhausted
// call.
//
// Deliberately quiet on the success path. The per-attempt counter already
// carries retry volume, and logging every attempt would bury the two records
// that actually need a human.
type Observer struct {
	m   *Metrics
	log *slog.Logger
}

// New builds an Observer. A nil logger uses slog.Default().
func New(m *Metrics, logger *slog.Logger) *Observer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Observer{m: m, log: logger}
}

// Metrics exposes the underlying metric set for the /metrics handler.
func (o *Observer) Metrics() *Metrics { return o.m }

// ObserveAttempt implements llmfallback.Observer.
func (o *Observer) ObserveAttempt(e llmfallback.AttemptEvent) {
	o.m.ObserveAttempt(e)

	// A retry that is about to be spent is the only attempt-level event worth a
	// record: it is the leading indicator of a degrading primary, and without it
	// a same-model retry storm is completely invisible (the switch log only
	// fires once the model is abandoned).
	// Only when a retry actually follows. The final attempt of a budget also
	// classifies RetrySameModel — the model is abandoned right after it — so
	// logging unconditionally announced a retry that never happened.
	if e.Outcome == llmfallback.RetrySameModel && e.Attempt < e.MaxAttempts {
		o.log.Warn("llm attempt failed, retrying same model",
			slog.String("event", "llm.attempt.retry"),
			slog.String("path", string(e.Path)),
			slog.String("model", e.Model),
			slog.String("position", e.Position),
			slog.Int("attempt", e.Attempt),
			slog.Int64("duration_ms", e.Duration.Milliseconds()),
			slog.String("error", llmfallback.SafeErrorForLog(e.Err, 200)),
		)
	}
}

// ObserveSwitch implements llmfallback.Observer. This is THE record that makes
// silent quality drift visible: from here on, output comes from a different
// model than the one the deployment nominated.
func (o *Observer) ObserveSwitch(e llmfallback.SwitchEvent) {
	o.m.ObserveSwitch(e)

	// budget_starved is a configuration fault, not an upstream one, so it is
	// logged at Error: nobody upstream will fix it and it silently costs the
	// primary its retry budget on every single blip.
	level := slog.LevelWarn
	if e.Reason == llmfallback.ReasonBudgetStarved || e.Reason == llmfallback.ReasonDenied {
		level = slog.LevelError
	}
	o.log.Log(context.Background(), level, "llm falling back to next model",
		slog.String("event", "llm.model.switch"),
		slog.String("path", string(e.Path)),
		slog.String("from", e.From),
		slog.String("to", e.To),
		slog.String("from_position", e.FromPos),
		slog.String("reason", string(e.Reason)),
		slog.Int("attempts_spent", e.Attempts),
		slog.String("error", llmfallback.SafeErrorForLog(e.Err, 200)),
	)
}

// ObserveResult implements llmfallback.Observer. Only total exhaustion is
// logged — a call served by a fallback already produced a switch record.
func (o *Observer) ObserveResult(e llmfallback.ResultEvent) {
	o.m.ObserveResult(e)

	if !e.OK && e.Model == "" {
		// A caller walking away is not an outage. Logging a disconnect at Error
		// with "exhausted every configured model" put user-closed SSE streams
		// into the same record operators page on; switches=0 with one attempt
		// was the only tell.
		switch e.End {
		case llmfallback.RunEndCancelled:
			o.log.Info("llm call ended by caller cancellation",
				slog.String("event", "llm.call.cancelled"),
				slog.String("path", string(e.Path)),
				slog.Int("switches", e.Switches),
				slog.Int64("duration_ms", e.Duration.Milliseconds()),
				slog.String("error", llmfallback.SafeErrorForLog(e.Err, 200)),
			)
			return
		case llmfallback.RunEndTimedOut:
			// Warn, not Info: nobody walked away — we ran out of our own budget,
			// which is what an operator needs to see. Not Error either: no model
			// was proven bad, so it must not read as a provider outage.
			o.log.Warn("llm call ran out of its time budget",
				slog.String("event", "llm.call.timeout"),
				slog.String("path", string(e.Path)),
				slog.Int("switches", e.Switches),
				slog.Int64("duration_ms", e.Duration.Milliseconds()),
				slog.String("error", llmfallback.SafeErrorForLog(e.Err, 200)),
			)
			return
		}
		o.log.Error("llm call exhausted every configured model",
			slog.String("event", "llm.call.exhausted"),
			slog.String("path", string(e.Path)),
			slog.Int("switches", e.Switches),
			slog.Int64("duration_ms", e.Duration.Milliseconds()),
			slog.String("error", llmfallback.SafeErrorForLog(e.Err, 200)),
		)
	}
}
