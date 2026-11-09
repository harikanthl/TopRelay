// Package pins remembers each MCP tool's definition (name, description, schemas, annotations) as a SHA-256
// fingerprint. A tool is pinned the first time it is seen; if its definition later changes (a "rug pull"), it is
// held: hidden from bots and refused, until the owner reviews and approves the change.
package pins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// canon is the part of a tool definition a model reads or a policy uses. Annotations are reduced to their
// effective values (MCP defaults applied), so "absent" and "false" fingerprint the same when they mean the same.
type canon struct {
	Name         string   `json:"name"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	InputSchema  any      `json:"inputSchema"`
	OutputSchema any      `json:"outputSchema"`
	Annotations  canonAnn `json:"annotations"`
}

type canonAnn struct {
	Title       string `json:"title"`
	ReadOnly    bool   `json:"readOnly"`
	Destructive bool   `json:"destructive"`
	Idempotent  bool   `json:"idempotent"`
	OpenWorld   bool   `json:"openWorld"`
}

// Fingerprint is the SHA-256 (hex) of a tool definition's canonical form. raw is one element of a tools/list
// result's "tools" array.
func Fingerprint(raw []byte) (string, error) {
	var t struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		InputSchema  any    `json:"inputSchema"`
		OutputSchema any    `json:"outputSchema"`
		Annotations  *struct {
			Title           string `json:"title"`
			ReadOnlyHint    bool   `json:"readOnlyHint"`
			DestructiveHint *bool  `json:"destructiveHint"`
			IdempotentHint  bool   `json:"idempotentHint"`
			OpenWorldHint   *bool  `json:"openWorldHint"`
		} `json:"annotations"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", err
	}
	c := canon{Name: t.Name, Title: t.Title, Description: t.Description, InputSchema: t.InputSchema,
		OutputSchema: t.OutputSchema, Annotations: canonAnn{Destructive: true, OpenWorld: true}}
	if a := t.Annotations; a != nil {
		c.Annotations.Title, c.Annotations.ReadOnly, c.Annotations.Idempotent = a.Title, a.ReadOnlyHint, a.IdempotentHint
		if a.DestructiveHint != nil {
			c.Annotations.Destructive = *a.DestructiveHint
		}
		if a.OpenWorldHint != nil {
			c.Annotations.OpenWorld = *a.OpenWorldHint
		}
	}
	if c.Annotations.ReadOnly {
		c.Annotations.Destructive = false
	}
	b, err := json.Marshal(c) // map keys come out sorted, so equal definitions give equal bytes
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// FingerprintTool fingerprints a tool listed through the MCP SDK; it equals Fingerprint of the tool's JSON.
func FingerprintTool(t *mcp.Tool) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return Fingerprint(b)
}

// Tool is a listed tool, as seen by the agent.
type Tool struct {
	Name        string
	Fingerprint string
	Description string
}

// Pin is an approved definition.
type Pin struct {
	SHA256      string    `json:"sha256"`
	Description string    `json:"description,omitempty"`
	PinnedAt    time.Time `json:"pinnedAt"`
}

// Change is a definition that differs from the pin and waits for review.
type Change struct {
	SHA256      string    `json:"sha256"`
	Description string    `json:"description,omitempty"`
	SeenAt      time.Time `json:"seenAt"`
}

// File is the content of a pins file.
type File struct {
	Version int               `json:"version"`
	Tools   map[string]Pin    `json:"tools"`
	Held    map[string]Change `json:"held,omitempty"`
}

// FileVersion is the pins file schema version.
const FileVersion = 1

// Path is a service's pins file.
func Path(configDir, service string) string {
	return filepath.Join(configDir, "pins", service+".json")
}

// Store is one service's pins, shared between the running agent and the CLI through the file. Every change is
// a locked read-modify-write, so neither side overwrites the other.
type Store struct {
	path string
	now  func() time.Time

	mu    sync.RWMutex
	f     File
	stamp string
}

// Open loads (or starts) the pins at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// stampOf identifies the file's content (a hash, not the mtime, so two quick writes can't look the same).
func stampOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Store) load() error {
	f, err := readFile(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.f, s.stamp = f, stampOf(s.path)
	s.mu.Unlock()
	return nil
}

func readFile(path string) (File, error) {
	f := File{Version: FileVersion, Tools: map[string]Pin{}, Held: map[string]Change{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("pins file %s is damaged: %w (delete it to pin every tool again)", path, err)
	}
	if f.Version > FileVersion {
		return f, fmt.Errorf("pins file %s is from a newer toprelay (version %d)", path, f.Version)
	}
	if f.Tools == nil {
		f.Tools = map[string]Pin{}
	}
	if f.Held == nil {
		f.Held = map[string]Change{}
	}
	return f, nil
}

func writeFile(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f.Version = FileVersion
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pins-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// lock takes the file lock: an exclusive-create lock file, portable across systems. A lock older than
// staleLock is left over from a crash and is broken.
func (s *Store) lock() (func(), error) {
	const staleLock = 10 * time.Second
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	lp := s.path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(lp) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(lp); serr == nil && time.Since(st.ModTime()) > staleLock {
			os.Remove(lp)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pins file %s is locked by another toprelay (remove %s if none is running)", s.path, lp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// update runs fn on the freshest file content under the lock and saves when fn reports a change.
func (s *Store) update(fn func(*File) bool) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := readFile(s.path)
	if err != nil {
		return err
	}
	if fn(&f) {
		if err := writeFile(s.path, f); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.f, s.stamp = f, stampOf(s.path)
	s.mu.Unlock()
	return nil
}

// Observe records a listing. New tools are pinned. A tool whose fingerprint differs from its pin is held; one
// that went back to its pinned definition is released. It returns the tools held after the listing that weren't
// held before it.
func (s *Store) Observe(tools []Tool) (newlyHeld []string, err error) {
	if !s.needsUpdate(tools) {
		return nil, nil
	}
	err = s.update(func(f *File) bool {
		changed := false
		now := s.now().UTC()
		for _, t := range tools {
			if t.Name == "" || t.Fingerprint == "" {
				continue
			}
			p, pinned := f.Tools[t.Name]
			switch {
			case !pinned:
				f.Tools[t.Name] = Pin{SHA256: t.Fingerprint, Description: t.Description, PinnedAt: now}
				changed = true
			case p.SHA256 == t.Fingerprint:
				if _, held := f.Held[t.Name]; held {
					delete(f.Held, t.Name)
					changed = true
				}
			default:
				if c, held := f.Held[t.Name]; !held || c.SHA256 != t.Fingerprint {
					if !held {
						newlyHeld = append(newlyHeld, t.Name)
					}
					f.Held[t.Name] = Change{SHA256: t.Fingerprint, Description: t.Description, SeenAt: now}
					changed = true
				}
			}
		}
		return changed
	})
	return newlyHeld, err
}

// needsUpdate reports whether a listing would change the file, from the in-memory copy (most listings don't,
// and those skip the lock and the disk).
func (s *Store) needsUpdate(tools []Tool) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range tools {
		if t.Name == "" || t.Fingerprint == "" {
			continue
		}
		p, pinned := s.f.Tools[t.Name]
		c, held := s.f.Held[t.Name]
		switch {
		case !pinned:
			return true
		case p.SHA256 == t.Fingerprint && held:
			return true
		case p.SHA256 != t.Fingerprint && (!held || c.SHA256 != t.Fingerprint):
			return true
		}
	}
	return false
}

// IsHeld reports whether a tool's definition changed since it was approved.
func (s *Store) IsHeld(tool string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.f.Held[tool]
	return ok
}

// Held returns the held tools, sorted.
func (s *Store) Held() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.f.Held))
}

// Snapshot returns a copy of the pins.
func (s *Store) Snapshot() File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return File{Version: s.f.Version, Tools: maps.Clone(s.f.Tools), Held: maps.Clone(s.f.Held)}
}

// Approve accepts the held definitions of tools (all held tools when tools is empty) as their new pins. It
// returns the tools approved; a name that isn't held is ignored.
func (s *Store) Approve(tools []string) (approved []string, err error) {
	err = s.update(func(f *File) bool {
		names := tools
		if len(names) == 0 {
			names = slices.Sorted(maps.Keys(f.Held))
		}
		for _, n := range names {
			c, ok := f.Held[n]
			if !ok {
				continue
			}
			f.Tools[n] = Pin{SHA256: c.SHA256, Description: c.Description, PinnedAt: s.now().UTC()}
			delete(f.Held, n)
			approved = append(approved, n)
		}
		return len(approved) > 0
	})
	return approved, err
}

// Forget drops the pins of tools (every pin when tools is empty): they are pinned again, as they are, the next
// time they're listed.
func (s *Store) Forget(tools []string) (forgotten []string, err error) {
	err = s.update(func(f *File) bool {
		names := tools
		if len(names) == 0 {
			names = slices.Sorted(maps.Keys(f.Tools))
		}
		for _, n := range names {
			_, p := f.Tools[n]
			_, h := f.Held[n]
			if p || h {
				delete(f.Tools, n)
				delete(f.Held, n)
				forgotten = append(forgotten, n)
			}
		}
		return len(forgotten) > 0
	})
	return forgotten, err
}

// Refresh re-reads the file if another process changed it. It reports whether the held set changed.
func (s *Store) Refresh() (bool, error) {
	s.mu.RLock()
	same := s.stamp == stampOf(s.path)
	before := slices.Sorted(maps.Keys(s.f.Held))
	s.mu.RUnlock()
	if same {
		return false, nil
	}
	if err := s.load(); err != nil {
		return false, err
	}
	return !slices.Equal(before, s.Held()), nil
}
