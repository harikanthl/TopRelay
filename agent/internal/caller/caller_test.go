package caller

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The protocol.md test vector, computed independently (Python hmac/hashlib).
const (
	vecKey    = "tr-caller-test-key-0123456789abcdefghijkl"
	vecBody   = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	vecClient = "https://claude.ai/oauth/mcp client;x"
	vecTS     = 1793500000
	vecNonce  = "000102030405060708090a0b0c0d0e0f"
	vecHeader = "v1; client=https://claude.ai/oauth/mcp%20client%3Bx; ts=1793500000; nonce=000102030405060708090a0b0c0d0e0f; sig=d42479b952efe9b90dfb5336b0cf2074c0e6fe4b578a9feb3b5c68fef7058f13"
)

func TestVector(t *testing.T) {
	got := Sign([]byte(vecKey), "POST", "/mcp?x=1", vecClient, vecTS, vecNonce, []byte(vecBody))
	if got != vecHeader {
		t.Fatalf("got  %s\nwant %s", got, vecHeader)
	}
	c, err := Verify([]byte(vecKey), vecHeader, "POST", "/mcp?x=1", []byte(vecBody), time.Unix(vecTS+30, 0))
	if err != nil || c.Client != vecClient || c.TS != vecTS || c.Nonce != vecNonce {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestVerifyRejects(t *testing.T) {
	key := []byte(vecKey)
	now := time.Unix(vecTS, 0)
	ok := func(method, target, body string, at time.Time, k []byte, h string) error {
		_, err := Verify(k, h, method, target, []byte(body), at)
		return err
	}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"method", ok("GET", "/mcp?x=1", vecBody, now, key, vecHeader), ErrBadSig},
		{"target", ok("POST", "/mcp", vecBody, now, key, vecHeader), ErrBadSig},
		{"body", ok("POST", "/mcp?x=1", vecBody+" ", now, key, vecHeader), ErrBadSig},
		{"key", ok("POST", "/mcp?x=1", vecBody, now, []byte(vecKey+"x"), vecHeader), ErrBadSig},
		{"client", ok("POST", "/mcp?x=1", vecBody, now, key, strings.Replace(vecHeader, "client=https", "client=http", 1)), ErrBadSig},
		{"nonce", ok("POST", "/mcp?x=1", vecBody, now, key, strings.Replace(vecHeader, "nonce=00", "nonce=01", 1)), ErrBadSig},
		{"ts edited", ok("POST", "/mcp?x=1", vecBody, now, key, strings.Replace(vecHeader, "ts=1793500000", "ts=1793500001", 1)), ErrBadSig},
		{"old", ok("POST", "/mcp?x=1", vecBody, now.Add(61*time.Second), key, vecHeader), ErrStale},
		{"future", ok("POST", "/mcp?x=1", vecBody, now.Add(-61*time.Second), key, vecHeader), ErrStale},
		{"missing", ok("POST", "/mcp?x=1", vecBody, now, key, ""), ErrMissing},
		{"version", ok("POST", "/mcp?x=1", vecBody, now, key, "v2"+vecHeader[2:]), ErrMalformed},
		{"duplicate", ok("POST", "/mcp?x=1", vecBody, now, key, vecHeader+"; ts=1"), ErrMalformed},
		{"no sig", ok("POST", "/mcp?x=1", vecBody, now, key, vecHeader[:strings.Index(vecHeader, "; sig=")]), ErrMalformed},
		{"garbage", ok("POST", "/mcp?x=1", vecBody, now, key, "v1; nonsense"), ErrMalformed},
	}
	for _, c := range cases {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, c.err, c.want)
		}
	}
}

func TestClientEncoding(t *testing.T) {
	for _, s := range []string{"", "key:0123456789abcdef", "https://chatgpt.com/x?y=1&z", "a b;c=d%e\n", "ünïcode"} {
		enc := EncodeClient(s)
		if strings.ContainsAny(enc, " ;=\n") {
			t.Errorf("%q encodes to %q", s, enc)
		}
		dec, err := DecodeClient(enc)
		if err != nil || dec != s {
			t.Errorf("%q → %q → %q %v", s, enc, dec, err)
		}
	}
	for _, bad := range []string{"%", "%4", "%zz"} {
		if _, err := DecodeClient(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestKeyFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caller-keys", "notes.key")
	made, err := EnsureKeyFile(path)
	if err != nil || !made {
		t.Fatal(made, err)
	}
	st, _ := os.Stat(path)
	dst, _ := os.Stat(filepath.Dir(path))
	if st.Mode().Perm() != 0o600 || dst.Mode().Perm() != 0o700 {
		t.Fatalf("modes %v %v", st.Mode(), dst.Mode())
	}
	b1, _ := os.ReadFile(path)
	if k := strings.TrimSpace(string(b1)); len(k) != 43 {
		t.Fatalf("key %q", k)
	}
	if made, err := EnsureKeyFile(path); err != nil || made {
		t.Fatal("second ensure", made, err)
	}
	b2, _ := os.ReadFile(path)
	if string(b1) != string(b2) {
		t.Fatal("ensure replaced an existing key")
	}
	k, err := WriteKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := os.ReadFile(path)
	st, _ = os.Stat(path)
	if strings.TrimSpace(string(b3)) != k || string(b3) == string(b1) || st.Mode().Perm() != 0o600 {
		t.Fatal("rotate")
	}
}
