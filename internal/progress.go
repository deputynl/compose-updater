package internal

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// composeEvent mirrors the JSON lines `docker compose` prints when
// COMPOSE_PROGRESS=json is set. The field *names* are stable across
// compose versions, but which field carries the human-readable phase
// word ("Downloading", "Created", ...) is NOT:
//
//   - compose v5 (cmd/display/json.go): Status is the coarse
//     "Working"/"Done"/"Warning"/"Error", and Text carries the phase
//     word.
//   - compose v2.x (pkg/progress/json.go): Text is usually empty and
//     Status carries the phase word instead (for pull layer events
//     specifically, v2 *does* also set Text to the phase word, same as
//     v5 - it's only container/up-phase and top-level pull events where
//     the two versions disagree).
//
// So phaseWord() below prefers Text and falls back to Status, and
// "done" is detected from Percent==100 (byte progress) or a known
// vocabulary of terminal phase words (container lifecycle), rather
// than trusting either field's raw value to mean one specific thing.
// Verified against docker/compose source at tags v5.4.0 and v2.40.3.
type composeEvent struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Status   string `json:"status"`
	Text     string `json:"text"`
	Details  string `json:"details"`
	Current  int64  `json:"current"`
	Total    int64  `json:"total"`
	Percent  int    `json:"percent"`
}

// phaseWord returns the human-readable phase ("Downloading", "Created",
// "Pull complete", ...) regardless of which field the running compose
// version put it in.
func phaseWord(ev composeEvent) string {
	if ev.Text != "" {
		return ev.Text
	}
	return ev.Status
}

// terminalWords are phase words that mark a resource (container,
// network, volume, image) as finished, across both schema variants.
var terminalWords = map[string]bool{
	"Created": true, "Started": true, "Healthy": true, "Exited": true,
	"Stopped": true, "Killed": true, "Removed": true, "Restarted": true,
	"Recreated": true, "Running": true, "Built": true, "Pulled": true,
	"Configured": true, "Copied": true, "Exported": true, "Committed": true,
	"Skipped": true, "Pull complete": true, "Already exists": true,
	"Download complete": true,
}

func isTerminal(ev composeEvent) bool {
	return ev.Percent == 100 || terminalWords[phaseWord(ev)]
}

// changeWords are up-phase phase words that mean compose actually did
// something to a container - as opposed to "Running"/"Healthy"/"Waiting"/
// "Exited", which mean it looked at the container and left it alone
// because nothing needed to change.
var changeWords = map[string]bool{
	"Creating": true, "Created": true,
	"Recreate": true, "Recreating": true, "Recreated": true,
	"Starting": true, "Started": true,
	"Restarting": true, "Restarted": true,
	"Stopping": true, "Stopped": true,
	"Killing": true, "Killed": true,
	"Removing": true, "Removed": true,
}

// --- plain-text fallback parsing ---
//
// COMPOSE_PROGRESS=json doesn't always take effect - either because the
// installed compose plugin predates it, or for other environment
// reasons we've observed in practice even on versions that should
// support it. When that happens, compose falls back to its classic
// human-readable output: a bracket progress bar for byte-level pull
// events, and plain "<id> <status>" lines otherwise. That format has
// been stable in Docker's pull/push rendering for years (it's
// pkg/jsonmessage's classic renderer), so it's a more reliable fallback
// than depending on the JSON flag working. Lines are converted into the
// same composeEvent shape so the aggregation logic above doesn't need
// to know which source produced them.
var (
	// ' <id> Downloading [====>    ]  1.2MB/5.5MB' or '... Extracting ...'
	plainBarRe = regexp.MustCompile(`^\s*(\S+)\s+(Downloading|Extracting)\s+\[[=>\s]*\]\s+(\S+)/(\S+)\s*$`)
	// ' Container <name>  <status>' (up phase)
	plainContainerRe = regexp.MustCompile(`^\s*Container\s+(\S+)\s+(.+?)\s*$`)
	// ' <id> <status words>' (pull phase, no byte data)
	plainIDWordRe = regexp.MustCompile(`^\s*(\S+)\s+(.+?)\s*$`)
	plainSizeRe   = regexp.MustCompile(`^([0-9.]+)\s*([kKmMgGtT]?i?[bB])$`)
)

// plainStatusWords gates plainIDWordRe, which would otherwise match
// almost any two-token line, to just docker's known pull phase words -
// avoiding misclassifying unrelated log output as a progress event.
var plainStatusWords = map[string]bool{
	"Pulling": true, "Pulled": true, "Pulling fs layer": true,
	"Waiting": true, "Verifying Checksum": true, "Download complete": true,
	"Pull complete": true, "Already exists": true, "Skipped": true,
}

// parseSize parses docker's classic size strings, e.g. "16.13MB" or
// "163.8kB" (decimal SI, as used by the pull progress bar). A "Ki/Mi/..."
// binary variant is accepted too in case some environment renders one,
// scaled by 1024 instead of 1000.
func parseSize(s string) (int64, bool) {
	m := plainSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	val, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	unit := strings.ToLower(m[2])
	step := 1000.0
	if strings.Contains(unit, "i") {
		step = 1024.0
	}
	mult := 1.0
	switch unit[0] {
	case 'k':
		mult = step
	case 'm':
		mult = step * step
	case 'g':
		mult = step * step * step
	case 't':
		mult = step * step * step * step
	}
	return int64(val * mult), true
}

// parsePlainLine tries to recognize one line of docker/compose's
// classic (non-JSON) progress output and turn it into a composeEvent
// equivalent to what --progress json would have sent.
func parsePlainLine(line string) (composeEvent, bool) {
	if m := plainBarRe.FindStringSubmatch(line); m != nil {
		current, okC := parseSize(m[3])
		total, okT := parseSize(m[4])
		if !okC || !okT || total <= 0 {
			return composeEvent{}, false
		}
		return composeEvent{
			ID:      m[1],
			Text:    m[2],
			Current: current,
			Total:   total,
			Percent: int(current * 100 / total),
		}, true
	}
	if m := plainContainerRe.FindStringSubmatch(line); m != nil {
		return composeEvent{ID: m[1], Text: strings.TrimSpace(m[2])}, true
	}
	if m := plainIDWordRe.FindStringSubmatch(line); m != nil {
		word := strings.TrimSpace(m[2])
		if !plainStatusWords[word] {
			return composeEvent{}, false
		}
		return composeEvent{ID: m[1], Text: word}, true
	}
	return composeEvent{}, false
}

// ProgressUpdate is broadcast over SSE so the UI can render a live bar.
type ProgressUpdate struct {
	Stack   string `json:"stack"`
	Phase   string `json:"phase"` // "pull" or "up"
	Text    string `json:"text"`
	Current int64  `json:"current"`
	Total   int64  `json:"total"`
	Percent int    `json:"percent"`
}

type layerProgress struct {
	current, total int64
}

// stackProgress aggregates raw compose events into a single bar for one
// stack during one phase (pull or up) of one run.
type stackProgress struct {
	mu      sync.Mutex
	phase   string
	layers  map[string]layerProgress // pull: byte progress per image layer
	seen    map[string]bool          // up: resource ids observed
	done    map[string]bool          // up: resource ids that reached terminal state
	changed bool                     // up: true once any changeWords event is seen
	last    ProgressUpdate
}

func newStackProgress(name, phase string) *stackProgress {
	text := "Starting"
	if phase == "pull" {
		text = "Pulling"
	}
	return &stackProgress{
		phase:  phase,
		layers: map[string]layerProgress{},
		seen:   map[string]bool{},
		done:   map[string]bool{},
		last:   ProgressUpdate{Stack: name, Phase: phase, Text: text},
	}
}

func (p *stackProgress) Last() ProgressUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// Changed reports whether this (up-phase) run actually did anything to
// a container, as opposed to finding everything already up to date.
func (p *stackProgress) Changed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// Apply folds one compose event into the running aggregate and returns
// the updated snapshot to broadcast.
func (p *stackProgress) Apply(stack string, ev composeEvent) ProgressUpdate {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch p.phase {
	case "pull":
		p.applyPullLocked(ev)
	case "up":
		p.applyUpLocked(ev)
	}
	p.last.Stack = stack
	p.last.Text = phaseWord(ev)
	return p.last
}

// applyPullLocked tracks byte progress per layer id. Any event carrying
// a nonzero Total is treated as a layer-progress unit, regardless of
// its ParentID - top-level "pulling this image" events never carry
// byte totals in either compose version, so this naturally excludes
// them without needing to parse the ParentID convention.
func (p *stackProgress) applyPullLocked(ev composeEvent) {
	if ev.ID != "" && (ev.Total > 0 || isTerminal(ev)) {
		lp := p.layers[ev.ID]
		if ev.Total > 0 {
			lp.current = ev.Current
			lp.total = ev.Total
		}
		if isTerminal(ev) && lp.total > 0 {
			lp.current = lp.total
		}
		p.layers[ev.ID] = lp
	}

	var current, total int64
	for _, lp := range p.layers {
		current += lp.current
		total += lp.total
	}
	p.last.Current, p.last.Total = current, total
	if total > 0 {
		p.last.Percent = int(current * 100 / total)
	}
}

func (p *stackProgress) applyUpLocked(ev composeEvent) {
	if ev.ID != "" {
		p.seen[ev.ID] = true
		if isTerminal(ev) {
			p.done[ev.ID] = true
		}
		if changeWords[phaseWord(ev)] {
			p.changed = true
		}
	}
	total := len(p.seen)
	done := len(p.done)
	p.last.Current, p.last.Total = int64(done), int64(total)
	if total > 0 {
		p.last.Percent = done * 100 / total
	}
}

// friendlyLine turns a compose event into a short human-readable log
// line, or reports show=false for events that are only useful for the
// progress bar: the many intermediate byte-count ticks of a single
// layer download, which would otherwise flood the console. Everything
// else - top-level events, terminal layer states, up-phase transitions,
// warnings/errors - is shown, since silently dropping anything we can't
// positively classify risks hiding a real failure.
func friendlyLine(phase string, ev composeEvent) (line string, show bool) {
	word := phaseWord(ev)
	if word == "" {
		return "", false
	}
	if ev.Total > 0 && !isTerminal(ev) {
		return "", false // mid-download byte tick, bar already shows this
	}

	label := ev.ID
	if phase == "pull" {
		switch {
		case ev.ParentID != "":
			img := strings.TrimPrefix(ev.ParentID, "Image ")
			id := ev.ID
			if len(id) > 12 {
				id = id[:12]
			}
			label = img + " " + id
		default:
			label = strings.TrimPrefix(ev.ID, "Image ")
		}
	}
	return fmt.Sprintf("%s: %s", label, word), true
}
