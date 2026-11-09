package shield

import (
	"encoding/json"
	"strings"
)

// Config is a service's shield settings. Empty fields mean the defaults (redact, warn).
type Config struct {
	Leaks     string `json:"leaks,omitempty"`
	Injection string `json:"injection,omitempty"`
}

// LeakMode returns the effective leak mode.
func (c Config) LeakMode() string {
	if c.Leaks == "" {
		return LeaksRedact
	}
	return c.Leaks
}

// InjectionMode returns the effective injection mode.
func (c Config) InjectionMode() string {
	if c.Injection == "" {
		return InjectionWarn
	}
	return c.Injection
}

// Validate checks the modes.
func (c Config) Validate() error {
	switch c.Leaks {
	case "", LeaksRedact, LeaksBlock, LeaksOff:
	default:
		return &modeError{"leaks", c.Leaks, "redact, block or off"}
	}
	switch c.Injection {
	case "", InjectionWarn, InjectionOff:
	default:
		return &modeError{"injection", c.Injection, "warn or off"}
	}
	return nil
}

type modeError struct{ field, got, want string }

func (e *modeError) Error() string {
	return "shield " + e.field + " mode " + `"` + e.got + `"` + " is not one of " + e.want
}

// Report is what inspecting one message found.
type Report struct {
	// Leaks are the leak rules that matched, one entry per match.
	Leaks []string
	// Blocked is set when the message was replaced because of a leak (block mode).
	Blocked bool
	// Injection are suspicious passages (warn mode never changes the message).
	Injection []Finding
}

// Empty reports whether nothing was found.
func (r Report) Empty() bool { return len(r.Leaks) == 0 && len(r.Injection) == 0 }

// MaxInjectionScan bounds the message size scanned for injection text, to keep CPU per call predictable.
const MaxInjectionScan = 8 << 20

// InspectMessage applies the shield to one JSON-RPC message from the app. It returns the message to send on
// (the input itself when unchanged).
func (c Config) InspectMessage(msg []byte, service string) ([]byte, bool, Report) {
	var rep Report
	out, changed := msg, false
	if c.LeakMode() != LeaksOff {
		red, hits := RedactJSON(msg)
		if len(hits) > 0 {
			rep.Leaks = hits
			if c.LeakMode() == LeaksBlock {
				out, rep.Blocked = blockMessage(msg, service, hits), true
			} else {
				out = red
			}
			changed = true
		}
	}
	if c.InjectionMode() != InjectionOff && len(msg) <= MaxInjectionScan {
		rep.Injection = ScanJSON(msg)
	}
	return out, changed, rep
}

// BlockedText is what the model reads instead of a result that held credentials.
func BlockedText(service string, hits []string) string {
	return service + ": TopRelay withheld this result because it contained credentials (" +
		strings.Join(Summary(hits), ", ") + "). Ask the owner to check what the app returns."
}

// blockMessage replaces a response's result: a tool result becomes a tool error the model can relay, anything
// else a JSON-RPC error. Messages without a result are redacted instead (they can't be withheld meaningfully).
func blockMessage(msg []byte, service string, hits []string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(msg, &obj) != nil {
		red, _ := RedactJSON(msg)
		return red
	}
	res, ok := obj["result"]
	if !ok {
		red, _ := RedactJSON(msg)
		return red
	}
	var shape struct {
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(res, &shape)
	text := BlockedText(service, hits)
	id := obj["id"]
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	var b []byte
	if len(shape.Content) > 0 && shape.Content[0] == '[' {
		b, _ = encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}}, "isError": true}})
	} else {
		b, _ = encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32001, "message": text}})
	}
	return b
}
