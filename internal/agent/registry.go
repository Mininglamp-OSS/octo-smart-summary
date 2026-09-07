package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Handler 是工具的实际执行体：吃 raw JSON args，回人类可读结果字符串。
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

type entry struct {
	schema Tool
	fn     Handler
	phase  ToolPhase
}

type terminalEntry struct {
	schema Tool
	fn     TerminalHandler
}

// ToolPhase describes the causal phase an ordinary tool belongs to when one
// planner step contains a workspace scope declaration. Keeping this metadata
// on registry entries prevents runner ordering from drifting when tools are
// renamed or new discovery tools are added.
type ToolPhase uint8

const (
	ToolPhaseDefault ToolPhase = iota
	ToolPhaseScopePreparation
	ToolPhaseScopeCommit
	ToolPhaseFetch
	ToolPhaseSummarize
)

// Registry 线程安全地保存工具 schema + handler，支持注册/取 schema/分发。
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]entry
	terminals map[string]terminalEntry
}

func NewRegistry() *Registry {
	return &Registry{
		tools:     make(map[string]entry),
		terminals: make(map[string]terminalEntry),
	}
}

func (r *Registry) Register(schema Tool, fn Handler) {
	r.RegisterWithPhase(schema, fn, GetToolPhase(schema.Function.Name))
}

func (r *Registry) RegisterWithPhase(schema Tool, fn Handler, phase ToolPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.terminals, schema.Function.Name)
	r.tools[schema.Function.Name] = entry{schema: schema, fn: fn, phase: phase}
}

// RegisterTerminal registers a tool whose successful dispatch terminates the
// Agent loop with a structured result. A name can represent either an ordinary
// or terminal tool, never both; the most recent registration wins.
func (r *Registry) RegisterTerminal(schema Tool, fn TerminalHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, schema.Function.Name)
	r.terminals[schema.Function.Name] = terminalEntry{schema: schema, fn: fn}
}

func (r *Registry) Schemas() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools)+len(r.terminals))
	for _, e := range r.tools {
		out = append(out, e.schema)
	}
	for _, e := range r.terminals {
		out = append(out, e.schema)
	}
	return out
}

// Has reports whether name belongs to the registered tool vocabulary. Trace
// logging uses this guard so a model-hallucinated tool name derived from user
// input never reaches logs as if it were trusted metadata.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.tools[name]; ok {
		return true
	}
	_, ok := r.terminals[name]
	return ok
}

// IsTerminal reports whether name is registered as a terminal tool.
func (r *Registry) IsTerminal(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.terminals[name]
	return ok
}

func (r *Registry) Phase(name string) ToolPhase {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if tool, ok := r.tools[name]; ok {
		return tool.phase
	}
	return ToolPhaseDefault
}

// Dispatch 按名分发。未知工具/handler panic 都转成错误返回，绝不中断回环。
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (result string, err error) {
	r.mu.RLock()
	e, ok := r.tools[name]
	_, terminal := r.terminals[name]
	r.mu.RUnlock()
	if !ok {
		if terminal {
			return "", fmt.Errorf("terminal tool %s must be dispatched with DispatchTerminal", name)
		}
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	defer func() {
		if p := recover(); p != nil {
			result = ""
			err = fmt.Errorf("tool %s panicked: %v", name, p)
		}
	}()

	return e.fn(ctx, args)
}

// DispatchTerminal dispatches a terminal tool and converts handler panics into
// errors, matching Dispatch's fail-safe behavior.
func (r *Registry) DispatchTerminal(ctx context.Context, name string, args json.RawMessage) (result TerminalOutcome, err error) {
	r.mu.RLock()
	e, ok := r.terminals[name]
	_, ordinary := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		if ordinary {
			return TerminalOutcome{}, fmt.Errorf("ordinary tool %s cannot be dispatched with DispatchTerminal", name)
		}
		return TerminalOutcome{}, fmt.Errorf("unknown terminal tool: %s", name)
	}

	defer func() {
		if p := recover(); p != nil {
			result = TerminalOutcome{}
			err = fmt.Errorf("terminal tool %s panicked: %v", name, p)
		}
	}()

	return e.fn(ctx, args)
}
