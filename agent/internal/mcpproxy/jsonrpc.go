// Package mcpproxy is the agent's MCP-aware layer: it refuses disallowed tool calls without touching the app and
// filters tools/list results (JSON and SSE), in front of any Streamable HTTP MCP endpoint.
package mcpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/harikanthl/toprelay/agent/internal/pins"
)

// Message is the part of a JSON-RPC message the proxy looks at.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// ToolName returns params.name for a tools/call ("" when it is missing or not a string).
func (m Message) ToolName() string {
	var p struct {
		Name string `json:"name"`
	}
	if len(m.Params) > 0 {
		if json.Unmarshal(m.Params, &p) != nil {
			return ""
		}
	}
	return p.Name
}

// IDKey is a canonical string for an id, for matching responses to requests.
func IDKey(id json.RawMessage) string {
	var v any
	if len(id) == 0 || json.Unmarshal(id, &v) != nil || v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// MissingToolName stands for the tool of a tools/call without a string params.name; such calls are always refused.
const MissingToolName = "(missing name)"

// Parse reads a JSON-RPC body: one message or a batch. It rejects bodies an exact-key JSON reader could read
// differently (duplicate or case-variant keys, a BOM, trailing data); see checkStrict.
func Parse(body []byte) (msgs []Message, batch bool, err error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil, false, fmt.Errorf("empty body")
	}
	if err := checkStrict(body); err != nil {
		return nil, t[0] == '[', err
	}
	if t[0] == '[' {
		if err := json.Unmarshal(t, &msgs); err != nil {
			return nil, true, err
		}
		return msgs, true, nil
	}
	var m Message
	if err := json.Unmarshal(t, &m); err != nil {
		return nil, false, err
	}
	return []Message{m}, false, nil
}

// RefusalText is the tool-result text protocol.md prescribes for a disallowed tool.
func RefusalText(tool, service string) string {
	return fmt.Sprintf("The tool %q is not available on %s. Its owner hasn't allowed it.", tool, service)
}

// HeldText is the tool-result text for a tool whose definition changed since its owner approved it.
func HeldText(tool, service string) string {
	return fmt.Sprintf("The tool %q on %s changed since its owner approved it, so TopRelay is holding it until they review the change.", tool, service)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

// Refusal is the JSON-RPC response refusing a tool call.
func Refusal(id json.RawMessage, tool, service string) []byte {
	b, _ := json.Marshal(refusalResponse(id, tool, service, nil))
	return b
}

func refusalResponse(id json.RawMessage, tool, service string, held func(string) bool) response {
	text := RefusalText(tool, service)
	if held != nil && held(tool) {
		text = HeldText(tool, service)
	}
	return response{JSONRPC: "2.0", ID: idOrNull(id), Result: toolResult{
		Content: []textContent{{Type: "text", Text: text}}, IsError: true}}
}

// Allowed decides whether a tool may be listed and called.
type Allowed func(tool string) bool

// FilterResult filters a tools/list result object, keeping the order of the remaining tools and every other
// field (ttlMs, cacheScope, nextCursor, _meta …). It returns the filtered result and every tool name seen.
func FilterResult(result json.RawMessage, allowed Allowed) (json.RawMessage, []string, error) {
	return FilterResultLearn(result, allowed, nil)
}

// ToolHint is a listed tool's annotations. Destructive follows MCP's default: true unless the tool is read-only
// or says destructiveHint: false.
type ToolHint struct {
	Name        string
	ReadOnly    bool
	Destructive bool
	// Fingerprint is the definition's SHA-256 (package pins); Description is what the model reads.
	Fingerprint string
	Description string
}

// FilterResultLearn is FilterResult that first reports every listed tool's annotations to learn, so the policy
// can use them for this very listing.
func FilterResultLearn(result json.RawMessage, allowed Allowed, learn func([]ToolHint)) (json.RawMessage, []string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		return nil, nil, err
	}
	raw, ok := obj["tools"]
	if !ok {
		return result, nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, nil, err
	}
	kept := make([]json.RawMessage, 0, len(tools))
	var seen []string
	names := make([]string, len(tools))
	var hints []ToolHint
	for i, t := range tools {
		var n struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Annotations *struct {
				ReadOnlyHint    bool  `json:"readOnlyHint"`
				DestructiveHint *bool `json:"destructiveHint"`
			} `json:"annotations"`
		}
		if json.Unmarshal(t, &n) != nil {
			continue
		}
		names[i] = n.Name
		h := ToolHint{Name: n.Name, Destructive: true, Description: n.Description}
		h.Fingerprint, _ = pins.Fingerprint(t)
		if a := n.Annotations; a != nil {
			h.ReadOnly = a.ReadOnlyHint
			if a.DestructiveHint != nil {
				h.Destructive = *a.DestructiveHint
			}
		}
		if h.ReadOnly {
			h.Destructive = false
		}
		hints = append(hints, h)
	}
	if learn != nil && len(hints) > 0 {
		learn(hints)
	}
	for i, t := range tools {
		if names[i] == "" {
			continue
		}
		seen = append(seen, names[i])
		if allowed(names[i]) {
			kept = append(kept, t)
		}
	}
	nb, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	obj["tools"] = nb
	out, err := marshalNoEscape(obj)
	return out, seen, err
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// FilterMessage filters one JSON-RPC message if it answers a tools/list id. It returns the (possibly unchanged)
// bytes, whether it changed, and the tool names seen.
func FilterMessage(msg []byte, listIDs map[string]bool, allowed Allowed) ([]byte, bool, []string) {
	return FilterMessageLearn(msg, listIDs, allowed, nil)
}

// FilterMessageLearn is FilterMessage with an annotations hook (FilterResultLearn).
func FilterMessageLearn(msg []byte, listIDs map[string]bool, allowed Allowed, learn func([]ToolHint)) ([]byte, bool, []string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(msg, &obj) != nil {
		return msg, false, nil
	}
	res, hasRes := obj["result"]
	if !hasRes || !listIDs[IDKey(obj["id"])] {
		return msg, false, nil
	}
	fr, seen, err := FilterResultLearn(res, allowed, learn)
	if err != nil {
		return msg, false, nil
	}
	obj["result"] = fr
	out, err := marshalNoEscape(obj)
	if err != nil {
		return msg, false, nil
	}
	return out, true, seen
}

// FilterBody filters a JSON response body (one message or a batch).
func FilterBody(body []byte, listIDs map[string]bool, allowed Allowed) ([]byte, []string) {
	return TransformBody(body, ListTransform(listIDs, allowed))
}

// Decision is the guard's verdict on a request body.
type Decision struct {
	// Refuse is set when the request must be answered locally; Body is the answer, with HTTP status Status.
	Refuse bool
	Status int
	Body   []byte
	// Invalid is set when the body isn't a JSON-RPC message the guard can read safely (answered with -32700).
	Invalid bool
	Tools   []string // refused tool names
	// Held is set when a refused tool is held for review (pins): its definition changed.
	Held bool
	// ListIDs are the ids of tools/list requests, for filtering the response.
	ListIDs map[string]bool
	// CallIDs are the ids of allowed tools/call requests, for rewriting file links in the response.
	CallIDs map[string]bool
	Msgs    []Message
}

// Reason is the call-log reason for a refusal.
func (d Decision) Reason() string {
	if d.Invalid {
		return ReasonInvalid
	}
	if d.Held {
		return ReasonToolChanged
	}
	return ReasonToolNotAllowed
}

// RefusedLabel is the RefusedHeader value for a refusal: the refused tools, or why the body was refused.
func (d Decision) RefusedLabel() string {
	if len(d.Tools) > 0 {
		return strings.Join(d.Tools, ",")
	}
	return "(invalid request)"
}

// Decide applies the allow-list to a request body. headerName is the Mcp-Name header when Mcp-Method is
// tools/call ("" otherwise).
func Decide(body []byte, headerName, service string, allowed Allowed) Decision {
	return DecideHeld(body, headerName, service, allowed, nil)
}

// DecideHeld is Decide that explains refusals of held tools (held reports them; nil: none) as held for review.
// allowed must already refuse held tools.
func DecideHeld(body []byte, headerName, service string, allowed Allowed, held func(string) bool) Decision {
	d := Decision{ListIDs: map[string]bool{}, CallIDs: map[string]bool{}, Status: http.StatusOK}
	msgs, batch, err := Parse(body)
	if err != nil {
		// Never forward what the guard can't read exactly as the app would.
		d.Refuse, d.Invalid, d.Status = true, true, http.StatusBadRequest
		d.Body, _ = json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: map[string]any{"code": -32700, "message": "Parse error: " + err.Error()}})
		return d
	}
	if len(msgs) == 0 {
		d.Refuse, d.Invalid, d.Status = true, true, http.StatusBadRequest
		d.Body, _ = json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: map[string]any{"code": -32600, "message": "Invalid Request: empty batch"}})
		return d
	}
	d.Msgs = msgs
	refused := make([]string, len(msgs))
	anyRefused := false
	for i, m := range msgs {
		switch m.Method {
		case "tools/call":
			name := m.ToolName()
			if name == "" {
				refused[i], anyRefused = MissingToolName, true
			} else if !allowed(name) {
				refused[i], anyRefused = name, true
			} else if !batch && headerName != "" && headerName != name && !allowed(headerName) {
				refused[i], anyRefused = headerName, true
			} else if k := IDKey(m.ID); k != "" {
				d.CallIDs[k] = true
			}
		case "tools/list":
			if k := IDKey(m.ID); k != "" {
				d.ListIDs[k] = true
			}
		}
	}
	if !anyRefused && headerName != "" && !allowed(headerName) {
		refused[0], anyRefused = headerName, true
	}
	if !anyRefused {
		return d
	}
	d.Refuse = true
	for _, r := range refused {
		if r != "" {
			d.Tools = append(d.Tools, r)
			d.Held = d.Held || (held != nil && held(r))
		}
	}
	if !batch {
		d.Body, _ = json.Marshal(refusalResponse(msgs[0].ID, refused[0], service, held))
		return d
	}
	// A batch with a refused call is answered whole, locally: nothing in it reaches the app.
	var out []response
	for i, m := range msgs {
		if len(m.ID) == 0 && m.Method != "" {
			continue // notifications get no response
		}
		if refused[i] != "" {
			out = append(out, refusalResponse(m.ID, refused[i], service, held))
			continue
		}
		out = append(out, response{JSONRPC: "2.0", ID: idOrNull(m.ID), Error: map[string]any{"code": -32600,
			"message": "Not sent: this batch also calls a tool that isn't available on " + service + ". Send the calls separately."}})
	}
	d.Body, _ = json.Marshal(out)
	return d
}

// Restrict combines a policy with an optional bearer key restriction.
func Restrict(policy Allowed, restriction []string, restricted bool) Allowed {
	if !restricted {
		return policy
	}
	return func(t string) bool { return policy(t) && slices.Contains(restriction, t) }
}
