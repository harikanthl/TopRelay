// Package redact removes secrets from text before it is printed or logged.
package redact

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const mask = "[REDACTED]"

var (
	mu      sync.RWMutex
	secrets []string
)

// keyPattern matches TopRelay agent and bearer keys, which must never reach a log.
var keyPattern = regexp.MustCompile(`tr[ak]1\.[A-Za-z0-9_\-]+(\.[A-Za-z0-9_\-]+)?`)

// Add registers secret values that must never be printed. Short values are ignored to avoid masking
// ordinary words.
func Add(values ...string) {
	mu.Lock()
	defer mu.Unlock()
	for _, v := range values {
		if len(v) < 8 {
			continue
		}
		dup := false
		for _, s := range secrets {
			if s == v {
				dup = true
				break
			}
		}
		if !dup {
			secrets = append(secrets, v)
		}
	}
	// Longest first, so a secret containing another is masked whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
}

// Values returns the registered secrets, longest first (a copy).
func Values() []string {
	mu.RLock()
	defer mu.RUnlock()
	return append([]string(nil), secrets...)
}

// String masks every registered secret in s.
func String(s string) string {
	mu.RLock()
	defer mu.RUnlock()
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, mask)
	}
	return s
}

// Log masks registered secrets and anything shaped like a TopRelay key. Use it for logs and errors.
func Log(s string) string {
	return keyPattern.ReplaceAllString(String(s), mask)
}

// Writer wraps w so every write is masked. With keys set, TopRelay key shapes are masked too.
type Writer struct {
	W    io.Writer
	Keys bool
}

func (r Writer) Write(p []byte) (int, error) {
	s := string(p)
	if r.Keys {
		s = Log(s)
	} else {
		s = String(s)
	}
	if _, err := io.WriteString(r.W, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Reset forgets every registered secret (for tests).
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	secrets = nil
}
