package keys

import (
	"testing"
	"time"
)

// The protocol.md file-URL vector; worker/test/keys.test.ts checks the same one.
func TestFileVector(t *testing.T) {
	const root = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8"
	u, err := FileURL("https://toprelay.example.workers.dev/", root, "notes", 1, "00112233445566778899aabbccddeeff", time.Unix(1793500900, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(u)
	want := "https://toprelay.example.workers.dev/notes/files/00112233445566778899aabbccddeeff?e=1793500900&s=" + fileVectorSig
	if u != want {
		t.Fatalf("got  %s\nwant %s", u, want)
	}
	if _, err := FileURL("https://x", root, "notes", 1, "XYZ", time.Now()); err == nil {
		t.Fatal("bad id accepted")
	}
	id, _ := NewFileID()
	if !FileIDRE.MatchString(id) {
		t.Fatal(id)
	}
}

const fileVectorSig = "tBkr0_7eqyIbMb0KVCASzymzU0--l_J_ECGXjWMnLlY"
