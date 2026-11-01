package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

func exercise(t *testing.T, s Store) {
	t.Helper()
	if _, err := s.Get("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Set("x", "value-1234"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get("x"); err != nil || v != "value-1234" {
		t.Fatalf("get = %q %v", v, err)
	}
	if err := s.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("x"); err != nil {
		t.Fatalf("deleting a missing secret must succeed: %v", err)
	}
	if _, err := Require(s, "x", "run login"); err == nil {
		t.Fatal("Require")
	}
}

func TestKeyringMock(t *testing.T) {
	keyring.MockInit()
	exercise(t, Keyring{})
}

func TestFile(t *testing.T) {
	dir := t.TempDir()
	s := Open(func(k string) string {
		if k == "TOPRELAY_KEYRING" {
			return "file"
		}
		return ""
	}, dir)
	exercise(t, s)
	s.Set("y", "abcdefgh")
	st, err := os.Stat(filepath.Join(dir, "secrets.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st, err)
	}
	if _, ok := Open(func(string) string { return "" }, dir).(Keyring); !ok {
		t.Fatal("default store must be the keyring")
	}
	if UpstreamHeader("notes") != "upstream-header/notes" {
		t.Fatal("UpstreamHeader")
	}
}
