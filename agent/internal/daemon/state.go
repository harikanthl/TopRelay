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
	// Limited counts calls refused by the service's own rate limits (toprelay limits).
	Limited int64 `json:"limited"`
	// Leaks counts answers in which the shield redacted or withheld credentials; Warnings counts answers with
	// text that tries to instruct the model.
	Leaks    int64 `json:"leaks"`
	Warnings int64 `json:"warnings"`
	// Held lists tools whose definition changed since approval: hidden and refused until `toprelay pins`
	// approves them.
	Held []string `json:"held,omitempty"`
}

// StateVersion is state.json's schema version. It goes up only for incompatible changes; new fields may appear
// in any version.
const StateVersion = 1

// State is the content of state.json.
type State struct {
	Version   int                      `json:"version"`
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
	subs map[chan struct{}]struct{}
}

// Subscribe returns a channel that receives (coalesced) after every write, and a function to unsubscribe.
func (f *StateFile) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = map[chan struct{}]struct{}{}
	}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}

// NewStateFile starts a fresh state for this process.
func NewStateFile(path string, now func() time.Time) *StateFile {
	if now == nil {
		now = time.Now
	}
	t := now().UTC()
	return &StateFile{path: path, now: now, s: State{Version: StateVersion, PID: os.Getpid(), StartedAt: t, UpdatedAt: t, Running: true, Services: map[string]*ServiceState{}}}
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
	if err := os.Rename(name, f.path); err != nil {
		return err
	}
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

// ReadState reads state.json (for `status` and the Mac app). A file from before versioning reads as version 1.
// If it says running but its process is gone (the agent crashed or was killed), Running is set to false and every
// service to disconnected.
func ReadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version == 0 {
		s.Version = StateVersion
	}
	if s.Services == nil {
		s.Services = map[string]*ServiceState{}
	}
	if s.Running && !processAlive(s.PID) {
		s.Running = false
		for _, st := range s.Services {
			st.Connected, st.Since = false, nil
		}
	}
	return &s, nil
}
