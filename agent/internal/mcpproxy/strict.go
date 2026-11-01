package mcpproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Go's encoding/json matches object keys case-insensitively (with Unicode folding, so "paramſ" matches "params")
// and lets the last duplicate win, while MCP servers in JS, Python and go-sdk match keys exactly. A body such as
// {"params":{"name":"delete","Name":"search"}} would then name one tool to the guard and another to the app.
// checkStrict rejects every body where the two readings could differ, so plain encoding/json is safe afterwards.

// messageKeys are the JSON-RPC fields the guard reads from a message; paramsKeys the ones it reads from params.
var (
	messageKeys = []string{"id", "method", "params", "result", "error"}
	paramsKeys  = []string{"name"}
)

// checkStrict validates body as exactly one JSON value (no BOM, no trailing data) with no duplicate keys anywhere,
// and with no key at the message or params level that case-folds to a field the guard reads without being it.
func checkStrict(body []byte) error {
	if bytes.HasPrefix(body, []byte("\xef\xbb\xbf")) {
		return errors.New("byte order mark before the JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('['):
		for dec.More() {
			if err := walkMessage(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // ]
			return err
		}
	case json.Delim('{'):
		if err := walkObject(dec, messageKeys, 0); err != nil {
			return err
		}
	default:
		return errors.New("not a JSON-RPC message or batch")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

// walkMessage reads one batch element, which must be an object.
func walkMessage(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("batch element is not an object")
	}
	return walkObject(dec, messageKeys, 0)
}

// walkObject reads the rest of an object whose '{' was consumed. guarded are the exact keys checked for
// case-folded look-alikes at this level (nil: none). depth 0 is a message, 1 its params.
func walkObject(dec *json.Decoder, guarded []string, depth int) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("object key is not a string")
		}
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		for _, g := range guarded {
			if key != g && strings.EqualFold(key, g) {
				return fmt.Errorf("key %q is ambiguous with %q", key, g)
			}
		}
		var next []string
		if depth == 0 && key == "params" {
			next = paramsKeys
		}
		if err := walkValue(dec, next, depth+1); err != nil {
			return err
		}
	}
	_, err := dec.Token() // }
	return err
}

func walkValue(dec *json.Decoder, guarded []string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return walkObject(dec, guarded, depth)
	case json.Delim('['):
		for dec.More() {
			if err := walkValue(dec, nil, depth+1); err != nil {
				return err
			}
		}
		_, err := dec.Token() // ]
		return err
	}
	return nil
}
