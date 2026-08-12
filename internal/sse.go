package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Event is one Server-Sent Event: Name becomes the `event:` field, Data
// is JSON-encoded into the `data:` field.
type Event struct {
	Name string
	Data any
}

// hub is a small pub/sub broadcaster for SSE subscribers. Subscribers
// that fall behind simply miss events rather than blocking publishers.
type hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newHub() *hub {
	return &hub{subs: map[chan Event]struct{}{}}
}

func (h *hub) subscribe() chan Event {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *hub) broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber too slow; drop the event rather than block.
		}
	}
}

// WriteSSE writes a single event in the text/event-stream wire format.
func WriteSSE(w http.ResponseWriter, ev Event) error {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	if ev.Name != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.Name); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
