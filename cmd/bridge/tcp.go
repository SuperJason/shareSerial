package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const clientWriteWait = 10 * time.Second

// connSet tracks live client connections so shutdown can close them.
type connSet struct {
	mu sync.Mutex
	m  map[net.Conn]struct{}
}

func newConnSet() *connSet { return &connSet{m: make(map[net.Conn]struct{})} }

func (c *connSet) add(n net.Conn) {
	c.mu.Lock()
	c.m[n] = struct{}{}
	c.mu.Unlock()
}

func (c *connSet) remove(n net.Conn) {
	c.mu.Lock()
	delete(c.m, n)
	c.mu.Unlock()
}

func (c *connSet) closeAll() {
	c.mu.Lock()
	conns := make([]net.Conn, 0, len(c.m))
	for n := range c.m {
		conns = append(conns, n)
	}
	c.mu.Unlock()
	for _, n := range conns {
		n.Close()
	}
}

// transports owns the three TCP listeners.
//
//	port 2601 raw     - duplex byte stream, write access per write_policy
//	port 2602 tap     - read-only mirror of everything on the wire (RX + TX)
//	port 2603 framed  - timestamped length-prefixed frames in both directions
type transports struct {
	app   *App
	rtlog *log.Logger
	conns *connSet

	rawLn   net.Listener
	tapLn   net.Listener
	frameLn net.Listener

	ownerMu sync.Mutex
	owner   int // subscriber id holding "first" write ownership, 0 = free

	closed atomic.Bool
	wg     sync.WaitGroup
}

func newTransports(a *App, rtlog *log.Logger) *transports {
	return &transports{app: a, rtlog: rtlog, conns: newConnSet()}
}

// canWrite applies the configured write policy to a subscriber.
func (t *transports) canWrite(subID int) (bool, string) {
	policy := t.app.Config().WritePolicy
	switch policy {
	case "all":
		return true, ""
	case "readonly":
		return false, "policy is readonly"
	default: // "first"
		t.ownerMu.Lock()
		defer t.ownerMu.Unlock()
		if t.owner == 0 {
			t.owner = subID
			return true, ""
		}
		if t.owner == subID {
			return true, ""
		}
		return false, fmt.Sprintf("write side is owned by tcp#%d", t.owner)
	}
}

func (t *transports) releaseWrite(subID int) {
	t.ownerMu.Lock()
	if t.owner == subID {
		t.owner = 0
	}
	t.ownerMu.Unlock()
}

func (t *transports) writeOwner() int {
	t.ownerMu.Lock()
	defer t.ownerMu.Unlock()
	return t.owner
}

// start brings up the listeners that are configured. The primary port is
// required, the other two are best-effort (a busy port must not kill the app).
func (t *transports) start(cfg Config) error {
	if cfg.TargetPort > 0 {
		ln, err := listen(cfg.TargetIP, cfg.TargetPort)
		if err != nil {
			return fmt.Errorf("raw port %s: %w", addrOf(cfg.TargetPort), err)
		}
		t.rawLn = ln
		t.rtlog.Printf("raw TCP listening on %s (write policy: %s)", ln.Addr(), cfg.WritePolicy)
		t.wg.Add(1)
		go t.acceptRaw(ln)
	}
	if cfg.TapPort > 0 {
		if ln, err := listen("", cfg.TapPort); err == nil {
			t.tapLn = ln
			t.rtlog.Printf("tap TCP listening on %s (read-only mirror)", ln.Addr())
			t.wg.Add(1)
			go t.acceptTap(ln)
		} else {
			t.rtlog.Printf("tap port %d unavailable: %v", cfg.TapPort, err)
		}
	}
	if cfg.FramePort > 0 {
		if ln, err := listen("", cfg.FramePort); err == nil {
			t.frameLn = ln
			t.rtlog.Printf("framed TCP listening on %s", ln.Addr())
			t.wg.Add(1)
			go t.acceptFramed(ln)
		} else {
			t.rtlog.Printf("frame port %d unavailable: %v", cfg.FramePort, err)
		}
	}
	return nil
}

func listen(host string, port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
}

func addrOf(port int) string { return fmt.Sprintf(":%d", port) }

func (t *transports) stop() {
	if t.closed.Swap(true) {
		return
	}
	for _, ln := range []net.Listener{t.rawLn, t.tapLn, t.frameLn} {
		if ln != nil {
			ln.Close()
		}
	}
	t.conns.closeAll()
	t.wg.Wait()
}

// --- raw duplex ------------------------------------------------------------

func (t *transports) acceptRaw(ln net.Listener) {
	defer t.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if t.closed.Load() {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s := t.app.Bus().add("raw", 4096)
		t.conns.add(conn)
		t.rtlog.Printf("raw client tcp#%d connected from %s (total=%d)", s.id, conn.RemoteAddr(), t.app.Bus().count())
		t.wg.Add(2)
		go func() {
			defer t.wg.Done()
			t.rawWriter(conn, s)
		}()
		go func() {
			defer t.wg.Done()
			t.rawReader(conn, s)
		}()
	}
}

func (t *transports) rawWriter(conn net.Conn, s *sub) {
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.ch:
			conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
			if _, err := conn.Write(msg.data); err != nil {
				t.rtlog.Printf("raw client tcp#%d write failed: %v", s.id, err)
				return
			}
		}
	}
}

func (t *transports) rawReader(conn net.Conn, s *sub) {
	defer func() {
		t.app.Bus().remove(s)
		t.releaseWrite(s.id)
		t.conns.remove(conn)
		conn.Close()
		t.rtlog.Printf("raw client tcp#%d disconnected (total=%d)", s.id, t.app.Bus().count())
	}()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if ok, why := t.canWrite(s.id); !ok {
				t.rtlog.Printf("raw client tcp#%d write denied: %s", s.id, why)
			} else if werr := t.app.Send(buf[:n], fmt.Sprintf("tcp#%d", s.id), s.id); werr != nil {
				t.rtlog.Printf("raw client tcp#%d -> serial failed: %v", s.id, werr)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !t.closed.Load() {
				t.rtlog.Printf("raw client tcp#%d read error: %v", s.id, err)
			}
			return
		}
	}
}

// --- read-only tap ---------------------------------------------------------

func (t *transports) acceptTap(ln net.Listener) {
	defer t.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if t.closed.Load() {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s := t.app.Bus().add("tap", 4096)
		t.conns.add(conn)
		t.rtlog.Printf("tap client tcp#%d connected from %s", s.id, conn.RemoteAddr())
		t.wg.Add(2)
		go func() {
			defer t.wg.Done()
			t.rawWriter(conn, s)
		}()
		go func() {
			// Drain reads so a half-closed socket is detected; tap clients
			// cannot write to the device.
			defer t.wg.Done()
			defer func() {
				t.app.Bus().remove(s)
				t.conns.remove(conn)
				conn.Close()
				t.rtlog.Printf("tap client tcp#%d disconnected", s.id)
			}()
			io.Copy(io.Discard, conn)
		}()
	}
}

// --- framed ----------------------------------------------------------------

// acceptFramed serves the timestamped protocol. Outbound frames are produced by
// a per-connection framer so a client receives exactly the same line split the
// log window shows, plus the arrival timestamp.
func (t *transports) acceptFramed(ln net.Listener) {
	defer t.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if t.closed.Load() {
				return
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		s := t.app.Bus().add("framed", 4096)
		t.conns.add(conn)
		t.rtlog.Printf("framed client tcp#%d connected from %s", s.id, conn.RemoteAddr())
		t.wg.Add(2)
		go func() {
			defer t.wg.Done()
			t.framedWriter(conn, s)
		}()
		go func() {
			defer t.wg.Done()
			t.framedReader(conn, s)
		}()
	}
}

func (t *transports) framedWriter(conn net.Conn, s *sub) {
	fr := newFramer(t.app.Config().MaxLineBytes, time.Duration(t.app.Config().LineIdleMs)*time.Millisecond)
	greeting := fmt.Sprintf("connected: %s", t.statusLine())
	conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
	conn.Write(appendFrame(nil, dirSys, time.Now().UnixMilli(), []byte(greeting)))

	send := func(ch chunk) bool {
		f := appendFrame(nil, dirRx, time.Now().UnixMilli(), ch.Data)
		conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
		_, err := conn.Write(f)
		return err == nil
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.ch:
			for _, ch := range fr.Push(msg.data) {
				if !send(ch) {
					return
				}
			}
		case now := <-tick.C:
			for _, ch := range fr.FlushIdle(now) {
				if !send(ch) {
					return
				}
			}
		}
	}
}

func (t *transports) framedReader(conn net.Conn, s *sub) {
	defer func() {
		t.app.Bus().remove(s)
		t.releaseWrite(s.id)
		t.conns.remove(conn)
		conn.Close()
		t.rtlog.Printf("framed client tcp#%d disconnected", s.id)
	}()
	for {
		f, err := readFrame(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !t.closed.Load() {
				t.rtlog.Printf("framed client tcp#%d frame error: %v", s.id, err)
			}
			return
		}
		if len(f.payload) == 0 {
			continue
		}
		if ok, why := t.canWrite(s.id); !ok {
			t.rtlog.Printf("framed client tcp#%d write denied: %s", s.id, why)
			continue
		}
		if werr := t.app.Send(f.payload, fmt.Sprintf("tcp#%d", s.id), s.id); werr != nil {
			t.rtlog.Printf("framed client tcp#%d -> serial failed: %v", s.id, werr)
		}
	}
}

func (t *transports) statusLine() string {
	st := t.app.State()
	port, _ := st["port"].(string)
	baud := st["baud"]
	if port == "" {
		return "no port open"
	}
	return fmt.Sprintf("%s @ %v", strings.TrimSpace(port), baud)
}
