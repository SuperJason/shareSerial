package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	logRotateBytes = 64 << 20
	flushInterval  = 200 * time.Millisecond
)

// appendFile is a buffered appender; it is never nil-checked by callers.
type appendFile struct {
	f    *os.File
	buf  []byte
	path string
	size int64
}

func openAppend(path string) (*appendFile, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &appendFile{f: f, path: path, buf: make([]byte, 0, 32<<10), size: st.Size()}, nil
}

func (b *appendFile) write(p []byte) {
	if b == nil || b.f == nil {
		return
	}
	b.buf = append(b.buf, p...)
	b.size += int64(len(p))
	if len(b.buf) >= 32<<10 {
		b.flush()
	}
}

func (b *appendFile) flush() {
	if b == nil || b.f == nil || len(b.buf) == 0 {
		return
	}
	b.f.Write(b.buf)
	b.buf = b.buf[:0]
}

func (b *appendFile) close() {
	if b == nil || b.f == nil {
		return
	}
	b.flush()
	b.f.Sync()
	b.f.Close()
	b.f = nil
}

// capture is the optional on-disk text log. It is off by default: a monitoring
// session should not silently fill the disk. Only text is written - the
// timestamped, human-readable line log - never raw binary (that is what the
// framed TCP port is for).
type capture struct {
	mu      sync.Mutex
	file    *appendFile
	path    string
	dir     string
	prefix  string
	stop    chan struct{}
	running bool
}

func newCapture() *capture { return &capture{} }

// Start opens a new capture file named <prefix>_YYYYmmdd_HHMMSS.log inside dir, so
// captures from different ports never look alike.
func (c *capture) Start(dir, prefix string) (string, error) {
	if prefix == "" {
		prefix = "serial"
	}
	if dir == "" {
		dir = defaultCaptureDir()
	}
	full := filepath.Join(dir, fmt.Sprintf("%s_%s.log", prefix, time.Now().Format("20060102_150405")))
	f, err := openAppend(full)
	if err != nil {
		return "", err
	}

	c.Stop()

	c.mu.Lock()
	c.file = f
	c.path = full
	c.dir = dir
	c.prefix = prefix
	c.running = true
	c.stop = make(chan struct{})
	stop := c.stop
	c.mu.Unlock()

	go c.flushLoop(stop)
	return full, nil
}

// Stop closes the current file and returns its path ("" when it was not on).
func (c *capture) Stop() string {
	c.mu.Lock()
	f := c.file
	path := c.path
	stop := c.stop
	c.file = nil
	c.path = ""
	c.running = false
	c.stop = nil
	c.mu.Unlock()

	if stop != nil {
		close(stop)
	}
	if f != nil {
		f.close()
	}
	return path
}

func (c *capture) flushLoop(stop chan struct{}) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.mu.Lock()
			c.file.flush()
			c.mu.Unlock()
		}
	}
}

func (c *capture) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *capture) Info() (enabled bool, path, dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running, c.path, c.dir
}

// Write appends one timestamped line. It is a no-op when capture is off.
func (c *capture) Write(rec Rec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running || c.file == nil {
		return
	}
	c.file.write([]byte(fmt.Sprintf("%s %-3s %-10s %s\n",
		time.UnixMilli(rec.Ts).Format("2006-01-02 15:04:05.000"),
		dirLabel(rec.Dir), rec.Src, rec.Text)))
	if c.file.size > logRotateBytes {
		c.rotateLocked()
	}
}

// rotateLocked starts a fresh file so one long session cannot grow unbounded.
func (c *capture) rotateLocked() {
	dir, prefix := c.dir, c.prefix
	c.file.close()
	full := filepath.Join(dir, fmt.Sprintf("%s_%s.log", prefix, time.Now().Format("20060102_150405")))
	if f, err := openAppend(full); err == nil {
		c.file = f
		c.path = full
	}
}

// defaultCaptureDir is %USERPROFILE%\Documents\serial-log.
func defaultCaptureDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "serial-log"
	}
	return filepath.Join(home, "Documents", "serial-log")
}
