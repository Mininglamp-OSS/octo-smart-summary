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
}

// Registry 线程安全地保存工具 schema + handler，支持注册/取 schema/分发。
type Registry struct {
	mu    sync.RWMutex
	tools map[string]entry
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]entry)}
}

func (r *Registry) Register(schema Tool, fn Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[schema.Function.Name] = entry{schema: schema, fn: fn}
}

func (r *Registry) Schemas() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e.schema)
	}
	return out
}

// Dispatch 按名分发。未知工具/handler panic 都转成错误返回，绝不中断回环。
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (result string, err error) {
	r.mu.RLock()
	e, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
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
