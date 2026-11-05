package main

import "sync"

// busMsg is one chunk on the fan-out bus. The direction lets a consumer pick a
// side: the browser terminal only wants RX (the device echoes what it is sent,
// so echoing TX locally would double every character), a line tap wants both.
type busMsg struct {
	data []byte
	dir  byte
}

// bus fans the raw serial byte stream out to every byte-transport subscriber
// (raw TCP clients, tap clients, framed clients, the browser terminal). Each
// subscriber has its own bounded queue drained by its own goroutine, so one
// slow consumer can never stall the serial read loop - the defect the original
// bridge had, where Broadcast wrote to every socket inline.
type sub struct {
	id      int
	name    string
	ch      chan busMsg
	done    chan struct{}
	dropped int
	noted   bool
}

type bus struct {
	mu   sync.RWMutex
	subs map[int]*sub
	next int
}

func newBus() *bus { return &bus{subs: make(map[int]*sub)} }

func (b *bus) add(name string, capacity int) *sub {
	if capacity <= 0 {
		capacity = 1024
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	s := &sub{id: b.next, name: name, ch: make(chan busMsg, capacity), done: make(chan struct{})}
	b.subs[s.id] = s
	return s
}

func (b *bus) remove(s *sub) {
	b.mu.Lock()
	if cur, ok := b.subs[s.id]; ok && cur == s {
		delete(b.subs, s.id)
		close(s.done)
	}
	b.mu.Unlock()
}

// publish hands one copy of p to every subscriber except exclude. Delivery is
// non-blocking: a subscriber whose queue is full loses the chunk and has it
// counted, which is reported once per episode so a wedged reader is visible in
// the runtime log instead of silently degrading.
func (b *bus) publish(p []byte, dir byte, exclude int, onDrop func(*sub)) int {
	if len(p) == 0 {
		return 0
	}
	b.mu.RLock()
	subs := make([]*sub, 0, len(b.subs))
	for _, s := range b.subs {
		if s.id != exclude {
			subs = append(subs, s)
		}
	}
	b.mu.RUnlock()

	var shared []byte
	for _, s := range subs {
		select {
		case <-s.done:
			continue
		default:
		}
		if shared == nil {
			shared = append([]byte(nil), p...)
		}
		select {
		case s.ch <- busMsg{data: shared, dir: dir}:
			s.noted = false
		default:
			s.dropped++
			if !s.noted {
				s.noted = true
				if onDrop != nil {
					onDrop(s)
				}
			}
		}
	}
	return len(subs)
}

func (b *bus) count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// list returns a snapshot of subscriber names and drop counters.
func (b *bus) list() []map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]map[string]any, 0, len(b.subs))
	for _, s := range b.subs {
		out = append(out, map[string]any{"id": s.id, "name": s.name, "dropped": s.dropped, "queued": len(s.ch)})
	}
	return out
}
