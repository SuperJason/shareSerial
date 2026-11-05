package main

import "sync"

// Rec is one displayed log record.
type Rec struct {
	Seq  uint64 `json:"seq"`
	Ts   int64  `json:"ts"`  // unix milliseconds
	Dir  string `json:"dir"` // "rx" | "tx" | "sys"
	Src  string `json:"src"` // "local" | "tcp#3" | "system"
	N    int    `json:"n"`   // raw byte count of the chunk
	Text string `json:"text,omitempty"`
	// Hex carries the hex rendering when the payload is binary, i.e. when Text
	// is not a faithful representation of the bytes.
	Hex string `json:"hex,omitempty"`
	// Partial marks a chunk cut by the idle timer / maxLine instead of a
	// terminator.
	Partial bool `json:"partial,omitempty"`
}

func (r Rec) size() int64 {
	return int64(len(r.Text) + len(r.Hex) + 64)
}

// ring keeps the last N records in memory, bounded by count and by bytes.
// Readers stream with a sequence cursor, so a slow consumer never forces the
// writer to block: it just skips ahead once its position is evicted.
type ring struct {
	mu       sync.Mutex
	recs     []Rec
	head     int // index of the oldest record in recs
	seq      uint64
	bytes    int64
	maxRecs  int
	maxBytes int64
	dropped  uint64
}

func newRing(maxRecs int, maxBytes int64) *ring {
	if maxRecs <= 0 {
		maxRecs = 200000
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	return &ring{maxRecs: maxRecs, maxBytes: maxBytes}
}

// Append assigns the next sequence number, stores the record and evicts old
// records until both caps hold again.
func (r *ring) Append(rec Rec) Rec {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	rec.Seq = r.seq
	r.recs = append(r.recs, rec)
	r.bytes += rec.size()
	for len(r.recs)-r.head > 1 && (len(r.recs)-r.head > r.maxRecs || r.bytes > r.maxBytes) {
		r.bytes -= r.recs[r.head].size()
		r.recs[r.head] = Rec{}
		r.head++
		r.dropped++
	}
	// Compact once the dead prefix dominates the slice.
	if r.head > 4096 && r.head*2 > len(r.recs) {
		live := len(r.recs) - r.head
		copy(r.recs, r.recs[r.head:])
		r.recs = r.recs[:live]
		r.head = 0
	}
	return rec
}

// Tail returns the last n retained records and their sequence cursor.
func (r *ring) Tail(n int) ([]Rec, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > 5000 {
		n = 2000
	}
	live := r.recs[r.head:]
	if len(live) == 0 {
		return nil, r.seq
	}
	from := 0
	if len(live) > n {
		from = len(live) - n
	}
	out := make([]Rec, len(live)-from)
	copy(out, live[from:])
	return out, r.seq
}

// Since returns up to limit records with Seq greater than after, plus the
// sequence number of the last returned record (monotonic cursor).
func (r *ring) Since(after uint64, limit int) ([]Rec, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	live := r.recs[r.head:]
	// Binary search for the first record with Seq > after.
	lo, hi := 0, len(live)
	for lo < hi {
		mid := (lo + hi) / 2
		if live[mid].Seq <= after {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo >= len(live) {
		return nil, after
	}
	end := lo + limit
	if end > len(live) {
		end = len(live)
	}
	out := make([]Rec, end-lo)
	copy(out, live[lo:end])
	return out, out[len(out)-1].Seq
}

// Oldest returns the sequence number of the oldest retained record, which lets
// a client detect that it fell behind and history was evicted.
func (r *ring) Oldest() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recs) == r.head {
		return r.seq + 1
	}
	return r.recs[r.head].Seq
}

func (r *ring) Last() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Clear drops every record. Sequence numbers keep increasing so existing
// cursors stay meaningful (they simply see nothing until new records arrive).
func (r *ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = nil
	r.head = 0
	r.bytes = 0
}

func (r *ring) Stats() (count int, bytes int64, total uint64, dropped uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recs) - r.head, r.bytes, r.seq, r.dropped
}
