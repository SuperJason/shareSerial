package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

// This file owns the CBUS pins that gate each DUT's VBUS on the tool boards.
//
// The constraint that shapes everything here: D2XX cannot open a device that
// another handle already holds, so while hub4com has an FT230X's COM port
// open, its CBUS pin is unreachable. The pin can therefore only be driven
// inside a window where that serial port is closed.
//
// That window is provided by hub4com's own restart machinery. kickRestart()
// kills the child, and the supervisor then waits hubRestartBackoff (2s) before
// respawning - so the port is free and known to stay free for about two
// seconds. The pin work takes on the order of milliseconds, which leaves a
// large margin. hub4com restarts by itself afterwards; nothing here has to
// bring it back.
//
// The cost, accepted deliberately: every VBUS operation interrupts the serial
// bridge for roughly the length of a hub4com restart. TCP clients disconnect
// and must reconnect, the extra slave monitor port drops, and in-flight bytes
// are lost. That is the price of toggling VBUS without changing the hardware
// or the existing port configuration.
//
// Multiple boards are supported: every usable FTDI device enumerated by D2XX
// is a board, each with its own pin/polarity settings and its own VBUS state.

const (
	// ftdiOpTimeout bounds the whole open/act/close sequence. It must stay
	// comfortably under hubRestartBackoff, or hub4com would respawn and take
	// the port back while we are still working.
	ftdiOpTimeout = 2 * time.Second
	ftdiFreePoll  = 25 * time.Millisecond

	// ftdiDetectTTL is how long a device enumeration stays fresh. The tray and
	// the Web UI both poll faster than this, so a board plugged in or removed
	// shows up within one tick without enumerating USB on every refresh.
	ftdiDetectTTL = 2 * time.Second
)

// cbusPinCount is the number of CBUS pins the FT230X exposes to bit-bang mode.
const cbusPinCount = 4

// ftdiDevice is the hardware edge, kept deliberately narrow so the manager can
// be tested without a board attached.
type ftdiDevice interface {
	// SetCBUS drives pin (0-3) to a logic level in CBUS bit-bang mode. It also
	// fails when the CBUS pins are not programmed as GPIO, so a successful
	// call doubles as the configuration check.
	SetCBUS(pin int, high bool) error
	Close() error
}

// boardRuntime is one detected board: its enumeration info plus the wiring
// settings resolved from the config and the last commanded VBUS state.
type boardRuntime struct {
	Info ftd2xx.Info

	// comPort is the COM port Windows assigned to this board's VCP interface,
	// from the Windows port enumeration. It stays known while hub4com holds
	// the port, which is exactly when D2XX cannot see the board anymore.
	comPort string

	pin       int
	activeLow bool

	vbusOn    bool
	vbusKnown bool
	lastErr   string
}

// serial returns the board's serial number.
func (b *boardRuntime) serial() string { return b.Info.Serial }

// ftdiManager drives the CBUS pins of every detected board by taking the
// devices over D2XX for a short window between hub4com restarts.
type ftdiManager struct {
	// boardSettings are the explicit ftdi_boards config entries, keyed by
	// serial (upper-cased). defaultSerial/defaultPin/defaultActiveLow carry
	// the legacy single-board keys and act as the fallback for boards without
	// an explicit entry.
	boardSettings    map[string]ftdiBoard
	defaultSerial    string
	defaultPin       int
	defaultActiveLow bool

	// hub is the manager whose child holds the serial ports. Its restart
	// machinery is what frees the devices. May be nil when hub4com is not in
	// use, in which case the ports are assumed to be free already.
	hub *hub4comManager

	logger *log.Logger

	// open, list and comPorts are the only OS-facing dependencies, injected so
	// tests can run without a board. comPorts maps an upper-cased FTDI serial
	// to its COM port via the Windows port enumeration, which works even while
	// the port is held open.
	open     func(serial string) (ftdiDevice, error)
	list     func() ([]ftd2xx.Info, error)
	comPorts func() map[string]string

	opTimeout time.Duration
	detectTTL time.Duration

	// stopCh stops the free-window watcher; started once via StartWatcher.
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	watcherActive bool

	// mu serialises operations: two concurrent toggles must not race for the
	// same free window. It also guards the board table and detection state.
	mu sync.Mutex

	boards     map[string]*boardRuntime
	boardOrder []string // stable menu order
	detectErr  string
	detectAt   time.Time
}

// ftdiOptions carries the settings the manager needs from the config.
type ftdiOptions struct {
	Boards []ftdiBoard

	// Legacy single-board settings; the fallback for unconfigured boards.
	DefaultSerial    string
	DefaultPin       int
	DefaultActiveLow bool
}

// newFTDIManager builds a manager that talks to real hardware.
func newFTDIManager(opts ftdiOptions, logger *log.Logger) *ftdiManager {
	settings := make(map[string]ftdiBoard, len(opts.Boards))
	for _, b := range opts.Boards {
		key := strings.ToUpper(strings.TrimSpace(b.Serial))
		if key != "" {
			settings[key] = b
		}
	}
	return &ftdiManager{
		boardSettings:    settings,
		defaultSerial:    strings.TrimSpace(opts.DefaultSerial),
		defaultPin:       opts.DefaultPin,
		defaultActiveLow: opts.DefaultActiveLow,
		logger:           logger,
		open:             openFTDIDevice,
		list:             ftd2xx.List,
		comPorts:         ftdiComPortMap,
		opTimeout:        ftdiOpTimeout,
		detectTTL:        ftdiDetectTTL,
		stopCh:           make(chan struct{}),
		boards:           make(map[string]*boardRuntime),
	}
}

// StartWatcher begins watching for hub4com restart windows.
//
// While hub4com holds an FT230X's COM port open, D2XX enumeration returns
// placeholder entries only, so the board's serial number is unobtainable.
// The only moments it can be (re)resolved are the windows in which hub4com
// has exited and not yet respawned. Polling for the running -> stopped edge
// catches every such window, including the restart the port monitor triggers
// after a board is unplugged and plugged back in.
func (m *ftdiManager) StartWatcher() {
	m.mu.Lock()
	if m.watcherActive || m.hub == nil {
		m.mu.Unlock()
		return
	}
	m.watcherActive = true
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		wasRunning := m.hub.Running()
		for {
			select {
			case <-m.stopCh:
				return
			case <-ticker.C:
				running := m.hub.Running()
				freed := wasRunning && !running
				wasRunning = running
				if !freed {
					continue
				}

				// The port is free right now; hub4com respawns after about
				// hubRestartBackoff. Retry within the window, then stop - the
				// next window is one restart away.
				for attempt := 0; attempt < 8; attempt++ {
					if err := m.refreshDetection(true); err == nil {
						m.logger.Printf("ftdi: re-detected during restart window")
						break
					}
					select {
					case <-m.stopCh:
						return
					case <-time.After(150 * time.Millisecond):
					}
				}
			}
		}
	}()
}

// Stop halts the watcher. Devices are never held outside an operation, so
// there is nothing to close.
func (m *ftdiManager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.wg.Wait()
}

// cbusMask builds a CBUS bit-bang mask for a single pin.
//
// In CBUS bit-bang mode the mask's upper nibble is the pin direction (1 =
// output) and the lower nibble is the output level, bit N mapping to CBUSN.
// Only the target pin's bits are set, so the other CBUS pins stay tri-stated
// and are never disturbed.
func cbusMask(pin int, output, high bool) byte {
	var m byte
	if output {
		m |= 0x10 << uint(pin)
	}
	if high {
		m |= 0x01 << uint(pin)
	}
	return m
}

// levelFor maps a desired VBUS state to the CBUS logic level that produces it.
func levelFor(activeLow, vbusOn bool) bool {
	// With activeLow the pin is pulled low to turn VBUS on.
	return vbusOn != activeLow
}

// vbusFromLevel maps an observed CBUS level back to a VBUS state.
func vbusFromLevel(activeLow, high bool) bool {
	return high != activeLow
}

// usableInfos filters an enumeration down to entries a board could be driven
// through: those with a readable serial number. Entries with no serial are
// discarded because while another handle has a device open,
// FT_GetDeviceInfoList returns a placeholder entry - type 0, blank serial,
// blank description - and selecting it would mean trying to open an empty
// serial number, which always fails.
func usableInfos(infos []ftd2xx.Info) []ftd2xx.Info {
	usable := make([]ftd2xx.Info, 0, len(infos))
	for _, i := range infos {
		if strings.TrimSpace(i.Serial) != "" {
			usable = append(usable, i)
		}
	}
	return usable
}

// normalizeFtdiSerial canonicalises a serial for use as a board key and for
// FT_OpenEx(FT_OPEN_BY_SERIAL_NUMBER): upper-cased, trimmed, and with the
// Windows FTDIBUS interface-letter suffix removed.
//
// The instance ID of an FT230X with serial DK0MAQPC is
// FTDIBUS\VID_0403+PID_6015+DK0MAQPCA\0000 - the trailing 'A' is the
// interface letter, not part of the serial. D2XX serials are at most 8
// characters, so a longer instance serial always ends in that suffix; opening
// by the suffixed form fails with FT_DEVICE_NOT_FOUND.
func normalizeFtdiSerial(serial string) string {
	s := strings.ToUpper(strings.TrimSpace(serial))
	if len(s) > 8 {
		s = s[:len(s)-1]
	}
	return s
}

// Detect refreshes the device enumeration immediately.
//
// It never opens a device, so it is safe to call while hub4com holds the
// serial ports - which is exactly when the bridge starts, before the ports are
// opened.
func (m *ftdiManager) Detect() error {
	return m.refreshDetection(true)
}

// refreshDetection re-enumerates if the cached result has gone stale.
//
// Boards come from two sources, unioned by serial:
//
//   - the D2XX enumeration, which can only name a board whose port nobody
//     holds (a held port yields a placeholder entry with a blank serial);
//   - the Windows port enumeration, which names every FTDI VCP port and its
//     serial from the FTDIBUS instance ID without opening the port - so a
//     board held by hub4com is still visible and gets a menu and a toggle.
//
// Boards already known are never forgotten on an inconclusive scan: while
// hub4com holds the ports, the D2XX scan degrades to placeholders and (on the
// non-Windows/test path, where no COM map exists) the table is kept.
func (m *ftdiManager) refreshDetection(force bool) error {
	m.mu.Lock()
	fresh := !force && len(m.boards) > 0 && time.Since(m.detectAt) < m.detectTTL
	m.mu.Unlock()
	if fresh {
		return nil
	}

	infos, err := m.list()
	var coms map[string]string
	if err == nil {
		coms = m.comPorts()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.detectAt = time.Now()

	if err != nil {
		m.detectErr = err.Error()
		return err
	}

	usable := usableInfos(infos)

	next := make(map[string]*boardRuntime, len(usable)+len(coms))
	var order []string

	// Boards the D2XX scan can still name.
	for _, info := range usable {
		key := strings.ToUpper(strings.TrimSpace(info.Serial))
		if key == "" {
			continue
		}
		if prev, ok := m.boards[key]; ok {
			// Known board: keep its settings and VBUS state, refresh the info.
			prev.Info = info
			next[key] = prev
		} else {
			next[key] = m.newBoard(info)
		}
		order = append(order, key)
	}

	// A scan that returns only placeholder entries is not evidence that the
	// boards are gone - that is exactly what they look like while hub4com
	// holds the ports - and without a COM map to consult there is nothing to
	// add either, so keep the boards resolved earlier.
	if len(usable) == 0 && len(infos) > 0 && len(coms) == 0 {
		for _, key := range m.boardOrder {
			if b, ok := m.boards[key]; ok {
				next[key] = b
				order = append(order, key)
			}
		}
	}

	// Merge the COM map: every FTDI serial Windows still enumerates is a
	// board, even when D2XX cannot name it because its port is held.
	if len(coms) > 0 {
		keys := make([]string, 0, len(coms))
		for key := range coms {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			// Windows FTDIBUS instance IDs append an interface letter to the
			// D2XX serial (DK0MAQPC + 'A'); FT_OpenEx expects the bare serial,
			// so the key is normalised here in addition to in the COM-map
			// implementation, which keeps the merge safe even if a source
			// reports the suffixed form.
			key = normalizeFtdiSerial(key)
			if key == "" {
				continue
			}
			com := coms[key]
			if com == "" {
				// The raw key differed only by the suffix; find it back.
				for raw, c := range coms {
					if normalizeFtdiSerial(raw) == key {
						com = c
						break
					}
				}
			}
			if b, ok := next[key]; ok {
				b.comPort = com
				continue
			}
			if prev, ok := m.boards[key]; ok {
				prev.comPort = com
				next[key] = prev
			} else {
				b := m.newBoard(ftd2xx.Info{Serial: key})
				b.comPort = com
				next[key] = b
			}
			order = append(order, key)
		}
	}

	m.boards = next
	m.boardOrder = order

	if len(next) == 0 {
		if len(infos) == 0 {
			m.detectErr = "no FTDI device detected"
		} else {
			m.detectErr = "FTDI devices are held by the serial ports; re-detecting at the next restart window"
		}
		return fmt.Errorf("ftdi: %s", m.detectErr)
	}

	m.detectErr = ""
	return nil
}

// newBoard resolves the wiring settings for a freshly detected board.
func (m *ftdiManager) newBoard(info ftd2xx.Info) *boardRuntime {
	key := strings.ToUpper(strings.TrimSpace(info.Serial))
	b := ftdiBoard{Pin: m.defaultPin, ActiveLow: m.defaultActiveLow}
	if s, ok := m.boardSettings[key]; ok {
		b = s
	}
	return &boardRuntime{Info: info, pin: b.Pin, activeLow: b.ActiveLow}
}

// Boards returns the detected boards in a stable order (first seen). It
// refreshes a stale cache first, so callers that only poll (the tray, the Web
// UI) see boards plugged in or removed within one poll.
func (m *ftdiManager) Boards() []boardRuntime {
	_ = m.refreshDetection(false)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]boardRuntime, 0, len(m.boards))
	for _, key := range m.boardOrder {
		if b, ok := m.boards[key]; ok {
			out = append(out, *b)
		}
	}
	return out
}

// BoardCount reports how many boards are currently known.
func (m *ftdiManager) BoardCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.boards)
}

// SetVBUS drives one board's DUT VBUS to the requested state.
//
// The whole open/act/close sequence happens inside one hub4com restart window
// and is serialised against other callers.
func (m *ftdiManager) SetVBUS(serial string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A toggle arriving with a cold cache simply re-enumerates.
	m.mu.Unlock()
	_ = m.refreshDetection(false)
	m.mu.Lock()

	key := strings.ToUpper(strings.TrimSpace(serial))
	board, ok := m.boards[key]
	if !ok {
		// An explicit serial wins even when not (yet) enumerated: the config
		// names it, so drive it and let openWhenFree report absence.
		if s, ok := m.boardSettings[key]; ok {
			board = m.newBoard(ftd2xx.Info{Serial: strings.TrimSpace(serial)})
			board.pin, board.activeLow = s.Pin, s.ActiveLow
			m.boards[key] = board
			m.boardOrder = append(m.boardOrder, key)
		} else if len(m.boards) == 0 {
			// The boards may all be present but held by hub4com (the table
			// is empty only because every scan since startup saw placeholder
			// entries). Open a window now, re-detect inside it, and retry.
			board = m.detectDuringWindow(serial)
			if board == nil {
				return fmt.Errorf("ftdi: no board with serial %s detected; check the USB connection", serial)
			}
		} else {
			return fmt.Errorf("ftdi: no board with serial %s detected; check the USB connection", serial)
		}
	}

	deadline := time.Now().Add(m.opTimeout)

	// Ask hub4com to restart only when it actually holds this board's port:
	// killing the child releases the port and the supervisor's backoff keeps
	// it released for about two seconds. A board on a port hub4com does not
	// open is driven without any bridge interruption at all.
	if m.hub != nil && m.hub.Running() && m.hubHolds(board) {
		m.logger.Printf("ftdi: pausing hub4com to free %s for CBUS%d", board.comPort, board.pin)
		m.hub.kickRestart()
	}

	dev, err := m.openWhenFree(board.serial(), deadline)
	if err != nil {
		board.lastErr = err.Error()
		return err
	}
	defer func() { _ = dev.Close() }()

	if err := dev.SetCBUS(board.pin, levelFor(board.activeLow, on)); err != nil {
		wrapped := fmt.Errorf("ftdi: driving CBUS%d on %s: %w", board.pin, board.serial(), err)
		board.lastErr = wrapped.Error()
		return wrapped
	}

	board.vbusOn = on
	board.vbusKnown = true
	board.lastErr = ""
	m.logger.Printf("ftdi: %s CBUS%d -> VBUS %s (hub4com will restart on its own)", board.serial(), board.pin, onOff(on))
	return nil
}

// detectDuringWindow manufactures a hub4com restart window on demand and
// re-detects inside it, returning the board the caller asked for (or the only
// board, when serial is empty).
//
// This unbreaks the startup chicken-and-egg: the board table is empty because
// hub4com took the port before the first scan, and the normal watcher only
// fires on the NEXT restart - which a freshly started, healthy hub4com may
// never do. The window costs one hub4com restart, the same price a VBUS
// toggle already pays.
func (m *ftdiManager) detectDuringWindow(serial string) *boardRuntime {
	if m.hub == nil {
		return nil
	}
	m.mu.Unlock()
	defer m.mu.Lock()

	if m.hub.Running() {
		m.logger.Printf("ftdi: board table empty; opening a restart window to enumerate")
		m.hub.kickRestart()
	}

	// hubRestartBackoff, not opTimeout: this window has to cover the kill, the
	// supervisor noticing, and the respawn delay, or the re-detection races
	// the child coming back.
	deadline := time.Now().Add(hubRestartBackoff + ftdiOpTimeout)
	for time.Now().Before(deadline) {
		if err := m.refreshDetection(true); err == nil {
			break
		}
		time.Sleep(ftdiFreePoll)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Prefer the requested serial; fall back to the only board.
	if key := strings.ToUpper(strings.TrimSpace(serial)); key != "" {
		if b, ok := m.boards[key]; ok {
			return b
		}
		return nil
	}
	if len(m.boardOrder) == 1 {
		return m.boards[m.boardOrder[0]]
	}
	return nil
}

// hubHolds reports whether hub4com's physical port is this board's port. An
// unknown COM port is assumed held: the safe assumption for a board whose
// port could not be resolved, since a needless restart is far cheaper than a
// failed toggle.
func (m *ftdiManager) hubHolds(b *boardRuntime) bool {
	if strings.TrimSpace(b.comPort) == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(b.comPort), strings.TrimSpace(m.hub.physicalPort))
}

// openWhenFree retries opening the device until the serial port has been
// released. A failed open is the expected condition while hub4com is still
// shutting down, so it is retried rather than reported.
func (m *ftdiManager) openWhenFree(serial string, deadline time.Time) (ftdiDevice, error) {
	var lastErr error
	for {
		dev, err := m.open(serial)
		if err == nil {
			return dev, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ftdi: %s did not become available within %s (last error: %w)",
				serial, m.opTimeout, lastErr)
		}
		time.Sleep(ftdiFreePoll)
	}
}

// Status exposes the subsystem to the Web UI and tray. It refreshes a stale
// device enumeration so a board plugged in or removed is reflected within one
// poll. The shape matches the single-board era with an added "boards" array,
// which every consumer now reads.
func (m *ftdiManager) Status() map[string]any {
	_ = m.refreshDetection(false)

	m.mu.Lock()
	defer m.mu.Unlock()

	boards := make([]map[string]any, 0, len(m.boards))
	for _, key := range m.boardOrder {
		b, ok := m.boards[key]
		if !ok {
			continue
		}
		st := map[string]any{
			"serial":     b.Info.Serial,
			"descriptor": b.Info.Description,
			"pin":        b.pin,
			"activeLow":  b.activeLow,
			"vbusOn":     b.vbusOn,
			"vbusKnown":  b.vbusKnown,
			"configured": m.boardSettings[key].Serial != "",
		}
		if b.comPort != "" {
			st["comPort"] = b.comPort
		}
		if b.lastErr != "" {
			st["lastError"] = b.lastErr
		}
		boards = append(boards, st)
	}

	// Backwards-compatible first-board view for older consumers.
	var first map[string]any
	if len(boards) > 0 {
		first = boards[0]
	}

	st := map[string]any{
		"detected": len(m.boards) > 0,
		"boards":   boards,
		"first":    first,
	}
	if m.detectErr != "" {
		st["detectError"] = m.detectErr
	}
	return st
}

// Validate reports whether the manager is configured well enough to run. The
// devices themselves are not touched: the serial ports are held by hub4com at
// startup, so no D2XX access is possible until a toggle opens the window.
func (m *ftdiManager) Validate() error {
	for _, b := range m.boardSettings {
		if b.Pin < 0 || b.Pin >= cbusPinCount {
			return fmt.Errorf("ftdi_cbus_pin must be between 0 and %d (board %q)", cbusPinCount-1, b.Serial)
		}
	}
	if m.defaultPin < 0 || m.defaultPin >= cbusPinCount {
		return fmt.Errorf("ftdi_cbus_pin must be between 0 and %d", cbusPinCount-1)
	}
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// sortedSerials is a small helper used by tests and the tray for stable
// ordering when boardOrder is not authoritative.
func sortedSerials(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}
