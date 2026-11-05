// tcp_demo is a small console client for the serial-log bridge. It proves the
// sharing path from the consumer side: connect, watch the device output, and
// type lines to send them back through the bridge to the serial port.
//
// Two transports are supported:
//
//	raw    (default, port 2601) - plain byte stream, exactly what the wire sees
//	framed (-framed, port 2603) - length-prefixed records carrying direction
//	                              and arrival timestamp
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	rawPort     = 2601
	framedPort  = 2603
	frameHdrLen = 4 + 1 + 8
	dirRx       = 1
	dirTx       = 2
)

func main() {
	targetIP := flag.String("ip", "127.0.0.1", "bridge IP address")
	targetPort := flag.Int("P", 0, "bridge TCP port (default 2601 raw, 2603 framed)")
	logFile := flag.String("log", "", "optional log file name")
	framed := flag.Bool("framed", false, "use the timestamped framed protocol")
	flag.Parse()

	logger, closeLogger, err := newLogger(*logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLogger()

	port := *targetPort
	if port == 0 {
		if *framed {
			port = framedPort
		} else {
			port = rawPort
		}
	}
	if port <= 0 || port > 65535 {
		logger.Fatal("invalid TCP port, must be between 1 and 65535")
	}

	addr := net.JoinHostPort(*targetIP, strconv.Itoa(port))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		logger.Fatalf("failed to connect to %s: %v", addr, err)
	}
	defer conn.Close()

	mode := "raw"
	if *framed {
		mode = "framed"
	}
	logger.Printf("connected to %s (%s mode)", addr, mode)
	logger.Println("type a line and press Enter to send it to the device; /quit to exit")

	stop := make(chan struct{})
	errCh := make(chan error, 2)
	consoleMu := &sync.Mutex{}

	go func() { errCh <- socketReader(conn, *framed, consoleMu, stop) }()
	go func() { errCh <- stdinWriter(conn, *framed, logger, stop) }()

	err = <-errCh
	close(stop)
	if err != nil && !errors.Is(err, io.EOF) {
		logger.Printf("session ended: %v", err)
	}
}

func socketReader(conn net.Conn, framed bool, mu *sync.Mutex, stop <-chan struct{}) error {
	if framed {
		return framedReader(conn, mu, stop)
	}
	reader := bufio.NewReader(conn)
	buf := make([]byte, 4096)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		n, err := reader.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return fmt.Errorf("socket read error: %w", err)
		}
		if n == 0 {
			continue
		}
		text := strings.ReplaceAll(string(buf[:n]), "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")
		mu.Lock()
		fmt.Printf("[recv] %s", text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
		mu.Unlock()
	}
}

func framedReader(conn net.Conn, mu *sync.Mutex, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		var hdr [frameHdrLen]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return io.EOF
			}
			return fmt.Errorf("frame header: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[0:4])
		dir := hdr[4]
		ts := int64(binary.BigEndian.Uint64(hdr[5:13]))
		if n > 1<<20 {
			return fmt.Errorf("frame too large: %d", n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return fmt.Errorf("frame payload: %w", err)
		}
		mu.Lock()
		fmt.Printf("[%s] %-3s %5dB %q\n",
			time.UnixMilli(ts).Format("15:04:05.000"), dirName(dir), n,
			strings.TrimRight(string(payload), "\r\n"))
		mu.Unlock()
	}
}

func stdinWriter(conn net.Conn, framed bool, logger *log.Logger, stop <-chan struct{}) error {
	stdin := bufio.NewScanner(os.Stdin)
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		if !stdin.Scan() {
			if err := stdin.Err(); err != nil {
				return fmt.Errorf("stdin read error: %w", err)
			}
			return io.EOF
		}
		line := stdin.Text()
		if strings.EqualFold(strings.TrimSpace(line), "/quit") {
			return io.EOF
		}
		payload := []byte(line + "\r\n")
		var out []byte
		if framed {
			var hdr [frameHdrLen]byte
			binary.BigEndian.PutUint32(hdr[0:4], uint32(len(payload)))
			hdr[4] = dirTx
			binary.BigEndian.PutUint64(hdr[5:13], uint64(time.Now().UnixMilli()))
			out = append(hdr[:], payload...)
		} else {
			out = payload
		}
		if _, err := conn.Write(out); err != nil {
			return fmt.Errorf("socket write error: %w", err)
		}
		logger.Printf("sent %d bytes", len(payload))
	}
}

func dirName(dir byte) string {
	switch dir {
	case dirRx:
		return "RX"
	case dirTx:
		return "TX"
	default:
		return "SYS"
	}
}

func newLogger(path string) (*log.Logger, func(), error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stdout, "[tcp-demo] ", log.LstdFlags), func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	mw := io.MultiWriter(os.Stdout, f)
	return log.New(mw, "[tcp-demo] ", log.LstdFlags), func() { _ = f.Close() }, nil
}
