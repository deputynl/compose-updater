package internal

import "sync"

// Event is one broadcast update: Name identifies the kind ("stack",
// "progress", "log", "done"), Data is whatever payload goes with it.
// Transport-agnostic on purpose - handleWS in main.go is the only thing
// that knows these get marshaled as {"type": Name, "data": Data} over a
// WebSocket connection.
type Event struct {
	Name string
	Data any
}

// hub is a small pub/sub broadcaster. Subscribers that fall behind simply
// miss events rather than blocking publishers.
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
