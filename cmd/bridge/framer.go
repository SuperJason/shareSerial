package main

import "time"

// chunk is one unit handed to the log pipeline. Data is the raw wire bytes,
// terminator included; the display layer strips the terminator.
type chunk struct {
	Data []byte
	// Complete reports whether a line terminator ended the chunk. false means
	// the chunk was cut by the idle timer or by maxLine, i.e. the device
	// paused or bursted without a terminator.
	Complete bool
}

// framer splits a byte stream into line records.
//
// A line ends at '\n', at a bare '\r' (many MCU consoles use CR only), or when
// the caller reports idle time / the buffer hits maxLine. '\r\n' is one line,
// not two: a trailing '\r' is held until the next byte decides which it is.
type framer struct {
	buf     []byte
	maxLine int
	idle    time.Duration
	lastAt  time.Time
}

func newFramer(maxLine int, idle time.Duration) *framer {
	if maxLine <= 0 {
		maxLine = 8192
	}
	return &framer{maxLine: maxLine, idle: idle, lastAt: time.Now()}
}

// Push appends data and returns every line it completed.
func (f *framer) Push(data []byte) []chunk {
	if len(data) == 0 {
		return nil
	}
	f.buf = append(f.buf, data...)
	f.lastAt = time.Now()
	return f.scan(false)
}

// FlushIdle returns the pending partial line once the stream has been silent
// for the idle window. It returns nil when there is nothing to flush yet.
func (f *framer) FlushIdle(now time.Time) []chunk {
	if len(f.buf) == 0 || f.idle <= 0 {
		return nil
	}
	if now.Sub(f.lastAt) < f.idle {
		return nil
	}
	f.lastAt = now
	return f.scan(true)
}

// Flush emits whatever is buffered, regardless of idle time (port closing).
func (f *framer) Flush() []chunk {
	if len(f.buf) == 0 {
		return nil
	}
	return f.scan(true)
}

// scan walks the buffer and cuts it into chunks. When force is false a trailing
// '\r' is left in the buffer because it may be the first half of '\r\n'.
func (f *framer) scan(force bool) []chunk {
	var out []chunk
	start := 0
	i := 0
	for i < len(f.buf) {
		b := f.buf[i]
		if b == '\n' {
			out = append(out, chunk{Data: f.buf[start : i+1], Complete: true})
			start = i + 1
			i++
			continue
		}
		if b == '\r' {
			atEnd := i+1 == len(f.buf)
			if atEnd && !force {
				break // wait for the next byte to tell CRLF from CR
			}
			if !atEnd && f.buf[i+1] == '\n' {
				i++ // '\r' of a CRLF pair: the '\n' branch cuts the line
				continue
			}
			out = append(out, chunk{Data: f.buf[start : i+1], Complete: true})
			start = i + 1
			i++
			continue
		}
		i++
	}
	if start > 0 {
		// Copy the remainder into a fresh buffer: the chunks returned above are
		// slices of the old array, so shifting in place would rewrite them.
		rest := make([]byte, len(f.buf)-start)
		copy(rest, f.buf[start:])
		f.buf = rest
	}
	if force && len(f.buf) > 0 {
		out = append(out, chunk{Data: f.buf, Complete: false})
		f.buf = nil
	}
	// Bound memory on binary/bursty streams: cut over-long runs into pieces.
	for len(f.buf) >= f.maxLine {
		piece := make([]byte, f.maxLine)
		copy(piece, f.buf[:f.maxLine])
		out = append(out, chunk{Data: piece, Complete: false})
		f.buf = append(f.buf[:0], f.buf[f.maxLine:]...)
	}
	return out
}
