// Package caller signs the X-TopRelay-Caller header the agent adds to every relayed request, so a local app can
// tell a request that really came through the relay from one any local process made up (docs/protocol.md
// "Agent → local app").
//
// Header (one line):
//
//	X-TopRelay-Caller: v1; client=<enc>; ts=<unix s>; nonce=<32 hex>; sig=<64 hex>
//
// sig is lowercase hex HMAC-SHA256, keyed with the bytes of the service's caller key (the trimmed content of its
// key file), over these fields joined with "\n":
//
//	"toprelay-caller-v1", METHOD, request-target, <enc>, <ts>, <nonce>, hex(SHA-256(body))
//
// request-target is the path and query exactly as on the HTTP request line (Go: r.RequestURI). <enc> is the
// OAuth client id or "key:<id>", percent-encoded (every byte outside A-Z a-z 0-9 - . _ ~ : / @ becomes %XX), and is
// signed in that encoded form.
package caller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Header is the header name. Go's canonical form is "X-Toprelay-Caller"; HTTP header names are case-insensitive.
const Header = "X-TopRelay-Caller"

// Version is the only header version.
const Version = "v1"

// MaxSkew is how far the timestamp may be from the verifier's clock.
const MaxSkew = 60 * time.Second

// MinKeyLen is the shortest caller key accepted (a generated key is 43 characters).
const MinKeyLen = 32

const sigContext = "toprelay-caller-v1"

// EncodeClient percent-encodes a client id for the header.
func EncodeClient(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', strings.IndexByte("-._~:/@", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// DecodeClient reverses EncodeClient.
func DecodeClient(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", errors.New("bad percent-encoding in client")
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", errors.New("bad percent-encoding in client")
		}
		b.WriteByte(byte(v))
		i += 2
	}
	return b.String(), nil
}

// Message is the exact byte string that is signed.
func Message(method, target, encClient string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{sigContext, method, target, encClient, strconv.FormatInt(ts, 10), nonce, hex.EncodeToString(sum[:])}, "\n"))
}

func mac(key, msg []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

// Sign returns the header value for one request.
func Sign(key []byte, method, target, client string, ts int64, nonce string, body []byte) string {
	enc := EncodeClient(client)
	sig := mac(key, Message(method, target, enc, ts, nonce, body))
	return fmt.Sprintf("%s; client=%s; ts=%d; nonce=%s; sig=%s", Version, enc, ts, nonce, sig)
}

// NewNonce returns 16 random bytes as hex.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Caller is a verified header.
type Caller struct {
	Client string
	TS     int64
	Nonce  string
}

// Verification errors.
var (
	ErrMissing   = errors.New("no X-TopRelay-Caller header")
	ErrMalformed = errors.New("malformed X-TopRelay-Caller header")
	ErrStale     = errors.New("X-TopRelay-Caller timestamp is too old or in the future")
	ErrBadSig    = errors.New("X-TopRelay-Caller signature does not match")
)

// Parse splits a header value into its fields.
func Parse(value string) (map[string]string, error) {
	parts := strings.Split(value, ";")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) != Version {
		return nil, ErrMalformed
	}
	out := map[string]string{}
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || k == "" {
			return nil, ErrMalformed
		}
		if _, dup := out[k]; dup {
			return nil, ErrMalformed
		}
		out[k] = v
	}
	for _, k := range []string{"client", "ts", "nonce", "sig"} {
		if _, ok := out[k]; !ok {
			return nil, ErrMalformed
		}
	}
	return out, nil
}

// Verify checks a header value against the request it came with. It is the reference for the verifiers in
// MIGRATION.md.
func Verify(key []byte, value, method, target string, body []byte, now time.Time) (Caller, error) {
	if value == "" {
		return Caller{}, ErrMissing
	}
	f, err := Parse(value)
	if err != nil {
		return Caller{}, err
	}
	ts, err := strconv.ParseInt(f["ts"], 10, 64)
	if err != nil || strings.Trim(f["ts"], "0123456789") != "" {
		return Caller{}, ErrMalformed
	}
	d := now.Sub(time.Unix(ts, 0))
	if d > MaxSkew || d < -MaxSkew {
		return Caller{}, ErrStale
	}
	want := mac(key, Message(method, target, f["client"], ts, f["nonce"], body))
	if !hmac.Equal([]byte(want), []byte(f["sig"])) {
		return Caller{}, ErrBadSig
	}
	client, err := DecodeClient(f["client"])
	if err != nil {
		return Caller{}, ErrMalformed
	}
	return Caller{Client: client, TS: ts, Nonce: f["nonce"]}, nil
}

// DefaultKeyPath is where the agent keeps a service's caller key unless the app supplies its own file.
func DefaultKeyPath(configDir, service string) string {
	return filepath.Join(configDir, "caller-keys", service+".key")
}

// NewKey returns a fresh key: 32 random bytes, base64url without padding (43 characters).
func NewKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// EnsureKeyFile creates a key file (0600, directory 0700) if it doesn't exist. It reports whether it created one.
func EnsureKeyFile(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	k, err := NewKey()
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil // another process made it first
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(k + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return false, err
	}
	return true, f.Close()
}

// WriteKeyFile replaces a key file atomically with a new key (0600) and returns the key.
func WriteKeyFile(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	k, err := NewKey()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".key-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(k + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return k, os.Rename(name, path)
}
