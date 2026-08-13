package main

import (
	"bufio"
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
)

const defaultTargetPort = 2601

func main() {
	targetIP := flag.String("ip", "127.0.0.1", "Target bridge IP address")
	targetPort := flag.Int("P", defaultTargetPort, "Target bridge TCP port")
	logFile := flag.String("log", "", "Optional log file name")
	flag.Parse()

	logger, closeLogger, err := newLogger(*logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLogger()

	if *targetPort <= 0 || *targetPort > 65535 {
		logger.Fatal("invalid TCP port, must be between 1 and 65535")
	}

	addr := net.JoinHostPort(*targetIP, strconv.Itoa(*targetPort))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		logger.Fatalf("failed to connect to %s: %v", addr, err)
	}
	defer conn.Close()

	logger.Printf("connected to bridge: %s", addr)
	handleSession(conn, logger)
}

func handleSession(conn net.Conn, logger *log.Logger) {
	defer func() {
		logger.Printf("client disconnected: %s", conn.RemoteAddr())
		_ = conn.Close()
	}()

	logger.Println("interactive mode: type text and press Enter to send; type /quit to end this client session")

	stop := make(chan struct{})
	errCh := make(chan error, 2)
	consoleMu := &sync.Mutex{}

	go socketReader(conn, consoleMu, stop, errCh)
	go stdinWriter(conn, logger, consoleMu, stop, errCh)

	err := <-errCh
	close(stop)

	if errors.Is(err, io.EOF) {
		return
	}
	if err != nil {
		logger.Printf("session ended: %v", err)
	}
}

func socketReader(conn net.Conn, consoleMu *sync.Mutex, stop <-chan struct{}, errCh chan<- error) {
	reader := bufio.NewReader(conn)
	buf := make([]byte, 4096)

	for {
		select {
		case <-stop:
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				errCh <- io.EOF
				return
			}
			errCh <- fmt.Errorf("socket read error: %w", err)
			return
		}

		if n == 0 {
			continue
		}

		text := strings.ReplaceAll(string(buf[:n]), "\r\n", "\n")
		text = strings.ReplaceAll(text, "\r", "\n")

		consoleMu.Lock()
		fmt.Printf("[recv] %s", text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
		consoleMu.Unlock()
	}
}

func stdinWriter(conn net.Conn, logger *log.Logger, consoleMu *sync.Mutex, stop <-chan struct{}, errCh chan<- error) {
	stdin := bufio.NewScanner(os.Stdin)

	for {
		select {
		case <-stop:
			return
		default:
		}

		consoleMu.Lock()
		//fmt.Print("[send] ")
		consoleMu.Unlock()

		if !stdin.Scan() {
			if err := stdin.Err(); err != nil {
				errCh <- fmt.Errorf("stdin read error: %w", err)
				return
			}
			errCh <- io.EOF
			return
		}

		line := stdin.Text()
		if strings.EqualFold(strings.TrimSpace(line), "/quit") {
			errCh <- io.EOF
			return
		}

		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			errCh <- fmt.Errorf("socket write error: %w", err)
			return
		}

		//logger.Printf("sent %d bytes to %s", len(line)+1, conn.RemoteAddr())
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
	logger := log.New(mw, "[tcp-demo] ", log.LstdFlags)
	return logger, func() { _ = f.Close() }, nil
}
