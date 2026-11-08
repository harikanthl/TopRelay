package mcpproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Transform rewrites one JSON-RPC message from the app. changed reports a rewrite; seen are the tool names in a
// tools/list result (for discovery).
type Transform func(msg []byte) (out []byte, changed bool, seen []string)

// ListTransform filters tools/list results for listIDs (FilterMessage).
func ListTransform(listIDs map[string]bool, allowed Allowed) Transform {
	return ListTransformLearn(listIDs, allowed, nil)
}

// ListTransformLearn is ListTransform that reports tool annotations to learn before filtering.
func ListTransformLearn(listIDs map[string]bool, allowed Allowed, learn func([]ToolHint)) Transform {
	if len(listIDs) == 0 {
		return nil
	}
	return func(m []byte) ([]byte, bool, []string) { return FilterMessageLearn(m, listIDs, allowed, learn) }
}

// Chain applies transforms in order; nil ones are skipped. The result is nil when all are.
func Chain(ts ...Transform) Transform {
	var live []Transform
	for _, t := range ts {
		if t != nil {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(m []byte) ([]byte, bool, []string) {
		changed := false
		var seen []string
		for _, t := range live {
			out, ch, s := t(m)
			m, changed, seen = out, changed || ch, append(seen, s...)
		}
		return m, changed, seen
	}
}

// TransformBody applies t to a JSON body (one message or a batch).
func TransformBody(body []byte, t Transform) ([]byte, []string) {
	if t == nil {
		return body, nil
	}
	tb := bytes.TrimSpace(body)
	if len(tb) > 0 && tb[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(tb, &arr) != nil {
			return body, nil
		}
		var all []string
		changed := false
		for i, m := range arr {
			nb, ch, seen := t(m)
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
	out, _, seen := t(body)
	return out, seen
}

// FileScheme is the URI scheme an app uses in a resource_link to offer a file through TopRelay.
const FileScheme = "toprelay-file:"

// Link is what a toprelay-file URI becomes: a signed, single-use public URL.
type Link struct {
	URL      string
	Name     string
	MimeType string
	Size     int64
}

// LinkFunc turns a toprelay-file URI into a public link.
type LinkFunc func(uri string) (Link, error)

// LinkTransform rewrites resource_link content items whose uri is a toprelay-file URI, in tools/call results for
// callIDs. An item that can't be shared becomes a text item saying why, so the model can tell the user.
func LinkTransform(callIDs map[string]bool, links LinkFunc) Transform {
	if len(callIDs) == 0 || links == nil {
		return nil
	}
	return func(msg []byte) ([]byte, bool, []string) {
		if !bytes.Contains(msg, []byte(FileScheme)) {
			return msg, false, nil
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(msg, &obj) != nil || !callIDs[IDKey(obj["id"])] {
			return msg, false, nil
		}
		var res map[string]json.RawMessage
		if json.Unmarshal(obj["result"], &res) != nil {
			return msg, false, nil
		}
		// Items stay raw; only a rewritten one is decoded, with UseNumber, so no number anywhere loses precision.
		var content []json.RawMessage
		if json.Unmarshal(res["content"], &content) != nil {
			return msg, false, nil
		}
		changed := false
		for i, raw := range content {
			if !bytes.Contains(raw, []byte(FileScheme)) {
				continue
			}
			var item map[string]any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if dec.Decode(&item) != nil {
				continue
			}
			uri, _ := item["uri"].(string)
			if item["type"] != "resource_link" || !strings.HasPrefix(uri, FileScheme) {
				continue
			}
			l, err := links(uri)
			if err != nil {
				item = map[string]any{"type": "text", "text": fmt.Sprintf("(The app offered a file that TopRelay couldn't share: %v.)", err)}
			} else {
				item["uri"] = l.URL
				if _, ok := item["name"].(string); !ok && l.Name != "" {
					item["name"] = l.Name
				}
				if _, ok := item["mimeType"].(string); !ok && l.MimeType != "" {
					item["mimeType"] = l.MimeType
				}
				item["size"] = l.Size
			}
			nb, err := marshalNoEscape(item)
			if err != nil {
				continue
			}
			content[i], changed = nb, true
		}
		if !changed {
			return msg, false, nil
		}
		cb, err := marshalNoEscape(content)
		if err != nil {
			return msg, false, nil
		}
		res["content"] = cb
		rb, err := marshalNoEscape(res)
		if err != nil {
			return msg, false, nil
		}
		obj["result"] = rb
		out, err := marshalNoEscape(obj)
		if err != nil {
			return msg, false, nil
		}
		return out, true, nil
	}
}
