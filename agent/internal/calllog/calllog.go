// Package calllog writes the local per-service call log: one JSON line per call, no bodies, no credentials.
package calllog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
)

// MaxSize is the size at which the log rotates (one old file is kept).
const MaxSize = 10 << 20

// Record is one call.
type Record struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	MCPMethod  string    `json:"mcpMethod,omitempty"`
	Tool       string    `json:"tool,omitempty"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"durationMs"`
	Client     string    `json:"client,omitempty"`
	Refused    bool      `json:"refused,omitempty"`
}

// Path is the log file for a service.
func Path(configDir, service string) string {
	return filepath.Join(configDir, "logs", service+".jsonl")
}

// Log appends records to a file, rotating it at MaxSize.
type Log struct {
	path    string
	maxSize int64
	mu      sync.Mutex
	f       *os.File
	size    int64
	// OnRecord, if set, is called after each record is written.
	OnRecord func(Record)
}

// Open opens (creating) the log at path.
func Open(path string) (*Log, error) {
	l := &Log{path: path, maxSize: MaxSize}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Log) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends one record.
func (l *Log) Write(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.mu.Lock()
	if l.size+int64(len(b)) > l.maxSize && l.size > 0 {
		l.f.Close()
		if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			l.mu.Unlock()
			return err
		}
		if err := l.open(); err != nil {
			l.mu.Unlock()
			return err
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	l.mu.Unlock()
	if err == nil && l.OnRecord != nil {
		l.OnRecord(r)
	}
	return err
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(p)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Middleware logs every request that passes through next.
func (l *Log) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := Record{Time: start.UTC(), Method: r.Method, Path: r.URL.Path, Client: r.Header.Get("X-Toprelay-Client"),
			MCPMethod: r.Header.Get("Mcp-Method"), Tool: r.Header.Get("Mcp-Name")}
		if r.Method == http.MethodPost && r.Body != nil && strings.HasPrefix(r.URL.Path, "/mcp") {
			body, err := io.ReadAll(io.LimitReader(r.Body, mcpproxy.MaxBody+1))
			if err == nil {
				if msgs, _, perr := mcpproxy.Parse(body); perr == nil && len(msgs) > 0 {
					if rec.MCPMethod == "" {
						rec.MCPMethod = msgs[0].Method
					}
					if rec.Tool == "" && msgs[0].Method == "tools/call" {
						rec.Tool = msgs[0].ToolName()
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			rec.Status = sw.status
			if rec.Status == 0 {
				rec.Status = http.StatusOK
			}
			rec.Refused = w.Header().Get(mcpproxy.RefusedHeader) != ""
			rec.DurationMs = time.Since(start).Milliseconds()
			l.Write(rec)
		}()
		next.ServeHTTP(sw, r)
	})
}

// Tail returns the last n lines of the log (including the rotated file when needed).
func Tail(path string, n int) ([]string, error) {
	var lines []string
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if n > 0 && len(lines) > n {
				lines = lines[1:]
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// Follow calls fn for every line appended to the log after offset 'from' (use the current size to skip history),
// following rotation, until ctx ends.
func Follow(ctx context.Context, path string, poll time.Duration, fn func(string)) error {
	var offset int64
	if st, err := os.Stat(path); err == nil {
		offset = st.Size()
	}
	var partial []byte
	for {
		st, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			offset = 0
		case err != nil:
			return err
		default:
			if st.Size() < offset { // rotated or truncated
				offset = 0
				partial = nil
			}
			if st.Size() > offset {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				f.Seek(offset, io.SeekStart)
				b, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					return err
				}
				offset += int64(len(b))
				partial = append(partial, b...)
				for {
					i := bytes.IndexByte(partial, '\n')
					if i < 0 {
						break
					}
					fn(string(partial[:i]))
					partial = partial[i+1:]
				}
			}
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
