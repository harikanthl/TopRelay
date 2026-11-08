package tunnel

import (
	"net/http"
	"slices"
	"strings"
)

// Frame limits from protocol.md.
const (
	MaxFrame       = 32 << 20
	MaxChunk       = 1 << 20
	MergeBytes     = 64 << 10
	WholeMaxBytes  = 1 << 20
	DefaultMaxBody = 20 << 20
)

// inFrame is any relay → agent frame.
type inFrame struct {
	V       int               `json:"v"`
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Tools   *[]string         `json:"tools"`
	Caps    []string          `json:"caps"`
	MaxBody int64             `json:"maxBody"`
	// Internal marks a request the agent answers itself ("file": a download), never the app.
	Internal string `json:"internal"`
}

type helloFrame struct {
	V     int       `json:"v"`
	Type  string    `json:"type"`
	Agent string    `json:"agent"`
	Caps  []string  `json:"caps"`
	Kind  string    `json:"kind"`
	Tools *[]string `json:"tools"`
}

type responseFrame struct {
	V       int               `json:"v"`
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
	Stream  bool              `json:"stream,omitempty"`
}

type chunkFrame struct {
	V    int    `json:"v"`
	Type string `json:"type"`
	ID   string `json:"id"`
	Data string `json:"data"`
}

type endFrame struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}

// responseHeaders is the relay's response header allow-list; the agent sends only these.
var responseHeaders = []string{"content-type", "cache-control", "mcp-session-id", "location", "etag",
	"last-modified", "content-disposition", "retry-after", "mcp-protocol-version"}

func filterResponseHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if slices.Contains(responseHeaders, lk) && len(v) > 0 {
			out[lk] = strings.Join(v, ", ")
		}
	}
	return out
}
