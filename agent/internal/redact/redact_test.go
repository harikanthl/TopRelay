package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Add("supersecrettoken", "short")
	if got := String("token=supersecrettoken short"); got != "token=[REDACTED] short" {
		t.Fatalf("got %q", got)
	}
	got := Log("auth tra1.abcDEF_123-x and trk1.eyJp.c2ln end")
	if strings.Contains(got, "tra1.") || strings.Contains(got, "trk1.") {
		t.Fatalf("keys not masked: %q", got)
	}
	var buf bytes.Buffer
	w := Writer{W: &buf, Keys: true}
	n, err := w.Write([]byte("x supersecrettoken trk1.a.b\n"))
	if err != nil || n != len("x supersecrettoken trk1.a.b\n") {
		t.Fatal(n, err)
	}
	if buf.String() != "x [REDACTED] [REDACTED]\n" {
		t.Fatalf("writer = %q", buf.String())
	}
	buf.Reset()
	Writer{W: &buf}.Write([]byte("trk1.a.b"))
	if buf.String() != "trk1.a.b" {
		t.Fatal("non-key writer must keep bearer keys (printed once by `key`)")
	}
}
