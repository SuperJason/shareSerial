package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

// fakeFTDI stands in for a tool board's FT230X. The manager only ever
// touches it through the ftdiDevice seam, so no hardware is needed.
type fakeFTDI struct {
	mu       sync.Mutex
	lastPin  int
	lastHigh bool
	setCalls int
	closed   bool
	failSet  error
}

func (f *fakeFTDI) SetCBUS(pin int, high bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet != nil {
		return f.failSet
	}
	f.lastPin = pin
	f.lastHigh = high
	f.setCalls++
	return nil
}

func (f *fakeFTDI) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeFTDI) state() (pin int, high bool, calls int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPin, f.lastHigh, f.setCalls, f.closed
}

// fakeOpener fails the first failCount attempts, mimicking the serial port
// still being held while hub4com shuts down.
type fakeOpener struct {
	mu         sync.Mutex
	failCount  int
	attempts   int
	lastSerial string
	dev        *fakeFTDI
}

func (o *fakeOpener) open(serial string) (ftdiDevice, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.attempts++
	o.lastSerial = serial
	if o.attempts <= o.failCount {
		return nil, fmt.Errorf("FT_DEVICE_NOT_OPENED (attempt %d)", o.attempts)
	}
	if o.dev == nil {
		o.dev = &fakeFTDI{}
	}
	return o.dev, nil
}

func (o *fakeOpener) tries() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.attempts
}

func (o *fakeOpener) openedSerial() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastSerial
}

// newTestManager pins the board explicitly so the existing tests exercise the
// pin logic without depending on detection. comPorts returns nothing so the
// Windows COM-port enumeration never leaks real bench hardware into a test.
func newTestManager(activeLow bool, opener *fakeOpener) *ftdiManager {
	return &ftdiManager{
		boardSettings: map[string]ftdiBoard{
			"TESTSN": {Serial: "TESTSN", Pin: 0, ActiveLow: activeLow},
		},
		logger:    log.New(io.Discard, "", 0),
		open:      opener.open,
		list:      func() ([]ftd2xx.Info, error) { return nil, nil },
		comPorts:  func() map[string]string { return nil },
		opTimeout: 500 * time.Millisecond,
		detectTTL: 2 * time.Second,
		stopCh:    make(chan struct{}),
		boards:    make(map[string]*boardRuntime),
	}
}

// newAutoManager leaves the boards list empty so detection is exercised.
func newAutoManager(infos []ftd2xx.Info, opener *fakeOpener) *ftdiManager {
	m := newTestManager(false, opener)
	m.boardSettings = map[string]ftdiBoard{}
	m.list = func() ([]ftd2xx.Info, error) { return infos, nil }
	return m
}

func ft230x(serial string) ftd2xx.Info {
	return ftd2xx.Info{
		Type:        ftd2xx.DeviceTypeXSeries,
		Serial:      serial,
		Description: "FT230X Basic UART",
	}
}

func TestCBUSMask(t *testing.T) {
	tests := []struct {
		pin          int
		output, high bool
		want         byte
	}{
		{pin: 0, output: false, high: false, want: 0x00},
		{pin: 0, output: true, high: false, want: 0x10},
		{pin: 0, output: true, high: true, want: 0x11},
		{pin: 1, output: true, high: true, want: 0x22},
		{pin: 2, output: true, high: true, want: 0x44},
		{pin: 3, output: true, high: true, want: 0x88},
		{pin: 3, output: true, high: false, want: 0x80},
	}
	for _, tc := range tests {
		if got := cbusMask(tc.pin, tc.output, tc.high); got != tc.want {
			t.Errorf("cbusMask(pin=%d output=%t high=%t) = 0x%02X, want 0x%02X",
				tc.pin, tc.output, tc.high, got, tc.want)
		}
	}
}

// TestVBUSPolarity pins down the activeLow mapping in both directions. Getting
// this backwards inverts the meaning of every plug/unplug command, so it is
// asserted directly rather than inferred from other behaviour.
func TestVBUSPolarity(t *testing.T) {
	tests := []struct {
		activeLow bool
		vbusOn    bool
		wantLevel bool
	}{
		{activeLow: true, vbusOn: true, wantLevel: false},
		{activeLow: true, vbusOn: false, wantLevel: true},
		{activeLow: false, vbusOn: true, wantLevel: true},
		{activeLow: false, vbusOn: false, wantLevel: false},
	}
	for _, tc := range tests {
		if got := levelFor(tc.activeLow, tc.vbusOn); got != tc.wantLevel {
			t.Errorf("activeLow=%t VBUS=%t: levelFor = %t, want %t",
				tc.activeLow, tc.vbusOn, got, tc.wantLevel)
		}
		if got := vbusFromLevel(tc.activeLow, tc.wantLevel); got != tc.vbusOn {
			t.Errorf("activeLow=%t level=%t: vbusFromLevel = %t, want %t",
				tc.activeLow, tc.wantLevel, got, tc.vbusOn)
		}
	}
}

// TestSetVBUSDrivesPinAndReleases checks the whole window: the pin is driven
// with the level the polarity implies, the handle is closed again so hub4com
// can reclaim the port, and the reported state follows.
func TestSetVBUSDrivesPinAndReleases(t *testing.T) {
	for _, tc := range []struct {
		activeLow bool
		vbusOn    bool
		wantHigh  bool
	}{
		{activeLow: true, vbusOn: true, wantHigh: false},
		{activeLow: true, vbusOn: false, wantHigh: true},
		{activeLow: false, vbusOn: true, wantHigh: true},
		{activeLow: false, vbusOn: false, wantHigh: false},
	} {
		opener := &fakeOpener{}
		m := newTestManager(tc.activeLow, opener)

		if err := m.SetVBUS("TESTSN", tc.vbusOn); err != nil {
			t.Fatalf("SetVBUS(%t): %v", tc.vbusOn, err)
		}

		_, high, calls, closed := opener.dev.state()
		if calls != 1 {
			t.Errorf("expected exactly one SetCBUS call, got %d", calls)
		}
		if high != tc.wantHigh {
			t.Errorf("activeLow=%t VBUS=%t: pin driven high=%t, want %t",
				tc.activeLow, tc.vbusOn, high, tc.wantHigh)
		}
		if !closed {
			t.Error("the device must be released after the operation, or hub4com can never reclaim the port")
		}
		st := m.Status()
		boards := st["boards"].([]map[string]any)
		if boards[0]["vbusOn"] != tc.vbusOn || boards[0]["vbusKnown"] != true {
			t.Errorf("Status() board state = (%v, known=%v), want (%v, true)",
				boards[0]["vbusOn"], boards[0]["vbusKnown"], tc.vbusOn)
		}
	}
}

// TestSetVBUSWaitsForPortToFree covers the core of the design: the first open
// attempts fail because hub4com has not yet released the port, and the manager
// must keep retrying inside the window rather than giving up.
func TestSetVBUSWaitsForPortToFree(t *testing.T) {
	opener := &fakeOpener{failCount: 3}
	m := newTestManager(false, opener)

	if err := m.SetVBUS("TESTSN", true); err != nil {
		t.Fatalf("SetVBUS should have retried until the port was free: %v", err)
	}
	if tries := opener.tries(); tries != 4 {
		t.Errorf("expected 4 open attempts (3 failures then a success), got %d", tries)
	}
}

// TestSetVBUSGivesUpWhenPortStaysBusy checks the failure path: if the port is
// never released the error must name the serial number and the last cause, not
// spin forever.
func TestSetVBUSGivesUpWhenPortStaysBusy(t *testing.T) {
	opener := &fakeOpener{failCount: 1 << 30}
	m := newTestManager(false, opener)
	m.opTimeout = 100 * time.Millisecond

	start := time.Now()
	err := m.SetVBUS("TESTSN", true)
	if err == nil {
		t.Fatal("expected an error when the port is never released")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("gave up after %s; should stop at the timeout", elapsed)
	}
	st := m.Status()
	boards := st["boards"].([]map[string]any)
	if boards[0]["vbusKnown"] == true {
		t.Error("a failed operation must not report a known state")
	}
	if boards[0]["lastError"] == nil {
		t.Error("the failure should be surfaced in Status() for the Web UI")
	}
}

// TestSetVBUSReportsNonGPIO covers the most likely bring-up fault: the pin is
// not programmed as GPIO, so CBUS bit-bang is refused.
func TestSetVBUSReportsNonGPIO(t *testing.T) {
	opener := &fakeOpener{}
	opener.dev = &fakeFTDI{failSet: fmt.Errorf("FT_INVALID_PARAMETER")}
	m := newTestManager(false, opener)

	err := m.SetVBUS("TESTSN", true)
	if err == nil {
		t.Fatal("expected an error when CBUS bit-bang is refused")
	}
	_, _, _, closed := opener.dev.state()
	if !closed {
		t.Error("a failed operation must still release the device")
	}
}

// TestSetVBUSPausesHub4com checks that a running hub4com is asked to restart,
// which is what frees the serial port. Without this the device would never
// become available.
func TestSetVBUSPausesHub4com(t *testing.T) {
	hub := &hub4comManager{
		// A non-nil cmd is what Running() reports as "holding the port".
		// Process stays nil so kickRestart takes its channel-notification
		// branch instead of trying to kill anything.
		cmd:       &exec.Cmd{},
		restartCh: make(chan struct{}, 1),
	}
	opener := &fakeOpener{}
	m := newTestManager(false, opener)
	m.hub = hub

	if err := m.SetVBUS("TESTSN", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	select {
	case <-hub.restartCh:
		// expected: a restart was requested
	default:
		t.Error("hub4com was running but no restart was requested, so the port would never be freed")
	}
}

// TestSetVBUSDoesNotPokeRestartWhenIdle is the mirror image, and matters more
// than it looks: poking restartCh on an idle manager would make hub4com
// respawn immediately and snatch the port back in the middle of the window.
func TestSetVBUSDoesNotPokeRestartWhenIdle(t *testing.T) {
	hub := &hub4comManager{restartCh: make(chan struct{}, 1)}
	opener := &fakeOpener{}
	m := newTestManager(false, opener)
	m.hub = hub

	if err := m.SetVBUS("TESTSN", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	select {
	case <-hub.restartCh:
		t.Error("an idle hub4com must not be woken, or it respawns into the middle of the window")
	default:
	}
}

func TestValidateRejectsBadPin(t *testing.T) {
	for _, pin := range []int{-1, 4, 99} {
		m := &ftdiManager{boardSettings: map[string]ftdiBoard{"X": {Serial: "X", Pin: pin}}}
		if err := m.Validate(); err == nil {
			t.Errorf("pin %d should be rejected", pin)
		}
	}
	for pin := 0; pin < cbusPinCount; pin++ {
		m := &ftdiManager{boardSettings: map[string]ftdiBoard{"X": {Serial: "X", Pin: pin}}}
		if err := m.Validate(); err != nil {
			t.Errorf("pin %d should be accepted: %v", pin, err)
		}
	}
}

func TestStatusIncludesFTDIFields(t *testing.T) {
	opener := &fakeOpener{}
	m := newTestManager(true, opener)
	if err := m.SetVBUS("TESTSN", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}

	st := m.Status()
	boards, ok := st["boards"].([]map[string]any)
	if !ok || len(boards) != 1 {
		t.Fatalf("Status()[boards] = %v; want one board", st["boards"])
	}
	for _, key := range []string{"serial", "pin", "activeLow", "vbusOn", "vbusKnown", "descriptor"} {
		if _, ok := boards[0][key]; !ok {
			t.Errorf("Status() board is missing key %q, which the Web UI and tray read", key)
		}
	}
}

// --- auto-detection ------------------------------------------------------

// TestUsableInfosSkipsPlaceholders covers behaviour observed on the real
// board: while another handle holds the device open, FT_GetDeviceInfoList
// returns a placeholder entry with no serial and no type. Selecting it would
// mean trying to open an empty serial number, which always fails.
func TestUsableInfosSkipsPlaceholders(t *testing.T) {
	placeholder := []ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown, Serial: "", Description: ""}}
	if got := usableInfos(placeholder); len(got) != 0 {
		t.Errorf("a placeholder entry with no serial must never be usable, got %v", got)
	}
	mixed := append([]ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, ft230x("DK0MAQPC"))
	if got := usableInfos(mixed); len(got) != 1 || got[0].Serial != "DK0MAQPC" {
		t.Errorf("usableInfos = %v; the real entry should survive", got)
	}
}

// TestSetVBUSAutoDetectsSerial covers the point of the feature: no serial in
// the config, and the right board is found and opened anyway.
func TestSetVBUSAutoDetectsSerial(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("DK0MAQPC")}, opener)

	if err := m.SetVBUS("DK0MAQPC", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	if got := opener.openedSerial(); got != "DK0MAQPC" {
		t.Errorf("opened %q, want the auto-detected DK0MAQPC", got)
	}

	st := m.Status()
	boards := st["boards"].([]map[string]any)
	if serial, _ := boards[0]["serial"].(string); serial != "DK0MAQPC" {
		t.Errorf("Status() serial = %q, which is what the tray and Web UI display", serial)
	}

	if got := m.Boards(); len(got) != 1 || got[0].Info.Description != "FT230X Basic UART" {
		t.Errorf("Boards() = %v; the description is shown in the Web UI", got)
	}
}

// TestSetVBUSFailsWithoutDetection checks that a toggle with no board attached
// reports why instead of hanging until the timeout.
func TestSetVBUSFailsWithoutDetection(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager(nil, opener)

	err := m.SetVBUS("ANY", true)
	if err == nil {
		t.Fatal("expected an error when no board is attached")
	}
	if opener.tries() != 0 {
		t.Error("must not attempt to open the device when detection found nothing")
	}
	st := m.Status()
	if st["detected"] != false {
		t.Error("Status() should report the board as not detected")
	}
	if st["detectError"] == nil {
		t.Error("Status() should explain why detection failed, for the tray and Web UI")
	}
}

// --- multiple boards ------------------------------------------------------

// TestMultipleBoardsDetectedAndDriven is the core of the new feature: two
// boards enumerate, each keeps its own settings and VBUS state, and a toggle
// on one does not disturb the other.
func TestMultipleBoardsDetectedAndDriven(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("SN_A"), ft230x("SN_B")}, opener)
	m.boardSettings = map[string]ftdiBoard{
		"SN_B": {Serial: "SN_B", Pin: 2, ActiveLow: true},
	}

	if got := m.Boards(); len(got) != 2 {
		t.Fatalf("Boards() = %d boards, want 2", len(got))
	}

	if err := m.SetVBUS("SN_A", true); err != nil {
		t.Fatalf("SetVBUS(SN_A): %v", err)
	}
	if err := m.SetVBUS("SN_B", false); err != nil {
		t.Fatalf("SetVBUS(SN_B): %v", err)
	}

	st := m.Status()
	boards := st["boards"].([]map[string]any)
	if len(boards) != 2 {
		t.Fatalf("Status()[boards] = %d, want 2", len(boards))
	}
	bySerial := map[string]map[string]any{}
	for _, b := range boards {
		bySerial[b["serial"].(string)] = b
	}
	a, b := bySerial["SN_A"], bySerial["SN_B"]
	if a["vbusOn"] != true || a["vbusKnown"] != true {
		t.Errorf("SN_A state = %v known=%v, want on/known", a["vbusOn"], a["vbusKnown"])
	}
	if b["vbusOn"] != false || b["vbusKnown"] != true {
		t.Errorf("SN_B state = %v known=%v, want off/known", b["vbusOn"], b["vbusKnown"])
	}
	if a["pin"] != 0 || b["pin"] != 2 {
		t.Errorf("pins = %v/%v; SN_B must take its configured CBUS2", a["pin"], b["pin"])
	}
	if b["activeLow"] != true {
		t.Errorf("SN_B activeLow = %v, want true from ftdi_boards", b["activeLow"])
	}
	if a["configured"] != false || b["configured"] != true {
		t.Errorf("configured flags = %v/%v; only SN_B has an explicit entry", a["configured"], b["configured"])
	}
}

// TestBoardSetRecomputesOnChange: a board unplugged (scan no longer lists it)
// must drop out of the board table; a new one plugged in must appear.
func TestBoardSetRecomputesOnChange(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("SN_A"), ft230x("SN_B")}, opener)
	if got := m.Boards(); len(got) != 2 {
		t.Fatalf("initial boards = %d, want 2", len(got))
	}

	m.list = func() ([]ftd2xx.Info, error) { return []ftd2xx.Info{ft230x("SN_B")}, nil }
	m.mu.Lock()
	m.detectAt = time.Time{}
	m.mu.Unlock()
	if got := m.Boards(); len(got) != 1 || got[0].Info.Serial != "SN_B" {
		t.Fatalf("after re-enumeration boards = %+v, want only SN_B", got)
	}
}

// TestUnknownSerialRejected: toggling a serial that is neither detected nor
// configured must fail cleanly rather than create a ghost board.
func TestUnknownSerialRejected(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("SN_A")}, opener)
	if err := m.SetVBUS("GHOST", true); err == nil {
		t.Fatal("expected an error for a serial that is neither detected nor configured")
	}
	if got := m.Boards(); len(got) != 1 {
		t.Errorf("a rejected toggle must not create a board; boards = %d", len(got))
	}
}

// TestConfiguredButAbsentBoardCanStillDrive: a board named in ftdi_boards but
// not enumerated is driven anyway; openWhenFree surfaces absence if real.
func TestConfiguredButAbsentBoardCanStillDrive(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager(nil, opener)
	m.boardSettings = map[string]ftdiBoard{
		"FIXTURE": {Serial: "FIXTURE", Pin: 1, ActiveLow: false},
	}
	if err := m.SetVBUS("FIXTURE", true); err != nil {
		t.Fatalf("SetVBUS on a configured-but-absent board: %v", err)
	}
	if got := opener.openedSerial(); got != "FIXTURE" {
		t.Errorf("opened %q, want FIXTURE", got)
	}
}

// TestDefaultsApplyToUnconfiguredBoards: boards without an ftdi_boards entry
// take the legacy single-board pin/polarity.
func TestDefaultsApplyToUnconfiguredBoards(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("SN_A")}, opener)
	m.defaultPin = 3
	m.defaultActiveLow = true

	if err := m.SetVBUS("SN_A", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	// activeLow=true, VBUS on -> pin driven low.
	if _, high, _, _ := opener.dev.state(); high {
		t.Error("defaultActiveLow must apply to unconfigured boards")
	}
	st := m.Status()
	boards := st["boards"].([]map[string]any)
	if boards[0]["pin"] != 3 || boards[0]["activeLow"] != true {
		t.Errorf("board settings = pin %v activeLow %v, want 3/true", boards[0]["pin"], boards[0]["activeLow"])
	}
}

// --- config defaults -----------------------------------------------------

func hubConfig() bridgeConfig {
	return bridgeConfig{
		ComPort:         "COM161",
		BaudRate:        115200,
		TargetPort:      2601,
		Hub4comPath:     "hub4com.exe",
		HubPhysicalPort: "COM6",
	}
}

// TestFtdiEnabledDefaultsOn covers the requirement that the feature is on
// unless explicitly turned off. This is why the field is a pointer: a plain
// bool cannot tell an absent key from an explicit false after JSON decoding.
func TestFtdiEnabledDefaultsOn(t *testing.T) {
	cfg := hubConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.FTDIEnabled() {
		t.Error("an absent ftdi_enabled must default to enabled")
	}
	if cfg.FtdiEnabled == nil {
		t.Error("Validate should materialise the default so the setting is explicit")
	}

	cfg = hubConfig()
	cfg.FtdiEnabled = boolPtr(false)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.FTDIEnabled() {
		t.Error("an explicit false must disable the feature")
	}

	cfg = hubConfig()
	cfg.FtdiEnabled = boolPtr(true)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.FTDIEnabled() {
		t.Error("an explicit true must enable the feature")
	}
}

// TestFtdiOffWithoutHub4com: hub4com is what releases the serial port, so
// without it the feature has nothing to pause. A plain single-port config must
// keep working rather than failing validation.
func TestFtdiOffWithoutHub4com(t *testing.T) {
	cfg := bridgeConfig{ComPort: "COM1", BaudRate: 115200, TargetPort: 2601}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate must not fail for a config without hub4com: %v", err)
	}
	if cfg.FTDIEnabled() {
		t.Error("without hub4com there is nothing to pause, so the feature should be off")
	}
}

func TestFtdiRejectsBadPinConfig(t *testing.T) {
	cfg := hubConfig()
	cfg.FtdiBoards = []ftdiBoard{{Serial: "X", Pin: 7}}
	if err := cfg.Validate(); err == nil {
		t.Error("an out-of-range ftdi_boards pin must be rejected")
	}
	cfg = hubConfig()
	cfg.FtdiCbusPin = 7
	if err := cfg.Validate(); err == nil {
		t.Error("an out-of-range legacy ftdi_cbus_pin must be rejected")
	}
}

// TestLegacyConfigMigratesToBoards: the old single-board keys must become a
// boards entry so old configs keep targeting the same board.
func TestLegacyConfigMigratesToBoards(t *testing.T) {
	cfg := hubConfig()
	cfg.FtdiSerial = "DK0MAQPC"
	cfg.FtdiCbusPin = 2
	cfg.FtdiActiveLow = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.FtdiBoards) != 1 {
		t.Fatalf("legacy keys must migrate into ftdi_boards, got %v", cfg.FtdiBoards)
	}
	b := cfg.FtdiBoards[0]
	if b.Serial != "DK0MAQPC" || b.Pin != 2 || b.ActiveLow != true {
		t.Errorf("migrated entry = %+v", b)
	}

	// An explicit ftdi_boards entry for the same serial wins; no duplicate.
	cfg = hubConfig()
	cfg.FtdiSerial = "DK0MAQPC"
	cfg.FtdiBoards = []ftdiBoard{{Serial: "DK0MAQPC", Pin: 1}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.FtdiBoards) != 1 || cfg.FtdiBoards[0].Pin != 1 {
		t.Errorf("explicit entry must not be duplicated by migration, got %v", cfg.FtdiBoards)
	}
}

// TestBoardForResolution: ftdiBoardFor must prefer the explicit entry, then
// the legacy keys, then the zero value.
func TestBoardForResolution(t *testing.T) {
	cfg := hubConfig()
	cfg.FtdiBoards = []ftdiBoard{{Serial: "B1", Pin: 1, ActiveLow: true}}
	cfg.FtdiCbusPin = 2
	cfg.FtdiActiveLow = true

	if b := cfg.ftdiBoardFor("b1"); b.Pin != 1 || !b.ActiveLow { // case-insensitive
		t.Errorf("explicit entry = %+v", b)
	}
	if b := cfg.ftdiBoardFor("OTHER"); b.Pin != 2 || !b.ActiveLow {
		t.Errorf("legacy defaults = %+v", b)
	}

	cfg2 := hubConfig()
	if b := cfg2.ftdiBoardFor("ANY"); b.Pin != 0 || b.ActiveLow {
		t.Errorf("plain defaults = %+v", b)
	}
}

// TestSetVBUSDetectsDuringWindow is the startup chicken-and-egg regression:
// hub4com took the port before the first scan, so the board table is empty
// and every scan sees placeholders. A toggle must manufacture its own restart
// window, re-detect inside it, and then drive the pin.
func TestSetVBUSDetectsDuringWindow(t *testing.T) {
	hub := &hub4comManager{restartCh: make(chan struct{}, 1)}
	opener := &fakeOpener{}

	m := newAutoManager(nil, opener)
	m.hub = hub
	m.opTimeout = 500 * time.Millisecond

	// Enumeration: placeholder while hub4com holds the port, real board once
	// the restart fires.
	m.list = func() ([]ftd2xx.Info, error) {
		if hub.Running() {
			return []ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, nil
		}
		return []ftd2xx.Info{ft230x("DK0MAQPC")}, nil
	}
	// hub4com is running and holds the port. kickRestart cannot kill a nil
	// Process, so emulate the kill by draining the restart notification into
	// releasing the port - exactly what the real supervisor does.
	hub.mu.Lock()
	hub.cmd = &exec.Cmd{}
	hub.mu.Unlock()
	go func() {
		<-hub.restartCh
		hub.mu.Lock()
		hub.cmd = nil
		hub.mu.Unlock()
	}()

	if err := m.SetVBUS("DK0MAQPC", true); err != nil {
		t.Fatalf("SetVBUS must open a window and detect the board: %v", err)
	}
	if got := opener.openedSerial(); got != "DK0MAQPC" {
		t.Errorf("opened %q, want DK0MAQPC", got)
	}
	// The board must now be in the table for later toggles.
	if got := m.Boards(); len(got) != 1 || got[0].Info.Serial != "DK0MAQPC" {
		t.Errorf("Boards() = %+v after window detection", got)
	}
}

// TestComMapAddsHeldBoards: COM6's board is held by hub4com (D2XX sees only a
// placeholder), but the Windows COM map still names it. The board must enter
// the table with its COM port so the tray can show and drive it.
func TestComMapAddsHeldBoards(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, opener)
	m.comPorts = func() map[string]string { return map[string]string{"DK0MAQPC": "COM6"} }

	boards := m.Boards()
	if len(boards) != 1 {
		t.Fatalf("boards = %d, want the COM-map board", len(boards))
	}
	if boards[0].comPort != "COM6" || boards[0].Info.Serial != "DK0MAQPC" {
		t.Errorf("board = %+v, want DK0MAQPC on COM6", boards[0])
	}

	if err := m.SetVBUS("DK0MAQPC", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	st := m.Status()
	if bs := st["boards"].([]map[string]any); bs[0]["comPort"] != "COM6" {
		t.Errorf("Status comPort = %v, want COM6", bs[0]["comPort"])
	}
}

// TestComMapNormalizesInterfaceSuffix is the regression guard for the failure
// seen on hardware: Windows FTDIBUS instance IDs append an interface letter to
// the D2XX serial (DK0MAQPC + 'A' -> DK0MAQPCA), and the COM map keyed boards
// by the 9-character instance serial. FT_OpenEx(FT_OPEN_BY_SERIAL_NUMBER)
// only accepts the bare 8-character serial, so every toggle of a board
// registered from the COM map failed with FT_DEVICE_NOT_FOUND, and a board
// visible to both sources appeared twice.
func TestComMapNormalizesInterfaceSuffix(t *testing.T) {
	opener := &fakeOpener{}
	// D2XX names the COM6 board; the COM map supplies both boards, keyed as
	// the enumerator reports them (with the interface letter).
	m := newAutoManager([]ftd2xx.Info{ft230x("DK0MAQPC")}, opener)
	m.comPorts = func() map[string]string {
		return map[string]string{
			"DK0MAQPCA": "COM6",  // D2XX serial + interface A
			"DK0MEBFQA": "COM18", // the free board, only the COM map names it
		}
	}

	boards := m.Boards()
	if len(boards) != 2 {
		t.Fatalf("boards = %d, want 2 (DK0MAQPC, DK0MEBFQ); a suffix mismatch splits or duplicates boards", len(boards))
	}
	bySerial := map[string]boardRuntime{}
	for _, b := range boards {
		bySerial[strings.ToUpper(b.Info.Serial)] = b
	}
	com6, ok := bySerial["DK0MAQPC"]
	if !ok {
		t.Fatalf("no board under serial DK0MAQPC; got %+v", boards)
	}
	if com6.comPort != "COM6" {
		t.Errorf("DK0MAQPC comPort = %q, want COM6 (the COM-map entry must merge, not duplicate)", com6.comPort)
	}
	if _, dup := bySerial["DK0MAQPCA"]; dup {
		t.Error("DK0MAQPCA is a phantom duplicate board that can never be opened")
	}
	if com18, ok := bySerial["DK0MEBFQ"]; !ok || com18.comPort != "COM18" {
		t.Errorf("DK0MEBFQ = %+v, want the COM18 board keyed by its bare serial", bySerial["DK0MEBFQ"])
	}

	// Toggling by the COM-map-sourced board must open the bare serial, which
	// succeeds; opening the suffixed one was the hardware failure.
	if err := m.SetVBUS("DK0MEBFQ", true); err != nil {
		t.Fatalf("SetVBUS on the COM-map-sourced board: %v", err)
	}
	if got := opener.openedSerial(); got != "DK0MEBFQ" {
		t.Errorf("opened %q, want the bare serial DK0MEBFQ", got)
	}
}

// TestComMapKeepsBoardAfterUnplug: once the COM map no longer lists a board
// and D2XX cannot name it either, it leaves the table.
func TestComMapKeepsBoardAfterUnplug(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager(nil, opener)
	m.comPorts = func() map[string]string { return map[string]string{"DK0MEBFQ": "COM18"} }
	if got := m.Boards(); len(got) != 1 {
		t.Fatalf("boards = %d, want 1", len(got))
	}

	m.comPorts = func() map[string]string { return nil }
	m.mu.Lock()
	m.detectAt = time.Time{}
	m.mu.Unlock()
	if got := m.Boards(); len(got) != 0 {
		t.Fatalf("board must leave the table when no source lists it, got %d", len(got))
	}
}

// TestSetVBUSSkipsRestartForForeignPort: a board whose COM port is not the
// hub4com physical port must not trigger a hub restart - its port is already
// free, and restarting would needlessly interrupt the bridge.
func TestSetVBUSSkipsRestartForForeignPort(t *testing.T) {
	hub := &hub4comManager{
		cmd:          &exec.Cmd{},
		restartCh:    make(chan struct{}, 1),
		physicalPort: "COM6",
	}
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("DK0MEBFQ")}, opener)
	m.comPorts = func() map[string]string { return map[string]string{"DK0MEBFQ": "COM18"} }
	m.hub = hub

	if err := m.SetVBUS("DK0MEBFQ", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	select {
	case <-hub.restartCh:
		t.Error("hub4com does not hold COM18; a restart would needlessly interrupt the bridge")
	default:
	}
}

// TestSetVBUSRestartsForHeldPort: the mirror image - the board on the hub4com
// physical port needs the restart window to free its port.
func TestSetVBUSRestartsForHeldPort(t *testing.T) {
	hub := &hub4comManager{
		cmd:          &exec.Cmd{},
		restartCh:    make(chan struct{}, 1),
		physicalPort: "COM6",
	}
	opener := &fakeOpener{}
	m := newAutoManager(nil, opener)
	m.comPorts = func() map[string]string { return map[string]string{"DK0MAQPC": "COM6"} }
	m.hub = hub

	if err := m.SetVBUS("DK0MAQPC", true); err != nil {
		t.Fatalf("SetVBUS: %v", err)
	}
	select {
	case <-hub.restartCh:
		// expected: the port is held, so a restart was requested
	default:
		t.Error("the board sits on hub4com's physical port; a restart must be requested to free it")
	}
}

// --- enumeration robustness ----------------------------------------------

// TestDetectionKeepsBoardsWhenScanIsInconclusive is the regression guard for
// the bug found on hardware: the periodic re-scan must not throw away the
// boards resolved at startup, or every toggle breaks as soon as hub4com has
// taken the ports.
func TestDetectionKeepsBoardsWhenScanIsInconclusive(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("DK0MAQPC")}, opener)

	if got := m.Boards(); len(got) != 1 {
		t.Fatal("expected the board to be detected before the port is taken")
	}

	// hub4com now holds the port and enumeration degrades to placeholders.
	m.list = func() ([]ftd2xx.Info, error) {
		return []ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, nil
	}
	m.mu.Lock()
	m.detectAt = time.Time{}
	m.mu.Unlock()

	if err := m.SetVBUS("DK0MAQPC", true); err != nil {
		t.Fatalf("SetVBUS should still work using the remembered serial: %v", err)
	}
	if got := opener.openedSerial(); got != "DK0MAQPC" {
		t.Errorf("opened %q, want the remembered DK0MAQPC", got)
	}
}

// TestDetectionClearsWhenBoardsAreGone: an empty scan, unlike a placeholder
// one, really does mean no boards are present.
func TestDetectionClearsWhenBoardsAreGone(t *testing.T) {
	opener := &fakeOpener{}
	m := newAutoManager([]ftd2xx.Info{ft230x("DK0MAQPC")}, opener)
	if got := m.Boards(); len(got) != 1 {
		t.Fatal("expected detection")
	}

	m.list = func() ([]ftd2xx.Info, error) { return nil, nil }
	m.mu.Lock()
	m.detectAt = time.Time{}
	m.mu.Unlock()

	if got := m.Boards(); len(got) != 0 {
		t.Fatalf("an empty scan must clear the boards, got %d", len(got))
	}
	if err := m.SetVBUS("DK0MAQPC", true); err == nil {
		t.Fatal("expected an error once the board is gone")
	}
	if opener.tries() != 0 {
		t.Error("must not try to open anything when no device is present")
	}
}

// TestWatcherReDetectsInRestartWindow reproduces the unplug/replug failure
// observed on hardware: after the board is unplugged the detection state is
// cleared, and while hub4com holds the port every scan returns only
// placeholder entries. The serial must be recovered during the restart window
// that follows hub4com exiting.
func TestWatcherReDetectsInRestartWindow(t *testing.T) {
	opener := &fakeOpener{}
	hub := &hub4comManager{}

	// Enumeration reports the real board only while the port is free; while
	// hub4com holds it, the scan sees a placeholder, exactly as on hardware.
	m := newAutoManager(nil, opener)
	m.list = func() ([]ftd2xx.Info, error) {
		if hub.Running() {
			return []ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, nil
		}
		return []ftd2xx.Info{ft230x("DK0MAQPC")}, nil
	}
	m.detectTTL = 20 * time.Millisecond
	m.hub = hub
	m.StartWatcher()
	defer m.Stop()

	// hub4com holds the port: detection cannot succeed.
	hub.mu.Lock()
	hub.cmd = &exec.Cmd{}
	hub.mu.Unlock()
	_ = m.refreshDetection(true)
	if got := m.Boards(); len(got) != 0 {
		t.Fatal("detection should not succeed while the port is held")
	}

	// hub4com exits: the watcher must recover the board within the window.
	hub.mu.Lock()
	hub.cmd = nil
	hub.mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := m.Boards(); len(got) == 1 {
			return // recovered
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("watcher did not re-detect the board during the restart window")
}

// TestHeldBoardMessage: while the port is held, the status must explain the
// situation rather than claiming no FT-X board is present.
func TestHeldBoardMessage(t *testing.T) {
	m := newAutoManager([]ftd2xx.Info{{Type: ftd2xx.DeviceTypeUnknown}}, &fakeOpener{})
	m.mu.Lock()
	m.detectAt = time.Time{} // force a fresh scan
	m.mu.Unlock()

	st := m.Status()
	if msg, _ := st["detectError"].(string); msg == "" || !strings.Contains(msg, "held by the serial ports") {
		t.Errorf("detectError = %q; it should say the boards are held, not misreport them as absent", msg)
	}
}
