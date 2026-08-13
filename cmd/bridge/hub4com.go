package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

const (
	hubStartupTimeout = 15 * time.Second
	hubReadyPoll      = 300 * time.Millisecond
	hubRestartBackoff = 2 * time.Second
	hubBackoffMax     = 10 * time.Second
)

// hub4comManager spawns and supervises a hub4com.exe child process that
// bridges a physical COM port to one end of a com0com virtual pair. The
// bridge itself opens the other end of the pair, so the physical port is
// only ever opened by hub4com (exclusive), while the bridge serves any
// number of TCP sessions. This lets one bridge process manage the whole
// COM22 -> LAN chain: start, crash-restart, and cleanup are automatic.
type hub4comManager struct {
	exePath      string
	physicalPort string // real COM port (e.g. COM22)
	slavePort    string // com0com pair end hub4com attaches to (e.g. COM24)
	bridgePort   string // pair peer the bridge opens (e.g. COM23)
	extraSlaves  []string // additional hub4com slave ports for other consumers (e.g. local serial monitor)
	baud         int
	logFile      string
	logger       *log.Logger

	// onReady is called after hub4com (re)started and the slave port is
	// held by it. It must close and re-open the bridge serial port.
	onReady func() error

	mu       sync.Mutex
	cmd      *exec.Cmd
	stopped  bool
	logF     *os.File
	readyCh  chan error // first readiness result (buffered)
	restartCh chan struct{}
	wg       sync.WaitGroup
}

func newHub4comManager(exePath, physicalPort, slavePort, bridgePort string, extraSlaves []string, baud int, logFile string, logger *log.Logger) *hub4comManager {
	return &hub4comManager{
		exePath:      exePath,
		physicalPort: physicalPort,
		slavePort:    slavePort,
		bridgePort:   bridgePort,
		extraSlaves:  extraSlaves,
		baud:         baud,
		logFile:      logFile,
		logger:       logger,
		readyCh:      make(chan error, 1),
		restartCh:    make(chan struct{}, 1),
	}
}

// peerPortName returns the com0com pair peer for a port name:
// COM23 -> COM24, COM25 -> COM26, CNCA0 -> CNCB0, CNCB2 -> CNCA2.
// Returns "" when the peer cannot be derived.
func peerPortName(port string) string {
	port = strings.ToUpper(strings.TrimSpace(port))
	if strings.HasPrefix(port, "CNCA") {
		return "CNCB" + port[4:]
	}
	if strings.HasPrefix(port, "CNCB") {
		return "CNCA" + port[4:]
	}
	if strings.HasPrefix(port, "COM") {
		if n, err := strconv.Atoi(port[3:]); err == nil {
			return fmt.Sprintf("COM%d", n+1)
		}
	}
	return ""
}

func devicePath(port string) string {
	if runtime.GOOS != "windows" {
		return port
	}
	upper := strings.ToUpper(strings.TrimSpace(port))
	if strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "CNC") {
		return `\\.\` + upper
	}
	return port
}

// probeSlaveBusy reports whether some process (hub4com) currently holds the
// port exclusively. hub4com opens its ports without sharing, so "cannot
// open" is exactly the signal that hub4com has taken the slave port.
func probeSlaveBusy(port string) bool {
	p, err := serial.Open(port, &serial.Mode{BaudRate: 115200})
	if err != nil {
		return true
	}
	_ = p.Close()
	return false
}

// Start launches the supervisor goroutine. The first readiness result is
// delivered on Ready().
func (m *hub4comManager) Start() error {
	if runtime.GOOS != "windows" {
		return errors.New("hub4com mode is only supported on Windows")
	}
	if strings.TrimSpace(m.exePath) == "" {
		return errors.New("hub4com.exe path is empty")
	}
	if _, err := os.Stat(m.exePath); err != nil {
		return fmt.Errorf("hub4com.exe not found at %s: %v", m.exePath, err)
	}
	if strings.TrimSpace(m.physicalPort) == "" {
		return errors.New("hub physical port (hub_physical_port) is required in hub4com mode")
	}
	if strings.TrimSpace(m.slavePort) == "" {
		return errors.New("hub slave port (hub_slave_port) is required in hub4com mode")
	}
	if strings.EqualFold(m.physicalPort, m.slavePort) || strings.EqualFold(m.slavePort, m.bridgePort) ||
		strings.EqualFold(m.physicalPort, m.bridgePort) {
		return errors.New("hub physical/slave/bridge ports must be three different ports")
	}
	seen := map[string]bool{strings.ToUpper(m.physicalPort): true, strings.ToUpper(m.slavePort): true, strings.ToUpper(m.bridgePort): true}
	for _, s := range m.extraSlaves {
		up := strings.ToUpper(s)
		if seen[up] {
			return fmt.Errorf("hub extra slave port %s duplicates another hub port", s)
		}
		seen[up] = true
	}

	if strings.TrimSpace(m.logFile) != "" {
		f, err := os.OpenFile(m.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			m.logger.Printf("warning: cannot open hub4com log %s: %v", m.logFile, err)
		} else {
			m.logF = f
		}
	}

	m.mu.Lock()
	m.stopped = false
	m.mu.Unlock()

	m.wg.Add(1)
	go m.supervise()
	return nil
}

// Ready returns the first readiness result channel. nil error means the
// bridge serial port is open and serving.
func (m *hub4comManager) Ready() <-chan error {
	return m.readyCh
}

func (m *hub4comManager) isStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

func (m *hub4comManager) currentBridgePort() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bridgePort
}

func (m *hub4comManager) currentBaud() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baud
}

func (m *hub4comManager) output() io.Writer {
	if m.logF != nil {
		return m.logF
	}
	return io.Discard
}

func (m *hub4comManager) supervise() {
	defer m.wg.Done()
	backoff := hubRestartBackoff

	for {
		if m.isStopped() {
			return
		}

		args := []string{"--octs=off", fmt.Sprintf("--baud=%d", m.baud)}
		// Explicit routes: physical port (0) broadcasts to every slave, each
		// slave routes back to the physical port only (consumers stay isolated).
		// Specifying any --route disables hub4com's built-in defaults, so the
		// full set must be provided.
		args = append(args, "--route=0:All")
		for i := 1; i < 2+len(m.extraSlaves); i++ {
			args = append(args, fmt.Sprintf("--route=%d:0", i))
		}
		args = append(args, devicePath(m.physicalPort), devicePath(m.slavePort))
		for _, s := range m.extraSlaves {
			args = append(args, devicePath(s))
		}

		cmd := exec.Command(m.exePath, args...)
		cmd.Stdout = m.output()
		cmd.Stderr = m.output()

		m.mu.Lock()
		m.cmd = cmd
		m.mu.Unlock()

		m.logger.Printf("hub4com: spawning %s %s", m.exePath, strings.Join(args, " "))
		if err := cmd.Start(); err != nil {
			m.logger.Printf("hub4com: start failed: %v", err)
			m.signalReady(fmt.Errorf("hub4com start failed: %v", err))
			if !m.sleepOrStop(backoff) {
				return
			}
			backoff = min(backoff*2, hubBackoffMax)
			continue
		}
		pid := cmd.Process.Pid
		m.logger.Printf("hub4com: started pid=%d", pid)

		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()

		ready, exited, waitErr := m.waitReady(waitDone)
		if ready {
			backoff = hubRestartBackoff
			// The com0com pair may need a moment to settle after a
			// hub4com (re)start; retry the bridge serial open.
			var readyErr error
			for attempt := 0; attempt < 10; attempt++ {
				if err := m.onReady(); err == nil {
					readyErr = nil
					break
				} else {
					readyErr = err
					time.Sleep(500 * time.Millisecond)
				}
			}
			if readyErr != nil {
				m.logger.Printf("hub4com: bridge serial open failed after ready: %v", readyErr)
				m.signalReady(fmt.Errorf("open %s failed: %v", m.bridgePort, readyErr))
			} else {
				m.logger.Printf("hub4com: ready, bridge serial=%s @ %d", m.bridgePort, m.baud)
				m.signalReady(nil)
			}
		} else {
			m.signalReady(fmt.Errorf("hub4com not ready: %v", waitErr))
		}

		if !exited {
			select {
			case waitErr = <-waitDone:
				// hub4com exited by itself
			case <-m.restartCh:
				m.logger.Printf("hub4com: restart requested, killing pid=%d", pid)
				_ = cmd.Process.Kill()
				waitErr = <-waitDone
			}
		}

		m.mu.Lock()
		m.cmd = nil
		m.mu.Unlock()

		if m.isStopped() {
			m.logger.Printf("hub4com: stopped pid=%d", pid)
			return
		}
		m.logger.Printf("hub4com: exited pid=%d err=%v, restarting in %s", pid, waitErr, backoff)
		if !m.sleepOrStop(backoff) {
			return
		}
		backoff = min(backoff*2, hubBackoffMax)
	}
}

// waitReady polls until hub4com holds the slave port (exclusive open) or the
// process exits / the timeout elapses. It consumes from waitDone only when
// hub4com is observed to have exited, and reports that via `exited` so the
// supervisor does not block on an already-drained channel.
func (m *hub4comManager) waitReady(waitDone <-chan error) (ready bool, exited bool, err error) {
	deadline := time.Now().Add(hubStartupTimeout)
	for time.Now().Before(deadline) {
		select {
		case waitErr := <-waitDone:
			return false, true, fmt.Errorf("hub4com exited early: %v (see %s)", waitErr, m.logFile)
		default:
		}
		if probeSlaveBusy(m.slavePort) {
			return true, false, nil
		}
		time.Sleep(hubReadyPoll)
	}
	select {
	case waitErr := <-waitDone:
		return false, true, fmt.Errorf("hub4com exited early: %v (see %s)", waitErr, m.logFile)
	default:
	}
	return false, false, errors.New("timeout waiting for hub4com to take " + m.slavePort)
}

func (m *hub4comManager) signalReady(err error) {
	select {
	case m.readyCh <- err:
	default:
	}
}

// RestartPhysical switches the physical port (menu: select COM port) and
// forces hub4com to restart with it.
func (m *hub4comManager) RestartPhysical(newPort string) {
	m.mu.Lock()
	m.physicalPort = newPort
	cmd := m.cmd
	m.mu.Unlock()
	m.logger.Printf("hub4com: switching physical port to %s", newPort)
	if cmd != nil {
		_ = cmd.Process.Kill()
		return
	}
	select {
	case m.restartCh <- struct{}{}:
	default:
	}
}

// SetBaud changes the baud rate used for hub4com and forces a restart.
func (m *hub4comManager) SetBaud(baud int) {
	m.mu.Lock()
	m.baud = baud
	cmd := m.cmd
	m.mu.Unlock()
	m.logger.Printf("hub4com: switching baud to %d", baud)
	if cmd != nil {
		_ = cmd.Process.Kill()
		return
	}
	select {
	case m.restartCh <- struct{}{}:
	default:
	}
}

func (m *hub4comManager) sleepOrStop(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-m.restartCh:
		return true
	}
}

// Stop kills the hub4com child (if any) and closes the log file.
func (m *hub4comManager) Stop() {
	m.mu.Lock()
	m.stopped = true
	cmd := m.cmd
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		m.logger.Printf("hub4com: stopping pid=%d", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	if m.logF != nil {
		_ = m.logF.Close()
		m.logF = nil
	}
}
