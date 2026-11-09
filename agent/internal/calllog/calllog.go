// Package calllog writes the local per-service call log: one JSON line per call, no bodies, no credentials.
//
// The log is tamper-evident. Every line carries a sequence number and the SHA-256 of the line before it, so
// editing, inserting or deleting a line breaks the chain; with a signing key, checkpoint lines sign the chain
// head with Ed25519 (see Verify).
package calllog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harikanthl/toprelay/agent/internal/mcpproxy"
	"github.com/harikanthl/toprelay/agent/internal/reqctx"
	"github.com/harikanthl/toprelay/agent/internal/shield"
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
	// Reason says why a call was refused or failed in the agent (tool_not_allowed, invalid_request,
	// rate_limited, credential_unavailable, app_unreachable).
	Reason   string `json:"reason,omitempty"`
	BytesIn  int64  `json:"bytesIn"`
	BytesOut int64  `json:"bytesOut"`
	// Leaks are the credential rules the shield matched in the app's answer ("jwt×2"); Blocked is set when the
	// answer was withheld instead of redacted.
	Leaks   []string `json:"leaks,omitempty"`
	Blocked bool     `json:"blocked,omitempty"`
	// Warnings are shield findings that didn't change the answer ("injection:ignore-instructions").
	Warnings []string `json:"warnings,omitempty"`

	// Type is empty for a call; "checkpoint" lines sign the chain, "prune" lines record retention.
	Type string `json:"type,omitempty"`
	// Seq numbers every line from 1; Prev is the SHA-256 (hex) of the line before. Both are set by Write.
	Seq  uint64 `json:"seq,omitempty"`
	Prev string `json:"prev,omitempty"`
	// Checkpoint fields: Sig is the Ed25519 signature (base64url) of CheckpointMessage, KeyID names the key.
	Sig   string `json:"sig,omitempty"`
	KeyID string `json:"keyId,omitempty"`
	// Prune fields: how many records retention removed, and the cutoff.
	Removed int        `json:"removed,omitempty"`
	Before  *time.Time `json:"before,omitempty"`
}

// Record types.
const (
	TypeCheckpoint = "checkpoint"
	TypePrune      = "prune"
)

// Checkpoint cadence: a checkpoint is written after this many lines or this long since the last one (checked
// on each write), and when the log closes.
const (
	CheckpointEvery    = 100
	CheckpointInterval = 5 * time.Minute
)

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
	// OnRecord, if set, is called after each call record is written (not checkpoints).
	OnRecord func(Record)

	// The chain: the last line's sequence number and hash.
	seq  uint64
	head string
	// Signing (SetSigner): checkpoints are written only with a key.
	service  string
	key      ed25519.PrivateKey
	keyID    string
	lastCkpt uint64
	ckptAt   time.Time
	now      func() time.Time
}

// Open opens (creating) the log at path and resumes its chain.
func Open(path string) (*Log, error) {
	l := &Log{path: path, maxSize: MaxSize, now: time.Now}
	if err := l.open(); err != nil {
		return nil, err
	}
	if err := l.resume(); err != nil {
		l.f.Close()
		return nil, err
	}
	return l, nil
}

// SetSigner turns on signed checkpoints for service with key.
func (l *Log) SetSigner(service string, key ed25519.PrivateKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.service, l.key, l.keyID = service, key, KeyID(key.Public().(ed25519.PublicKey))
	l.ckptAt = l.now()
}

// KeyID names a public key: the first 16 hex digits of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// CheckpointMessage is what a checkpoint signs: the service, the checkpoint's own sequence number, and Prev,
// the hash of the line before it, which commits to every earlier line.
func CheckpointMessage(service string, seq uint64, prev string, t time.Time) []byte {
	return []byte("toprelay/audit/v1\n" + service + "\n" + strconv.FormatUint(seq, 10) + "\n" + prev + "\n" + t.UTC().Format(time.RFC3339Nano))
}

func lineHash(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// resume finds the chain's last line (in the current file, else the rotated one) so new lines continue it.
func (l *Log) resume() error {
	for _, p := range []string{l.path, l.path + ".1"} {
		line, err := lastLine(p)
		if err != nil {
			return err
		}
		if line == nil {
			continue
		}
		var r struct {
			Seq uint64 `json:"seq"`
		}
		json.Unmarshal(line, &r)
		l.seq, l.head = r.Seq, lineHash(line)
		return nil
	}
	return nil
}

// lastLine returns the last complete line of a file (nil if none).
func lastLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 << 10
	for size := int64(chunk); ; size *= 2 {
		off := max(0, st.Size()-size)
		buf := make([]byte, st.Size()-off)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		end := bytes.LastIndexByte(buf, '\n')
		if end < 0 {
			if off == 0 {
				return nil, nil // no complete line
			}
			continue
		}
		start := bytes.LastIndexByte(buf[:end], '\n')
		if start >= 0 || off == 0 {
			line := bytes.TrimSpace(buf[start+1 : end])
			if len(line) == 0 {
				return nil, nil
			}
			return line, nil
		}
	}
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
	// A crash mid-write can leave a partial last line; end it so the next line starts clean.
	if st.Size() > 0 {
		last := make([]byte, 1)
		if r, err := os.Open(l.path); err == nil {
			r.ReadAt(last, st.Size()-1)
			r.Close()
			if last[0] != '\n' {
				n, _ := f.Write([]byte{'\n'})
				l.size += int64(n)
			}
		}
	}
	return nil
}

// Write appends one call record (its Seq and Prev are set here), and a checkpoint when one is due.
func (l *Log) Write(r Record) error {
	r.Type, r.Sig, r.KeyID = "", "", ""
	l.mu.Lock()
	err := l.appendLocked(&r)
	if err == nil && l.key != nil && (l.seq-l.lastCkpt >= CheckpointEvery || l.now().Sub(l.ckptAt) >= CheckpointInterval) {
		err = l.checkpointLocked()
	}
	l.mu.Unlock()
	if err == nil && l.OnRecord != nil {
		l.OnRecord(r)
	}
	return err
}

// appendLocked chains and writes one line, rotating first if it wouldn't fit. l.mu must be held.
func (l *Log) appendLocked(r *Record) error {
	r.Seq, r.Prev = l.seq+1, l.head
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if l.size+int64(len(b))+1 > l.maxSize && l.size > 0 {
		l.f.Close()
		if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := l.open(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(append(b, '\n'))
	l.size += int64(n)
	if err != nil {
		return err
	}
	l.seq, l.head = r.Seq, lineHash(b)
	return nil
}

// checkpointLocked signs the chain head. l.mu must be held and l.key set.
func (l *Log) checkpointLocked() error {
	if l.seq == l.lastCkpt {
		return nil
	}
	t := l.now().UTC()
	r := Record{Time: t, Type: TypeCheckpoint, KeyID: l.keyID}
	seq, prev := l.seq+1, l.head
	r.Sig = base64.RawURLEncoding.EncodeToString(ed25519.Sign(l.key, CheckpointMessage(l.service, seq, prev, t)))
	if err := l.appendLocked(&r); err != nil {
		return err
	}
	if r.Seq != seq || r.Prev != prev {
		return errors.New("call log: checkpoint chained out of order")
	}
	l.lastCkpt, l.ckptAt = l.seq, t
	return nil
}

// Checkpoint signs the chain head now (no-op without a key or with nothing new).
func (l *Log) Checkpoint() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.key == nil {
		return nil
	}
	return l.checkpointLocked()
}

// Prune drops records older than before from the log and its rotated copy. A line that doesn't parse is kept.
func (l *Log) Prune(before time.Time) (removed int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := pruneFile(l.path+".1", before)
	removed += n
	if err != nil {
		return removed, err
	}
	l.f.Close()
	n, err = pruneFile(l.path, before)
	removed += n
	if oerr := l.open(); oerr != nil {
		return removed, errors.Join(err, oerr)
	}
	if err == nil && removed > 0 && l.key != nil {
		// Retention is the one way lines may leave the log: say so in the chain and sign it.
		b := before.UTC()
		r := Record{Time: l.now().UTC(), Type: TypePrune, Removed: removed, Before: &b}
		if err = l.appendLocked(&r); err == nil {
			err = l.checkpointLocked()
		}
	}
	return removed, err
}

func pruneFile(path string, before time.Time) (int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var keep bytes.Buffer
	removed := 0
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r struct {
			Time time.Time `json:"time"`
		}
		// Only a prefix goes, so what stays is an unbroken stretch of the chain.
		if keep.Len() == 0 && json.Unmarshal(line, &r) == nil && !r.Time.IsZero() && r.Time.Before(before) {
			removed++
			continue
		}
		keep.Write(line)
	}
	if removed == 0 {
		return 0, nil
	}
	if keep.Len() == 0 {
		return removed, os.Remove(path)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, keep.Bytes(), 0o600); err != nil {
		return 0, err
	}
	return removed, os.Rename(tmp, path)
}

// Close signs the chain head (with a key) and closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var cerr error
	if l.key != nil {
		cerr = l.checkpointLocked()
	}
	return errors.Join(cerr, l.f.Close())
}

type statusWriter struct {
	http.ResponseWriter
	status int
	n      int64
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
	n, err := s.ResponseWriter.Write(p)
	s.n += int64(n)
	return n, err
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
		ctx, found := reqctx.WithFindings(r.Context())
		r = r.WithContext(ctx)
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
			rec.BytesIn = int64(len(body))
		} else if r.ContentLength > 0 {
			rec.BytesIn = r.ContentLength
		}
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			rec.Status = sw.status
			if rec.Status == 0 {
				rec.Status = http.StatusOK
			}
			rec.Refused = w.Header().Get(mcpproxy.RefusedHeader) != ""
			rec.Reason = w.Header().Get(mcpproxy.ReasonHeader)
			rec.BytesOut = sw.n
			rec.DurationMs = time.Since(start).Milliseconds()
			leaks, blocked, warnings := found.Snapshot()
			rec.Leaks, rec.Blocked, rec.Warnings = shield.Summary(leaks), blocked, warnings
			if len(rec.Leaks) == 0 {
				rec.Leaks = nil
			}
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
	var partial, lastLine []byte
	for {
		st, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			offset = 0
		case err != nil:
			return err
		default:
			if st.Size() < offset {
				// Smaller: rotated, or pruned (old records removed, the rest kept). After a prune the last line
				// printed is still there, so carry on after it instead of printing everything again.
				offset = 0
				partial = nil
				if len(lastLine) > 0 {
					if b, err := os.ReadFile(path); err == nil {
						if i := bytes.LastIndex(b, append(append([]byte{}, lastLine...), '\n')); i >= 0 {
							offset = int64(i + len(lastLine) + 1)
						}
					}
				}
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
					lastLine = append(lastLine[:0], partial[:i]...)
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
