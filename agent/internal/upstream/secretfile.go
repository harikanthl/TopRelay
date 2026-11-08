package upstream

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxSecretFile is the largest secret file read.
const MaxSecretFile = 8 << 10

// ErrSecretFile wraps every problem reading a secret file, so callers can tell the remote caller "the app's
// credential is unavailable" without the path.
var ErrSecretFile = errors.New("secret file")

// ReadSecretFile reads a secret from a file the app owns: an absolute path to a regular file, owned by this user,
// not readable or writable by group or others, at most 8 KiB. The value is the content with surrounding
// whitespace removed; it must be non-empty and safe as an HTTP header value. It is read fresh on every call, so the
// app can rotate it at any time (write a new file and rename it over the old one).
func ReadSecretFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w %s: the path must be absolute", ErrSecretFile, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSecretFile, err)
	}
	defer f.Close()
	// Checks run on the open file, so a swap between check and read can't slip another file in.
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSecretFile, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%w %s: not a regular file", ErrSecretFile, path)
	}
	if err := checkOwner(st); err != nil {
		return "", fmt.Errorf("%w %s: %v", ErrSecretFile, path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w %s: other users can access it (mode %04o); run chmod 600 %s", ErrSecretFile, path, st.Mode().Perm(), path)
	}
	if st.Size() > MaxSecretFile {
		return "", fmt.Errorf("%w %s: larger than %d bytes", ErrSecretFile, path, MaxSecretFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxSecretFile+1))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSecretFile, err)
	}
	if len(b) > MaxSecretFile {
		return "", fmt.Errorf("%w %s: larger than %d bytes", ErrSecretFile, path, MaxSecretFile)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%w %s: empty", ErrSecretFile, path)
	}
	if !SafeHeaderValue(v) {
		return "", fmt.Errorf("%w %s: contains control characters or line breaks", ErrSecretFile, path)
	}
	return v, nil
}

// SafeHeaderValue reports whether v can be sent as one HTTP header value: tab, visible ASCII and bytes ≥ 0x80.
func SafeHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '\t' || (c >= 0x20 && c != 0x7f) {
			continue
		}
		return false
	}
	return true
}

// ValidHeaderName reports whether n is an HTTP token, and not one the agent manages itself.
func ValidHeaderName(n string) error {
	if n == "" {
		return errors.New("empty header name")
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return fmt.Errorf("header name %q has a character that isn't allowed", n)
		}
	}
	switch strings.ToLower(n) {
	case "host", "content-length", "transfer-encoding", "connection", "x-toprelay-caller", "x-toprelay-client",
		"x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "cf-connecting-ip", "mcp-session-id",
		"mcp-protocol-version", "mcp-method", "mcp-name", "content-type", "accept":
		return fmt.Errorf("header %q is set by TopRelay or the bot; pick the header your app checks for its token", n)
	}
	return nil
}
