package internal

import (
	"sync"
	"time"
)

// StackConfig is the persisted, per-stack configuration: the group-update
// toggle plus the outcome of the last completed run, so both survive a
// restart. LastState only ever holds a terminal RunState (success/failed)
// - the transient "updating" state is never persisted, so a crash
// mid-update can't leave a stale "updating" ghost on the next start.
type StackConfig struct {
	Enabled   bool      `json:"enabled"`
	LastState string    `json:"lastState,omitempty"`
	LastRun   time.Time `json:"lastRun,omitzero"`
	LastError string    `json:"lastError,omitempty"`
	// PostUpdateScript, if set, is run once `docker compose up -d` has
	// succeeded, via `docker compose exec` into PostUpdateContainer (see
	// runPostScript). PostUpdateContainer may be left blank when the
	// stack has exactly one service - it's auto-detected in that case -
	// and must be set explicitly for multi-service stacks.
	PostUpdateScript    string `json:"postUpdateScript,omitempty"`
	PostUpdateContainer string `json:"postUpdateContainer,omitempty"`
}

// defaultFinalStepScript is what a fresh install (or an existing state.json
// from before this feature existed) gets seeded with the first time it
// loads - see the finalStepConfigured handling in NewStateStore.
const defaultFinalStepScript = "docker image prune -a -f"

type stateFile struct {
	Stacks map[string]StackConfig `json:"stacks"`

	// FinalStep runs once after a group "Update selected" run finishes
	// (not after single-container updates), independent of any single
	// stack's outcome. FinalStepConfigured distinguishes "never touched,
	// apply the default" from "user explicitly set/cleared it" - without
	// it, an explicitly-disabled or blanked-out final step would keep
	// reverting to the default on every load, since Go's zero value for
	// an unset bool/string looks identical to "deliberately off/empty".
	FinalStepEnabled    bool   `json:"finalStepEnabled"`
	FinalStepScript     string `json:"finalStepScript"`
	FinalStepConfigured bool   `json:"finalStepConfigured"`
}

// StateStore persists which stacks are included in group updates.
type StateStore struct {
	mu   sync.Mutex
	path string
	data stateFile
}

func NewStateStore(path string) (*StateStore, error) {
	s := &StateStore{path: path, data: stateFile{Stacks: map[string]StackConfig{}}}
	if err := loadJSON(path, &s.data); err != nil {
		return nil, err
	}
	if s.data.Stacks == nil {
		s.data.Stacks = map[string]StackConfig{}
	}
	if !s.data.FinalStepConfigured {
		s.data.FinalStepEnabled = true
		s.data.FinalStepScript = defaultFinalStepScript
		s.data.FinalStepConfigured = true
		if err := saveJSON(s.path, &s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Sync reconciles the persisted state against the set of stack names
// currently found on disk: new stacks default to enabled, stacks that
// no longer exist are dropped. Returns the merged config.
func (s *StateStore) Sync(found []string) (map[string]StackConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := make(map[string]StackConfig, len(found))
	for _, name := range found {
		if cfg, ok := s.data.Stacks[name]; ok {
			next[name] = cfg
		} else {
			next[name] = StackConfig{Enabled: true}
		}
	}
	s.data.Stacks = next
	if err := saveJSON(s.path, &s.data); err != nil {
		return nil, err
	}
	out := make(map[string]StackConfig, len(next))
	for k, v := range next {
		out[k] = v
	}
	return out, nil
}

func (s *StateStore) SetEnabled(name string, enabled bool) (StackConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.data.Stacks[name]
	cfg.Enabled = enabled
	s.data.Stacks[name] = cfg
	if err := saveJSON(s.path, &s.data); err != nil {
		return StackConfig{}, err
	}
	return cfg, nil
}

func (s *StateStore) SetPostScript(name, container, script string) (StackConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.data.Stacks[name]
	cfg.PostUpdateScript = script
	cfg.PostUpdateContainer = container
	s.data.Stacks[name] = cfg
	if err := saveJSON(s.path, &s.data); err != nil {
		return StackConfig{}, err
	}
	return cfg, nil
}

// SetStatus persists the outcome of a completed run for name. Callers
// should only pass terminal states (success/failed) - see StackConfig.
func (s *StateStore) SetStatus(name string, state RunState, lastRun time.Time, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.data.Stacks[name]
	cfg.LastState = string(state)
	cfg.LastRun = lastRun
	cfg.LastError = lastError
	s.data.Stacks[name] = cfg
	return saveJSON(s.path, &s.data)
}

func (s *StateStore) GetFinalStep() (enabled bool, script string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.FinalStepEnabled, s.data.FinalStepScript
}

func (s *StateStore) SetFinalStep(enabled bool, script string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data.FinalStepEnabled = enabled
	s.data.FinalStepScript = script
	s.data.FinalStepConfigured = true
	return saveJSON(s.path, &s.data)
}

func (s *StateStore) Get(name string) (StackConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, ok := s.data.Stacks[name]
	return cfg, ok
}

func (s *StateStore) Snapshot() map[string]StackConfig {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]StackConfig, len(s.data.Stacks))
	for k, v := range s.data.Stacks {
		out[k] = v
	}
	return out
}
