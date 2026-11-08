package mcpproxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLinkTransform(t *testing.T) {
	links := func(uri string) (Link, error) {
		if strings.HasSuffix(uri, "bad") {
			return Link{}, errors.New("nope")
		}
		return Link{URL: "https://relay/f/1?e=1&s=x", Name: "a.pdf", MimeType: "application/pdf", Size: 3}, nil
	}
	xf := LinkTransform(map[string]bool{"7": true}, links)
	msg := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"hi"},` +
		`{"type":"resource_link","uri":"toprelay-file:///a.pdf","name":"Mine.pdf"},{"type":"resource_link","uri":"toprelay-file:///bad"}],"isError":false}}`
	out, changed, _ := xf([]byte(msg))
	s := string(out)
	if !changed || !strings.Contains(s, `"uri":"https://relay/f/1?e=1&s=x"`) || !strings.Contains(s, `"name":"Mine.pdf"`) ||
		!strings.Contains(s, `"mimeType":"application/pdf"`) || !strings.Contains(s, `"size":3`) || !strings.Contains(s, "couldn't share: nope") ||
		!strings.Contains(s, `"isError":false`) || strings.Contains(s, "\\u0026") {
		t.Fatalf("%s", s)
	}
	// Numbers keep their exact value, in rewritten and untouched items alike.
	big := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"x","_meta":{"rowId":9007199254740993}},` +
		`{"type":"resource_link","uri":"toprelay-file:///a.pdf","_meta":{"v":12345678901234567890}}]}}`
	out, _, _ = xf([]byte(big))
	if !strings.Contains(string(out), "9007199254740993") || !strings.Contains(string(out), "12345678901234567890") {
		t.Fatalf("precision lost: %s", out)
	}
	// Another id, a request, or no toprelay-file: untouched, byte for byte.
	for _, m := range []string{strings.Replace(msg, `"id":7`, `"id":8`, 1), `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"resource_link","uri":"https://x"}]}}`} {
		if out, ch, _ := xf([]byte(m)); ch || string(out) != m {
			t.Errorf("changed %s", m)
		}
	}
	// SSE: the event is rewritten, others pass through.
	in := "event: message\ndata: " + msg + "\n\n: keepalive\n\n"
	var w bytes.Buffer
	if err := TransformSSE(&w, strings.NewReader(in), xf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.String(), "https://relay/f/1") || !strings.HasSuffix(w.String(), ": keepalive\n\n") || strings.Contains(w.String(), "toprelay-file:///a.pdf") {
		t.Fatalf("%q", w.String())
	}
	if LinkTransform(nil, links) != nil || LinkTransform(map[string]bool{"1": true}, nil) != nil {
		t.Fatal("transform without ids or minter")
	}
}
