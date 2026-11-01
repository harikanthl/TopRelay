package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ServiceState is one service's live status in state.json.
type ServiceState struct {
	Connected        bool       `json:"connected"`
	Since            *time.Time `json:"since"`
	LastError        string     `json:"lastError"`
	ReconnectAttempt int        `json:"reconnectAttempt"`
	NextRetryMs      int64      `json:"nextRetryMs"`
	Requests         int64      `json:"requests"`
	Refused          int64      `json:"refused"`
}

// State is the content of state.json.
type State struct {
	PID       int                      `json:"pid"`
	StartedAt time.Time                `json:"startedAt"`
	UpdatedAt time.Time                `json:"updatedAt"`
	Running   bool                     `json:"running"`
	Services  map[string]*ServiceState `json:"services"`
}

// StatePath is the state file in the config dir.
func StatePath(dir string) string { return filepath.Join(dir, "state.json") }

// StateFile writes state.json atomically on every change.
type StateFile struct {
	path string
	mu   sync.Mutex
	s    State
	now  func() time.Time
}

// NewStateFile starts a fresh state for this process.
func NewStateFile(path string, now func() time.Time) *StateFile {
	if now == nil {
		now = time.Now
	}
	t := now().UTC()
	return &StateFile{path: path, now: now, s: State{PID: os.Getpid(), StartedAt: t, UpdatedAt: t, Running: true, Services: map[string]*ServiceState{}}}
}

// Update changes one service's state and writes the file.
func (f *StateFile) Update(service string, fn func(*ServiceState)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.s.Services[service]
	if st == nil {
		st = &ServiceState{}
		f.s.Services[service] = st
	}
	fn(st)
	return f.writeLocked()
}

// Remove drops a service and writes the file.
func (f *StateFile) Remove(service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.s.Services, service)
	return f.writeLocked()
}

// Stop marks the daemon stopped and every service disconnected.
func (f *StateFile) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s.Running = false
	for _, st := range f.s.Services {
		st.Connected = false
		st.Since = nil
	}
	return f.writeLocked()
}

// Write writes the file as it is now.
func (f *StateFile) Write() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeLocked()
}

// Snapshot returns a copy of the state.
func (f *StateFile) Snapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.s)
	var s State
	json.Unmarshal(b, &s)
	return s
}

func (f *StateFile) writeLocked() error {
	f.s.UpdatedAt = f.now().UTC()
	b, err := json.MarshalIndent(f.s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.path), ".state-*.json")
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
	return os.Rename(name, f.path)
}

// ReadState reads state.json (for `status` and the Mac app).
func ReadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
