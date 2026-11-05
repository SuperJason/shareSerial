package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

const readTimeout = 50 * time.Millisecond

var (
	errNoPort   = errors.New("serial port is not open")
	errNoDevice = errors.New("no serial port selected")
)

// serialPort is the subset of serial.Port the bridge uses, so tests can inject
// a fake device.
type serialPort interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	SetMode(mode *serial.Mode) error
	SetReadTimeout(d time.Duration) error
}

var openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
	return serial.Open(name, mode)
}

type portParams struct {
	Port     string
	Mode     serial.Mode
	Encoding string
	Idle     time.Duration
	MaxLine  int
}

// App owns the serial port and is the single source of truth for everything
// downstream: the log ring, the byte fan-out bus and the capture files.
type App struct {
	rtlog *log.Logger
	ring  *ring
	bus   *bus
	cap   *capture

	mu      sync.Mutex
	cfg     Config
	cfgPath string
	port    serialPort
	quit    chan struct{}
	codec   *codec // TX encoding + TX display decoding
	params  portParams
	status  string
	lastErr string

	demoStop chan struct{}

	writeMu   sync.Mutex
	startedAt time.Time
	rxBytes   atomic.Int64
	txBytes   atomic.Int64
	rxRecs    atomic.Int64
	txRecs    atomic.Int64
}

func newApp(cfg Config, rtlog *log.Logger) (*App, error) {
	a := &App{
		rtlog:     rtlog,
		ring:      newRing(cfg.MaxRecords, cfg.MaxBytes),
		bus:       newBus(),
		cfg:       cfg,
		status:    "closed",
		startedAt: time.Now(),
	}
	a.cap = newCapture()
	if cfg.CaptureEnabled {
		if path, err := a.cap.Start(cfg.CaptureDir, cfg.ComPort); err != nil {
			a.emitSys(fmt.Sprintf("capture could not start: %v", err))
		} else {
			a.emitSys("capture started: " + path)
		}
	}
	return a, nil
}

// SetCapture turns the text capture on or off. dir only applies when starting:
// it becomes the folder for this and later captures. The runtime toggle is not
// persisted - capture always starts off unless the config or -capture says
// otherwise - so a forgotten session cannot quietly fill the disk.
func (a *App) SetCapture(on bool, dir string) (string, error) {
	if !on {
		path := a.cap.Stop()
		a.emitSys("capture stopped: " + path)
		a.rtlog.Printf("capture stopped: %s", path)
		return path, nil
	}
	a.mu.Lock()
	if dir != "" {
		a.cfg.CaptureDir = dir
	}
	dir = a.cfg.CaptureDir
	port := a.cfg.ComPort
	cfgPath := a.cfgPath
	cfg := a.cfg
	a.mu.Unlock()

	path, err := a.cap.Start(dir, port)
	if err != nil {
		return "", err
	}
	a.emitSys("capture started: " + path)
	a.rtlog.Printf("capture started: %s", path)
	if cfgPath != "" {
		if err := saveConfig(cfgPath, cfg); err != nil {
			a.rtlog.Printf("save config failed: %v", err)
		}
	}
	return path, nil
}

// SetCaptureDir remembers a new capture folder without touching a running
// capture; the next Start uses it.
func (a *App) SetCaptureDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("empty folder")
	}
	a.mu.Lock()
	a.cfg.CaptureDir = dir
	cfgPath := a.cfgPath
	cfg := a.cfg
	a.mu.Unlock()
	if cfgPath != "" {
		if err := saveConfig(cfgPath, cfg); err != nil {
			a.rtlog.Printf("save config failed: %v", err)
			return err
		}
	}
	return nil
}

// SchemesDir holds user colour schemes: <config dir>/schemes/*.json.
func (a *App) SchemesDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfgPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(a.cfgPath), "schemes")
}

func (a *App) Config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SetConfigPath records where the runtime configuration is persisted.
func (a *App) SetConfigPath(p string) {
	a.mu.Lock()
	a.cfgPath = p
	a.mu.Unlock()
}

func (a *App) ConfigPath() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfgPath
}

func (a *App) Ring() *ring    { return a.ring }
func (a *App) Bus() *bus      { return a.bus }
func (a *App) Logger() *log.Logger { return a.rtlog }

// modeLabel renders "115200 8N1" style text for the log.
func modeLabel(m serial.Mode) string {
	parity := "N"
	switch m.Parity {
	case serial.OddParity:
		parity = "O"
	case serial.EvenParity:
		parity = "E"
	case serial.MarkParity:
		parity = "M"
	case serial.SpaceParity:
		parity = "S"
	}
	stop := "1"
	if m.StopBits == serial.TwoStopBits {
		stop = "2"
	}
	return fmt.Sprintf("%d %d%s%s", m.BaudRate, m.DataBits, parity, stop)
}

func parityOf(name string) serial.Parity {
	switch strings.ToLower(name) {
	case "odd":
		return serial.OddParity
	case "even":
		return serial.EvenParity
	case "mark":
		return serial.MarkParity
	case "space":
		return serial.SpaceParity
	default:
		return serial.NoParity
	}
}

func stopBitsOf(n int) serial.StopBits {
	if n == 2 {
		return serial.TwoStopBits
	}
	return serial.OneStopBit
}

func (c Config) serialMode() serial.Mode {
	return serial.Mode{
		BaudRate: c.BaudRate,
		DataBits: c.DataBits,
		Parity:   parityOf(c.Parity),
		StopBits: stopBitsOf(c.StopBits),
	}
}

func (c Config) portParams() portParams {
	return portParams{
		Port:     c.ComPort,
		Mode:     c.serialMode(),
		Encoding: c.Encoding,
		Idle:     time.Duration(c.LineIdleMs) * time.Millisecond,
		MaxLine:  c.MaxLineBytes,
	}
}

// Open closes whatever is open and starts a read session on params.
func (a *App) Open(params portParams) error {
	if strings.TrimSpace(params.Port) == "" {
		return errNoDevice
	}
	a.Close("reconfiguring")
	if params.MaxLine <= 0 {
		params.MaxLine = 8192
	}
	if params.Idle <= 0 {
		params.Idle = 100 * time.Millisecond
	}
	quit := make(chan struct{})
	p, err := openSerialPort(params.Port, &params.Mode)
	if err != nil {
		a.mu.Lock()
		a.status = "error"
		a.lastErr = err.Error()
		autoReopen := a.cfg.AutoReopen
		if autoReopen {
			// Close() must be able to stop the retry loop below.
			a.quit = quit
		}
		a.mu.Unlock()
		a.emitSys(fmt.Sprintf("open %s failed: %v", params.Port, err))
		a.rtlog.Printf("serial open failed on %s: %v", params.Port, err)
		if autoReopen {
			a.emitSys(fmt.Sprintf("retrying %s in the background - another program may be holding the port", params.Port))
			go a.reopenLoop(params, quit, time.Second)
		}
		return err
	}
	a.startSession(p, quit, params, false)
	return nil
}

// startSession installs a freshly opened port and starts reading it.
func (a *App) startSession(p serialPort, quit chan struct{}, params portParams, reopened bool) {
	p.SetReadTimeout(readTimeout)
	a.mu.Lock()
	a.port = p
	a.quit = quit
	a.codec = newCodec(params.Encoding)
	a.params = params
	a.status = "open"
	a.lastErr = ""
	a.mu.Unlock()

	if reopened {
		a.emitSys(fmt.Sprintf("%s reopened", params.Port))
		a.rtlog.Printf("serial reopened: %s", params.Port)
	} else {
		a.emitSys(fmt.Sprintf("%s opened, %s, encoding %s", params.Port, modeLabel(params.Mode), encodingLabel(params.Encoding)))
		a.rtlog.Printf("serial open: %s %s", params.Port, modeLabel(params.Mode))
	}
	go a.readLoop(p, quit, params)
}

// reopenLoop retries opening the port with backoff until it succeeds or the
// session is closed. It covers both a port that was busy at startup and a
// device that vanished mid-session.
func (a *App) reopenLoop(params portParams, quit chan struct{}, backoff time.Duration) {
	for {
		select {
		case <-quit:
			return
		case <-time.After(backoff):
		}
		p, err := openSerialPort(params.Port, &params.Mode)
		if err != nil {
			if backoff < 4*time.Second {
				backoff *= 2
			}
			continue
		}
		select {
		case <-quit:
			p.Close()
			return
		default:
		}
		a.startSession(p, quit, params, true)
		return
	}
}

func (a *App) readLoop(p serialPort, quit chan struct{}, params portParams) {
	rx := newCodec(params.Encoding)
	fr := newFramer(params.MaxLine, params.Idle)
	buf := make([]byte, 4096)
	backoff := time.Second

	for {
		n, err := p.Read(buf)
		if n > 0 {
			a.handleRX(buf[:n], fr, rx, params.Port)
			backoff = time.Second
		}
		if err == nil {
			// Read timeout: nothing arrived, but a partial line may be stale.
			if n == 0 {
				for _, ch := range fr.FlushIdle(time.Now()) {
					a.record("rx", "device", ch, rx)
				}
			}
			continue
		}
		select {
		case <-quit:
			return
		default:
		}
		p.Close()
		for _, ch := range fr.Flush() {
			a.record("rx", "device", ch, rx)
		}
		a.mu.Lock()
		a.status = "error"
		a.lastErr = err.Error()
		if a.port == p {
			a.port = nil
		}
		autoReopen := a.cfg.AutoReopen
		a.mu.Unlock()
		a.emitSys(fmt.Sprintf("serial error on %s: %v", params.Port, err))
		a.rtlog.Printf("serial read error on %s: %v", params.Port, err)
		if !autoReopen {
			return
		}
		a.emitSys(fmt.Sprintf("waiting for %s to come back...", params.Port))
		a.reopenLoop(params, quit, backoff)
		return
	}
}

// Close stops the read session. reason only documents why in the log.
func (a *App) Close(reason string) {
	a.mu.Lock()
	p := a.port
	quit := a.quit
	wasOpen := p != nil || a.status == "open"
	a.port = nil
	a.quit = nil
	if a.status != "error" {
		a.status = "closed"
	}
	a.mu.Unlock()

	if quit != nil {
		close(quit)
	}
	if p != nil {
		p.Close()
	}
	if wasOpen && reason != "" {
		a.emitSys("closed: " + reason)
		a.rtlog.Printf("serial closed: %s", reason)
	}
}

// Reopen applies a new configuration to the serial layer.
func (a *App) Reopen(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	return a.Open(cfg.portParams())
}

func (a *App) handleRX(raw []byte, fr *framer, c *codec, src string) {
	if len(raw) == 0 {
		return
	}
	a.rxBytes.Add(int64(len(raw)))
	a.bus.publish(raw, dirRx, 0, a.onDrop)
	for _, ch := range fr.Push(raw) {
		a.record("rx", src, ch, c)
	}
}

func (a *App) onDrop(s *sub) {
	a.rtlog.Printf("subscriber %d (%s) queue full: dropping chunks", s.id, s.name)
}

// Send writes to the serial port and mirrors the bytes to every other
// subscriber. exclude suppresses the originating TCP client so it never
// receives its own echo.
func (a *App) Send(data []byte, src string, exclude int) error {
	if len(data) == 0 {
		return nil
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.Lock()
	p := a.port
	c := a.codec
	a.mu.Unlock()
	if p == nil {
		return errNoPort
	}
	out := data
	if c != nil {
		out = c.Encode(data)
	}
	if _, err := p.Write(out); err != nil {
		a.emitSys(fmt.Sprintf("write failed: %v", err))
		a.rtlog.Printf("serial write failed: %v", err)
		return err
	}
	a.txBytes.Add(int64(len(out)))
	a.bus.publish(out, dirTx, exclude, a.onDrop)
	for _, ch := range splitTX(out) {
		a.record("tx", src, ch, c)
	}
	return nil
}

// splitTX cuts a write into display lines without waiting for more input: the
// user must see a command even when it has no terminator.
func splitTX(data []byte) []chunk {
	var out []chunk
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			out = append(out, chunk{Data: data[start : i+1], Complete: true})
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, chunk{Data: data[start:], Complete: false})
	}
	return out
}

func (a *App) record(dir, src string, ch chunk, c *codec) {
	data := ch.Data
	ts := time.Now().UnixMilli()
	// The binary capture always stores raw wire bytes.
	if c != nil {
		data = c.Decode(data)
	}
	text, binary := displayText(data)
	rec := Rec{Ts: ts, Dir: dir, Src: src, N: len(ch.Data), Text: text, Partial: !ch.Complete}
	if binary {
		rec.Hex = hexString(trimTerminator(data))
	}
	rec = a.ring.Append(rec)
	a.cap.Write(rec)
	switch dir {
	case "rx":
		a.rxRecs.Add(1)
	case "tx":
		a.txRecs.Add(1)
	}
}

func (a *App) emitSys(msg string) {
	rec := a.ring.Append(Rec{Ts: time.Now().UnixMilli(), Dir: "sys", Src: "system", Text: msg})
	a.cap.Write(rec)
}

func encodingLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "utf-8"
	}
	return strings.ToLower(name)
}

// State is the JSON snapshot the UI polls and streams.
func (a *App) State() map[string]any {
	a.mu.Lock()
	cfg := a.cfg
	status := a.status
	lastErr := a.lastErr
	params := a.params
	open := a.port != nil
	a.mu.Unlock()

	count, bytes, total, dropped := a.ring.Stats()
	rx := a.rxBytes.Load()
	tx := a.txBytes.Load()
	state := map[string]any{
		"status":     status,
		"open":       open,
		"last_error": lastErr,
		"port":       params.Port,
		"baud":       cfg.BaudRate,
		"data_bits":  cfg.DataBits,
		"stop_bits":  cfg.StopBits,
		"parity":     cfg.Parity,
		"encoding":   cfg.Encoding,
		"rx_bytes":   rx,
		"tx_bytes":   tx,
		"rx_records": a.rxRecs.Load(),
		"tx_records": a.txRecs.Load(),
		"records":    count,
		"ring_bytes": bytes,
		"seq":        total,
		"evicted":    dropped,
		"clients":    a.bus.count(),
		"subscribers": a.bus.list(),
		"uptime_s":   int(time.Since(a.startedAt).Seconds()),
		"capture":    captureState(a),
		"config": map[string]any{
			"com_port":       cfg.ComPort,
			"baud_rate":      cfg.BaudRate,
			"data_bits":      cfg.DataBits,
			"stop_bits":      cfg.StopBits,
			"parity":         cfg.Parity,
			"encoding":       cfg.Encoding,
			"target_port":    cfg.TargetPort,
			"tap_port":       cfg.TapPort,
			"frame_port":     cfg.FramePort,
			"write_policy":   cfg.WritePolicy,
			"web_port":       cfg.WebPort,
			"capture_dir":    cfg.CaptureDir,
			"capture_default": cfg.CaptureEnabled,
		},
	}
	return state
}

// ListPorts returns the available serial port names.
func ListPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil || ports == nil {
		return []string{}
	}
	return ports
}

// captureState reports the text capture for the UI menu.
func captureState(a *App) map[string]any {
	on, path, dir := a.cap.Info()
	if dir == "" {
		a.mu.Lock()
		dir = a.cfg.CaptureDir
		a.mu.Unlock()
	}
	return map[string]any{"enabled": on, "path": path, "dir": dir}
}

// --- demo source -----------------------------------------------------------
// A synthetic device for UI work and for verifying the whole pipeline without
// hardware. It feeds the exact same framing/record path as the real port.

var demoLines = []string{
	"U-Boot 2024.01 (Nov 05 2026 - 09:12:33 +0800)",
	"[    0.000000] Booting Linux on physical CPU 0x0",
	"[    0.412331] printk: console [ttyMSM0] enabled",
	"[    1.902114] mmc0: new HS200 MMC card at address 0001",
	"[    2.113004] EXT4-fs (mmcblk0p12): mounted filesystem with ordered data mode",
	"[i] sensor init ok, chip id 0x5449",
	"[w] battery voltage 3.62V below warning threshold",
	"[i] modem: AT+CREG? -> +CREG: 0,1",
	"[e] nfc: card poll timeout (retry 2/3)",
	"[i] heartbeat 0000000000000000000000000000000000000000",
}

func (a *App) StartDemo() {
	a.mu.Lock()
	if a.demoStop != nil {
		a.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	a.demoStop = stop
	a.status = "demo"
	a.mu.Unlock()

	a.emitSys("demo source started (no serial port is open)")
	go func() {
		fr := newFramer(8192, 100*time.Millisecond)
		tick := time.NewTicker(220 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			i++
			var payload []byte
			switch {
			case i%37 == 0:
				payload = []byte{0x55, 0xaa, 0x01, 0x04, 0x00, 0x00, 0xfe, 0x1c}
			case i%13 == 0:
				payload = []byte(fmt.Sprintf("progress %3d%%", (i*7)%100)) // no terminator
			default:
				payload = []byte(demoLines[i%len(demoLines)] + "\r\n")
			}
			a.rxBytes.Add(int64(len(payload)))
			a.bus.publish(payload, dirRx, 0, a.onDrop)
			for _, ch := range fr.Push(payload) {
				a.record("rx", "demo", ch, nil)
			}
		}
	}()
}

func (a *App) StopDemo() {
	a.mu.Lock()
	stop := a.demoStop
	a.demoStop = nil
	if a.status == "demo" {
		a.status = "closed"
	}
	a.mu.Unlock()
	if stop != nil {
		close(stop)
		a.emitSys("demo source stopped")
	}
}

// SendHex parses a hex string ("55 AA 01" or "55AA01") into bytes.
func SendHex(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', ':', ',', '-', '0':
			if r == '0' {
				return r
			}
			return -1
		}
		return r
	}, s)
	if len(clean)%2 != 0 {
		return nil, fmt.Errorf("hex payload has an odd number of digits")
	}
	out := make([]byte, 0, len(clean)/2)
	for i := 0; i < len(clean); i += 2 {
		v, err := strconv.ParseUint(clean[i:i+2], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid hex at offset %d: %q", i, clean[i:i+2])
		}
		out = append(out, byte(v))
	}
	return out, nil
}
