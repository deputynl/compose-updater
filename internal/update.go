package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type RunState string

const (
	StateIdle     RunState = "idle"
	StateUpdating RunState = "updating"
	StateSuccess  RunState = "success"
	StateFailed   RunState = "failed"
)

type StackStatus struct {
	Name      string    `json:"name"`
	State     RunState  `json:"state"`
	LastRun   time.Time `json:"lastRun,omitzero"`
	LastError string    `json:"lastError,omitempty"`
}

const maxLogLines = 500

// Manager runs `docker compose pull && up -d` for one or more stacks,
// capped at maxParallel concurrent stacks, and broadcasts progress over
// SSE via hub.
type Manager struct {
	composeRoot string
	maxParallel int
	hub         *hub
	state       *StateStore // persists terminal status/lastRun across restarts

	mu       sync.Mutex
	statuses map[string]*StackStatus
	logs     map[string][]string
	progress map[string]*stackProgress
	active   bool
}

func NewManager(composeRoot string, maxParallel int, state *StateStore) *Manager {
	if maxParallel < 1 {
		maxParallel = 1
	}
	return &Manager{
		composeRoot: composeRoot,
		maxParallel: maxParallel,
		hub:         newHub(),
		state:       state,
		statuses:    map[string]*StackStatus{},
		logs:        map[string][]string{},
		progress:    map[string]*stackProgress{},
	}
}

// Reconcile adds entries for new stack names - seeded from the last
// persisted outcome, if any - and drops ones that no longer exist,
// preserving in-memory status for names still present.
func (m *Manager) Reconcile(names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := make(map[string]*StackStatus, len(names))
	for _, name := range names {
		if s, ok := m.statuses[name]; ok {
			next[name] = s
			continue
		}
		st := &StackStatus{Name: name, State: StateIdle}
		if m.state != nil {
			if cfg, ok := m.state.Get(name); ok && cfg.LastState != "" {
				st.State = RunState(cfg.LastState)
				st.LastRun = cfg.LastRun
				st.LastError = cfg.LastError
			}
		}
		next[name] = st
	}
	m.statuses = next

	nextProgress := make(map[string]*stackProgress, len(names))
	for _, name := range names {
		if p, ok := m.progress[name]; ok {
			nextProgress[name] = p
		}
	}
	m.progress = nextProgress
}

type Snapshot struct {
	Active   bool                      `json:"active"`
	Statuses map[string]*StackStatus   `json:"statuses"`
	Progress map[string]ProgressUpdate `json:"progress"`
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	statuses := make(map[string]*StackStatus, len(m.statuses))
	for k, v := range m.statuses {
		cp := *v
		statuses[k] = &cp
	}
	progressByName := make(map[string]*stackProgress, len(m.progress))
	for k, v := range m.progress {
		progressByName[k] = v
	}
	active := m.active
	m.mu.Unlock()

	progress := make(map[string]ProgressUpdate, len(progressByName))
	for k, v := range progressByName {
		progress[k] = v.Last()
	}
	return Snapshot{Active: active, Statuses: statuses, Progress: progress}
}

func (m *Manager) Subscribe() chan Event     { return m.hub.subscribe() }
func (m *Manager) Unsubscribe(ch chan Event) { m.hub.unsubscribe(ch) }

// Status returns a copy of the current status for a single stack.
func (m *Manager) Status(name string) (StackStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.statuses[name]
	if !ok {
		return StackStatus{}, false
	}
	return *s, true
}

// StartRun kicks off updates for the given stack names in the
// background, running up to maxParallel at once. It returns an error if
// a run is already active or if a requested stack is unknown.
// isGroupRun marks a "Update selected" batch (as opposed to a single-
// container update) - only group runs trigger the final step afterward.
func (m *Manager) StartRun(names []string, isGroupRun bool) error {
	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		return fmt.Errorf("an update is already running")
	}
	for _, n := range names {
		if _, ok := m.statuses[n]; !ok {
			m.mu.Unlock()
			return fmt.Errorf("unknown stack %q", n)
		}
	}
	if len(names) == 0 {
		m.mu.Unlock()
		return fmt.Errorf("no stacks selected")
	}
	m.active = true
	m.mu.Unlock()

	log.Printf("run starting: stacks=%v group=%v", names, isGroupRun)
	go m.runAll(names, isGroupRun)
	return nil
}

func (m *Manager) runAll(names []string, isGroupRun bool) {
	defer func() {
		m.mu.Lock()
		m.active = false
		m.mu.Unlock()
		log.Printf("run finished: stacks=%v", names)
		m.hub.broadcast(Event{Name: "done", Data: map[string]any{}})
	}()

	sem := make(chan struct{}, m.maxParallel)
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-sem }()
			m.runStack(name)
		}(name)
	}
	wg.Wait()

	// Runs once for the whole batch, after every stack has finished
	// (regardless of individual outcomes) - "active" stays true and the
	// UI keeps showing "updating…" through this step too, since it's
	// still part of the same run from the user's point of view.
	if isGroupRun {
		m.runFinalStep()
	}
}

// finalStepLogTag tags final-step console lines. Parenthesized so it
// can't collide with a real stack/folder name.
const finalStepLogTag = "(cleanup)"

func (m *Manager) runFinalStep() {
	if m.state == nil {
		return
	}
	enabled, script := m.state.GetFinalStep()
	script = strings.TrimSpace(script)
	if !enabled || script == "" {
		return
	}
	m.runFinalStepScript(script)
}

func (m *Manager) runFinalStepScript(script string) {
	m.appendLog(finalStepLogTag, "Running final step...")
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = m.composeRoot
	if err := m.runAndStream(cmd, func(line string) {
		m.appendLog(finalStepLogTag, line)
	}); err != nil {
		m.appendLog(finalStepLogTag, "final step failed: "+err.Error())
		return
	}
	m.appendLog(finalStepLogTag, "Final step completed")
}

// RunFinalStepNow runs script (the final-step dialog's current field
// value, saved or not) immediately, independent of any "Update selected"
// run and regardless of the "run after Update selected" toggle - it's an
// explicit manual trigger, handy for testing an edit before saving it.
// Returns an error if a run is already active or script is blank.
func (m *Manager) RunFinalStepNow(script string) error {
	script = strings.TrimSpace(script)
	if script == "" {
		return fmt.Errorf("no final step script to run")
	}

	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		return fmt.Errorf("an update is already running")
	}
	m.active = true
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			m.active = false
			m.mu.Unlock()
			m.hub.broadcast(Event{Name: "done", Data: map[string]any{}})
		}()
		m.runFinalStepScript(script)
	}()
	return nil
}

func (m *Manager) runStack(name string) {
	m.setStatus(name, StateUpdating, "", false)

	dir := filepath.Join(m.composeRoot, name)
	if _, err := m.runCompose(name, dir, "pull", []string{"pull"}); err != nil {
		m.setStatus(name, StateFailed, err.Error(), true)
		return
	}
	upProgress, err := m.runCompose(name, dir, "up", []string{"up", "-d", "--remove-orphans"})
	if err != nil {
		m.setStatus(name, StateFailed, err.Error(), true)
		return
	}
	// "Updated" should reflect the last time something actually changed,
	// not the last time we merely checked - an unchanged container just
	// reports "Running" during `up`, while a real change reports phase
	// words like "Recreate"/"Started". See changeWords in progress.go.
	changed := upProgress.Changed()

	if m.state != nil {
		if cfg, ok := m.state.Get(name); ok {
			if script := strings.TrimSpace(cfg.PostUpdateScript); script != "" {
				m.appendLog(name, "Running post-update steps...")
				if err := m.runPostScript(name, dir, cfg.PostUpdateContainer, script); err != nil {
					m.setStatus(name, StateFailed, "post-update steps failed: "+err.Error(), true)
					return
				}
				m.appendLog(name, "Post-update steps completed")
			}
		}
	}

	m.setStatus(name, StateSuccess, "", changed)
}

// runCompose runs `docker compose <args...>` in dir, asking compose for
// structured JSON progress events (COMPOSE_PROGRESS=json) so byte-level
// pull progress and container state changes can be aggregated into a
// live bar. Compose versions too old to understand that env var just
// ignore it and print their normal human-readable lines instead, which
// fall back to being logged verbatim with no progress bar - see
// friendlyLine/handleComposeLine.
func (m *Manager) runCompose(name, dir, phase string, args []string) (*stackProgress, error) {
	sp := newStackProgress(name, phase)
	m.mu.Lock()
	m.progress[name] = sp
	m.mu.Unlock()
	m.hub.broadcast(Event{Name: "progress", Data: sp.Last()})

	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "COMPOSE_PROGRESS=json")

	err := m.runAndStream(cmd, func(line string) {
		m.handleComposeLine(name, phase, sp, line)
	})
	return sp, err
}

// runPostScript runs a user-authored shell script once the stack is up,
// via `docker compose exec` into the target service's container, streaming
// its combined output into the same console log as the compose commands.
// It carries the same trust level as the rest of this tool: whoever can
// configure it already has full control over what gets run via the update
// mechanism itself.
func (m *Manager) runPostScript(name, dir, container, script string) error {
	if container == "" {
		services, err := composeServices(dir)
		if err != nil {
			return fmt.Errorf("detect service: %w", err)
		}
		switch len(services) {
		case 0:
			return fmt.Errorf("no services found in this stack")
		case 1:
			container = services[0]
		default:
			return fmt.Errorf("stack has multiple services (%s) - set a container name in post-update settings", strings.Join(services, ", "))
		}
	}

	cmd := exec.Command("docker", "compose", "exec", "-T", container, "sh", "-c", script)
	cmd.Dir = dir
	return m.runAndStream(cmd, func(line string) {
		m.appendLog(name, line)
	})
}

// composeServices lists the service names defined in dir's compose file.
func composeServices(dir string) ([]string, error) {
	cmd := exec.Command("docker", "compose", "config", "--services")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			services = append(services, line)
		}
	}
	return services, nil
}

// runAndStream starts cmd with stdout+stderr merged, calling onLine for
// each line of output as it arrives, and returns cmd.Wait()'s error.
func (m *Manager) runAndStream(cmd *exec.Cmd, onLine func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		onLine("log stream error: " + err.Error())
	}

	return cmd.Wait()
}

func (m *Manager) handleComposeLine(name, phase string, sp *stackProgress, line string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return
	}

	var ev composeEvent
	var ok bool
	if err := json.Unmarshal([]byte(trimmed), &ev); err == nil && (ev.ID != "" || ev.Text != "") {
		ok = true
	} else if pev, matched := parsePlainLine(line); matched {
		// COMPOSE_PROGRESS=json didn't take effect (unsupported compose
		// version, or just not honored in this environment) - fall back
		// to parsing compose's classic bracket-bar text output instead.
		ev, ok = pev, true
	}
	if !ok {
		// Neither form recognized - stray warning line, deprecation
		// notice, etc. Show it verbatim rather than dropping it.
		m.appendLog(name, line)
		return
	}

	update := sp.Apply(name, ev)
	m.hub.broadcast(Event{Name: "progress", Data: update})

	if text, show := friendlyLine(phase, ev); show {
		m.appendLog(name, text)
	}
}

func (m *Manager) appendLog(name, line string) {
	m.mu.Lock()
	lines := append(m.logs[name], line)
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	m.logs[name] = lines
	m.mu.Unlock()

	log.Printf("[%s] %s", name, line)
	m.hub.broadcast(Event{Name: "log", Data: map[string]string{"stack": name, "line": line}})
}

// touched marks that this call should bump LastRun to now - a failure is
// always touched (worth knowing when it last failed), a success only
// when something actually changed, so a no-op run leaves the "Updated"
// timestamp at whenever the last real change was.
func (m *Manager) setStatus(name string, state RunState, errMsg string, touched bool) {
	m.mu.Lock()
	s, ok := m.statuses[name]
	if !ok {
		s = &StackStatus{Name: name}
		m.statuses[name] = s
	}
	s.State = state
	s.LastError = errMsg
	if touched {
		s.LastRun = time.Now()
	}
	cp := *s
	m.mu.Unlock()

	if errMsg != "" {
		log.Printf("%s: %s (%s)", name, state, errMsg)
	} else {
		log.Printf("%s: %s", name, state)
	}

	if (state == StateSuccess || state == StateFailed) && m.state != nil {
		_ = m.state.SetStatus(name, state, cp.LastRun, errMsg)
	}

	m.hub.broadcast(Event{Name: "stack", Data: cp})
}
