package calllog

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
	"time"
)

func signedLog(t *testing.T) (*Log, string, ed25519.PrivateKey) {
	t.Helper()
	path := Path(t.TempDir(), "notes")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	l.SetSigner("notes", key)
	return l, path, key
}

func pubOf(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }

func TestVerifyIntact(t *testing.T) {
	l, path, key := signedLog(t)
	for i := range 250 {
		l.Write(Record{Time: time.Now().UTC(), Status: 200 + i%3, Tool: "search"})
	}
	l.Close()
	rep, err := Verify(path, "notes", pubOf(key))
	if err != nil || !rep.OK {
		t.Fatalf("%+v %v", rep, err)
	}
	// 250 calls, a checkpoint after every 100 lines, and one on close.
	if rep.Calls != 250 || rep.Checkpoints != 3 || rep.Unsigned != 0 || rep.SignedThrough != rep.LastSeq || rep.FirstSeq != 1 {
		t.Fatalf("%+v", rep)
	}
	// A reopened log continues the chain.
	l2, _ := Open(path)
	l2.SetSigner("notes", key)
	l2.Write(Record{Time: time.Now().UTC(), Status: 200})
	l2.Close()
	if rep, _ := Verify(path, "notes", pubOf(key)); !rep.OK || rep.Calls != 251 {
		t.Fatalf("after reopen %+v", rep)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	cases := map[string]func(lines []string) []string{
		"edit": func(ls []string) []string {
			ls[3] = strings.Replace(ls[3], `"status":200`, `"status":204`, 1)
			return ls
		},
		"delete": func(ls []string) []string { return append(ls[:4], ls[5:]...) },
		"reorder": func(ls []string) []string {
			ls[2], ls[3] = ls[3], ls[2]
			return ls
		},
		"insert": func(ls []string) []string {
			return append(ls[:2], append([]string{`{"time":"2026-01-01T00:00:00Z","status":200}`}, ls[2:]...)...)
		},
		"forged checkpoint": func(ls []string) []string {
			last := len(ls) - 1
			ls[last] = strings.Replace(ls[last], `"sig":"`, `"sig":"A`, 1)
			return ls
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			l, path, key := signedLog(t)
			for range 8 {
				l.Write(Record{Time: time.Now().UTC(), Status: 200})
			}
			l.Close()
			b, _ := os.ReadFile(path)
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			os.WriteFile(path, []byte(strings.Join(mutate(lines), "\n")+"\n"), 0o600)
			rep, err := Verify(path, "notes", pubOf(key))
			if err != nil || rep.OK || len(rep.Problems) == 0 {
				t.Fatalf("not detected: %+v %v", rep, err)
			}
		})
	}
}

func TestVerifyWrongKeyAndNoKey(t *testing.T) {
	l, path, _ := signedLog(t)
	l.Write(Record{Time: time.Now().UTC(), Status: 200})
	l.Close()
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	if rep, _ := Verify(path, "notes", pubOf(other)); rep.OK || !strings.Contains(rep.Problems[0].What, "another key") {
		t.Fatalf("%+v", rep)
	}
	if rep, _ := Verify(path, "notes", nil); rep.OK || rep.Calls != 1 {
		t.Fatalf("%+v", rep)
	}
	// A checkpoint for one service doesn't verify for another.
	l2, path2, key := signedLog(t)
	l2.Write(Record{Time: time.Now().UTC(), Status: 200})
	l2.Close()
	if rep, _ := Verify(path2, "other", pubOf(key)); rep.OK {
		t.Fatal("signature not bound to the service")
	}
}

func TestVerifyAcrossRotationAndPrune(t *testing.T) {
	l, path, key := signedLog(t)
	l.maxSize = 2000
	old := time.Now().Add(-40 * 24 * time.Hour).UTC()
	for range 10 {
		l.Write(Record{Time: old, Status: 200})
	}
	for range 10 {
		l.Write(Record{Time: time.Now().UTC(), Status: 201})
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal("expected a rotation")
	}
	if rep, _ := Verify(path, "notes", pubOf(key)); !rep.OK {
		t.Fatalf("rotated %+v", rep)
	}
	n, err := l.Prune(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil || n == 0 {
		t.Fatal(n, err)
	}
	l.Write(Record{Time: time.Now().UTC(), Status: 202})
	l.Close()
	rep, err := Verify(path, "notes", pubOf(key))
	if err != nil || !rep.OK || rep.Prunes != 1 || rep.Pruned != n || rep.FirstSeq <= 1 {
		t.Fatalf("after prune %+v %v", rep, err)
	}
}

func TestVerifyLegacyPrefix(t *testing.T) {
	path := Path(t.TempDir(), "notes")
	os.MkdirAll(strings.TrimSuffix(path, "/notes.jsonl"), 0o700)
	os.WriteFile(path, []byte(`{"time":"2026-01-01T00:00:00Z","status":200}`+"\n"), 0o600)
	l, _ := Open(path)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	l.SetSigner("notes", key)
	l.Write(Record{Time: time.Now().UTC(), Status: 200})
	l.Close()
	rep, _ := Verify(path, "notes", pubOf(key))
	if !rep.OK || rep.Legacy != 1 || rep.Calls != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestPartialLastLineIsEnded(t *testing.T) {
	path := Path(t.TempDir(), "notes")
	l, _ := Open(path)
	l.Write(Record{Time: time.Now().UTC(), Status: 200})
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"time":"2026`)
	f.Close()
	l, _ = Open(path)
	l.Write(Record{Time: time.Now().UTC(), Status: 201})
	l.Close()
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"status":201`) {
		t.Fatalf("%q", lines)
	}
}
