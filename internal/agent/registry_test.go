package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRegistry_RegisterAndSchemas(t *testing.T) {
	reg := NewRegistry()
	ts, th := ExtractTimeRangeTool()
	cs, ch := GetCurrentTimeTool()
	reg.Register(ts, th)
	reg.Register(cs, ch)

	if got := len(reg.Schemas()); got != 2 {
		t.Fatalf("Schemas len = %d, want 2", got)
	}
}

func TestRegistry_Dispatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "echo"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return "ok:" + string(args), nil
		})
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "boom"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			panic("kaboom")
		})

	tests := []struct {
		name       string
		tool       string
		args       string
		wantResult string
		wantErr    bool
		errSubstr  string
	}{
		{name: "known tool", tool: "echo", args: `{"a":1}`, wantResult: `ok:{"a":1}`},
		{name: "unknown tool", tool: "nope", args: `{}`, wantErr: true, errSubstr: "unknown tool"},
		{name: "handler panic recovered", tool: "boom", args: `{}`, wantErr: true, errSubstr: "panicked"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := reg.Dispatch(context.Background(), tt.tool, json.RawMessage(tt.args))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (res=%q)", res)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("error %q missing substr %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res != tt.wantResult {
				t.Fatalf("result = %q, want %q", res, tt.wantResult)
			}
		})
	}
}
