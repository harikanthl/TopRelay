// Package mcpproxy is the agent's MCP-aware layer: it refuses disallowed tool calls without touching the app and
// filters tools/list results (JSON and SSE), in front of any Streamable HTTP MCP endpoint.
package mcpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// Message is the part of a JSON-RPC message the proxy looks at.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// ToolName returns params.name for a tools/call.
func (m Message) ToolName() string {
	var p struct {
		Name string `json:"name"`
	}
	if len(m.Params) > 0 {
		_ = json.Unmarshal(m.Params, &p)
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

// Parse reads a JSON-RPC body: one message or a batch.
func Parse(body []byte) (msgs []Message, batch bool, err error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil, false, fmt.Errorf("empty body")
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
	b, _ := json.Marshal(refusalResponse(id, tool, service))
	return b
}

func refusalResponse(id json.RawMessage, tool, service string) response {
	return response{JSONRPC: "2.0", ID: idOrNull(id), Result: toolResult{
		Content: []textContent{{Type: "text", Text: RefusalText(tool, service)}}, IsError: true}}
}

// Allowed decides whether a tool may be listed and called.
type Allowed func(tool string) bool

// FilterResult filters a tools/list result object, keeping the order of the remaining tools and every other
// field (ttlMs, cacheScope, nextCursor, _meta …). It returns the filtered result and every tool name seen.
func FilterResult(result json.RawMessage, allowed Allowed) (json.RawMessage, []string, error) {
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
	for _, t := range tools {
		var n struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &n) != nil {
			continue
		}
		seen = append(seen, n.Name)
		if allowed(n.Name) {
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
	var obj map[string]json.RawMessage
	if json.Unmarshal(msg, &obj) != nil {
		return msg, false, nil
	}
	res, hasRes := obj["result"]
	if !hasRes || !listIDs[IDKey(obj["id"])] {
		return msg, false, nil
	}
	fr, seen, err := FilterResult(res, allowed)
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
	t := bytes.TrimSpace(body)
	if len(t) > 0 && t[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(t, &arr) != nil {
			return body, nil
		}
		var all []string
		changed := false
		for i, m := range arr {
			nb, ch, seen := FilterMessage(m, listIDs, allowed)
			arr[i] = nb
			changed = changed || ch
			all = append(all, seen...)
		}
		if !changed {
			return body, all
		}
		out, err := marshalNoEscape(arr)
		if err != nil {
			return body, all
		}
		return out, all
	}
	out, _, seen := FilterMessage(body, listIDs, allowed)
	return out, seen
}

// Decision is the guard's verdict on a request body.
type Decision struct {
	// Refuse is set when the request must be answered locally; Body is the answer.
	Refuse bool
	Body   []byte
	Tools  []string // refused tool names
	// ListIDs are the ids of tools/list requests, for filtering the response.
	ListIDs map[string]bool
	Msgs    []Message
}

// Decide applies the allow-list to a request body. headerName is the Mcp-Name header when Mcp-Method is
// tools/call ("" otherwise).
func Decide(body []byte, headerName, service string, allowed Allowed) Decision {
	d := Decision{ListIDs: map[string]bool{}}
	msgs, batch, err := Parse(body)
	if err != nil {
		if headerName != "" && !allowed(headerName) {
			d.Refuse, d.Tools = true, []string{headerName}
			d.Body = Refusal(nil, headerName, service)
		}
		return d
	}
	d.Msgs = msgs
	refused := make([]string, len(msgs))
	anyRefused := false
	for i, m := range msgs {
		switch m.Method {
		case "tools/call":
			name := m.ToolName()
			if !allowed(name) {
				refused[i], anyRefused = name, true
			} else if !batch && headerName != "" && headerName != name && !allowed(headerName) {
				refused[i], anyRefused = headerName, true
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
		}
	}
	if !batch {
		d.Body = Refusal(msgs[0].ID, refused[0], service)
		return d
	}
	// A batch with a refused call is answered whole, locally: nothing in it reaches the app.
	var out []response
	for i, m := range msgs {
		if len(m.ID) == 0 && m.Method != "" {
			continue // notifications get no response
		}
		if refused[i] != "" {
			out = append(out, refusalResponse(m.ID, refused[i], service))
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
