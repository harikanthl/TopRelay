package mcpproxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerTransport adds fixed headers to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

// HTTPClientWithHeaders returns a client that adds headers to every request (nil base: default transport).
func HTTPClientWithHeaders(base http.RoundTripper, headers map[string]string) *http.Client {
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{Transport: headerTransport{base: base, headers: headers}}
}

// Connect opens an MCP client session to a Streamable HTTP endpoint.
func Connect(ctx context.Context, endpoint string, client *http.Client) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "toprelay", Version: "0.1.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	t := &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: 1}
	cs, err := c.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the MCP server at %s: %w", endpoint, err)
	}
	return cs, nil
}

// ListAllTools pages through tools/list.
func ListAllTools(ctx context.Context, cs *mcp.ClientSession) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		out = append(out, t)
	}
	return out, nil
}

// ListTools connects to an MCP URL, lists its tools and disconnects. headers are added to every request.
func ListTools(ctx context.Context, endpoint string, headers map[string]string) ([]*mcp.Tool, error) {
	cs, err := Connect(ctx, endpoint, HTTPClientWithHeaders(nil, headers))
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	return ListAllTools(ctx, cs)
}

// ToolNames returns the names of tools.
func ToolNames(tools []*mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
