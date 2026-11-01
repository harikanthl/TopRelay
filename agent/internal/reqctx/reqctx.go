// Package reqctx carries per-request facts from the tunnel to the local handlers.
package reqctx

import "context"

type toolsKey struct{}

// WithTools records the bearer key's tool restriction from the request frame.
func WithTools(ctx context.Context, tools []string) context.Context {
	if tools == nil {
		tools = []string{}
	}
	return context.WithValue(ctx, toolsKey{}, tools)
}

// Tools returns the tool restriction and whether there is one.
func Tools(ctx context.Context) ([]string, bool) {
	t, ok := ctx.Value(toolsKey{}).([]string)
	return t, ok
}
