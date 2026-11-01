// tinyserver is a small stdio MCP server used by the stdio bridge tests. It has a read-only "echo" tool, a
// destructive "wipe" tool, an "add_tool" tool that registers "late" at runtime (to test list_changed), a
// "crash" tool that exits the process (to test restarts), one resource and one prompt.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "tinyserver", Version: "1.0.0"}, &mcp.ServerOptions{Instructions: "tiny test server"})
	obj := json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`)
	s.AddTool(&mcp.Tool{Name: "echo", Description: "Echo a message", InputSchema: obj, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in struct {
				Msg string `json:"msg"`
			}
			json.Unmarshal(r.Params.Arguments, &in)
			return text("echo: " + in.Msg + " pid=" + itoa(os.Getpid())), nil
		})
	s.AddTool(&mcp.Tool{Name: "wipe", Description: "Delete everything", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return text("wiped"), nil
		})
	s.AddTool(&mcp.Tool{Name: "add_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			s.AddTool(&mcp.Tool{Name: "late", InputSchema: json.RawMessage(`{"type":"object"}`), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
				func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return text("late ran"), nil
				})
			return text("added"), nil
		})
	s.AddTool(&mcp.Tool{Name: "crash", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			os.Exit(3)
			return nil, nil
		})
	s.AddResource(&mcp.Resource{URI: "tiny://readme", Name: "readme", MIMEType: "text/plain"},
		func(ctx context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "tiny://readme", MIMEType: "text/plain", Text: "hello from tiny"}}}, nil
		})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Description: "Say hi"},
		func(ctx context.Context, r *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi " + os.Getenv("TINY_GREETING")}}}}, nil
		})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Print(err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
