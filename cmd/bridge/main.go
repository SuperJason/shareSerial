package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

const (
	defaultBaudRate  = 115200
	defaultTCPPort   = 2601
	defaultConfig    = "conf.json"
	maxCacheBytes    = 4 * 1024 * 1024
	reconnectDelay   = 2 * time.Second
	clientWriteWait  = 2 * time.Second
	serialReadWindow = 500 * time.Millisecond
	ctrlA            = byte(1)
	nameQueryTimeout = 350 * time.Millisecond
)

var commonBaudRates = []int{1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600}

type bridgeConfig struct {
	ComPort         string   `json:"com_port"`
	BaudRate        int      `json:"baud_rate"`
	LogFile         string   `json:"log_file"`
	TargetIP        string   `json:"target_ip"`
	TargetPort      int      `json:"target_port"`
	DataLogFile     string   `json:"data_log_file"`
	Hub4comPath     string   `json:"hub4com_path"`
	HubPhysicalPort string   `json:"hub_physical_port"`
	HubSlavePort    string   `json:"hub_slave_port"`
	HubExtraSlaves  []string `json:"hub_extra_slaves"`
	Hub4comLogFile  string   `json:"hub4com_log_file"`
	WebPort         int      `json:"web_port"`

	// FTDI CBUS VBUS control. Enabled by default: the tool board is part of
	// the fixture, so this is opt-out rather than opt-in.
	//
	// No virtual port is involved: the CBUS pin is driven inside a brief
	// window where hub4com has released the serial port, so the existing
	// port configuration is untouched.
	//
	// FtdiEnabled is a pointer because an absent key must mean "enabled" while
	// an explicit false must mean "disabled", and a plain bool cannot tell the
	// two apart after JSON decoding.
	FtdiEnabled   *bool       `json:"ftdi_enabled"`
	FtdiSerial    string      `json:"ftdi_serial"`     // legacy single-board serial; migrated into FtdiBoards
	FtdiCbusPin   int         `json:"ftdi_cbus_pin"`   // legacy single-board pin; migrated into FtdiBoards
	FtdiActiveLow bool        `json:"ftdi_active_low"` // legacy single-board polarity; migrated into FtdiBoards
	FtdiBoards    []ftdiBoard `json:"ftdi_boards"`     // per-board settings; every enumerated board not listed here uses the defaults
}

// ftdiBoard holds the VBUS wiring of one tool board. A zero Serial means "the
// defaults for boards not explicitly configured".
type ftdiBoard struct {
	Serial    string `json:"serial"`     // optional; empty = apply to any unconfigured board
	Pin       int    `json:"pin"`        // CBUS pin driving VBUS, 0-3
	ActiveLow bool   `json:"active_low"` // true = pin low turns VBUS on
}

// FTDIEnabled reports whether the CBUS feature is on. A missing key means
// enabled; only an explicit false turns the feature off.
func (c *bridgeConfig) FTDIEnabled() bool {
	return c.FtdiEnabled == nil || *c.FtdiEnabled
}

func boolPtr(b bool) *bool { return &b }

// Validate checks the config and applies safe defaults.
func (c *bridgeConfig) Validate() error {
	if strings.TrimSpace(c.ComPort) == "" {
		return fmt.Errorf("com_port is required")
	}
	if c.BaudRate <= 0 {
		return fmt.Errorf("baud_rate must be > 0")
	}
	if c.TargetPort <= 0 || c.TargetPort > 65535 {
		return fmt.Errorf("target_port must be between 1 and 65535")
	}
	if c.WebPort <= 0 || c.WebPort > 65535 {
		c.WebPort = 18080
	}
	if strings.TrimSpace(c.Hub4comPath) != "" && strings.TrimSpace(c.HubPhysicalPort) == "" {
		return fmt.Errorf("hub_physical_port is required in hub4com mode")
	}
	// Materialise the default so the setting is explicit from here on.
	if c.FtdiEnabled == nil {
		c.FtdiEnabled = boolPtr(true)
	}
	if c.FTDIEnabled() {
		if c.FtdiCbusPin < 0 || c.FtdiCbusPin >= cbusPinCount {
			return fmt.Errorf("ftdi_cbus_pin must be between 0 and %d", cbusPinCount-1)
		}
		// Migrate the legacy single-board keys into the boards list so the
		// rest of the code only deals with one shape.
		if strings.TrimSpace(c.FtdiSerial) != "" {
			if !c.hasFtdiBoard(c.FtdiSerial) {
				c.FtdiBoards = append(c.FtdiBoards, ftdiBoard{
					Serial:    strings.TrimSpace(c.FtdiSerial),
					Pin:       c.FtdiCbusPin,
					ActiveLow: c.FtdiActiveLow,
				})
			}
		}
		for _, b := range c.FtdiBoards {
			if b.Pin < 0 || b.Pin >= cbusPinCount {
				return fmt.Errorf("ftdi_cbus_pin must be between 0 and %d (board %q)", cbusPinCount-1, b.Serial)
			}
		}
		// The pin can only be reached by taking the device over D2XX, which
		// needs hub4com's restart machinery to release the port. Without
		// hub4com there is nothing to pause, so the feature is skipped rather
		// than failed: a plain single-port config must keep working.
		if strings.TrimSpace(c.Hub4comPath) == "" {
			c.FtdiEnabled = boolPtr(false)
		}
	}
	return nil
}

// hasFtdiBoard reports whether the boards list already carries settings for
// the given serial.
func (c *bridgeConfig) hasFtdiBoard(serial string) bool {
	for _, b := range c.FtdiBoards {
		if strings.EqualFold(strings.TrimSpace(b.Serial), strings.TrimSpace(serial)) {
			return true
		}
	}
	return false
}

// ftdiBoardFor returns the settings for a board with the given serial: an
// explicit entry when present, otherwise the legacy defaults (so old configs
// behave identically) and then the zero-value defaults.
func (c *bridgeConfig) ftdiBoardFor(serial string) ftdiBoard {
	for _, b := range c.FtdiBoards {
		if strings.EqualFold(strings.TrimSpace(b.Serial), strings.TrimSpace(serial)) {
			return b
		}
	}
	if strings.TrimSpace(c.FtdiSerial) != "" || c.FtdiCbusPin != 0 || c.FtdiActiveLow {
		return ftdiBoard{Pin: c.FtdiCbusPin, ActiveLow: c.FtdiActiveLow}
	}
	return ftdiBoard{}
}

type serialManager struct {
	mu       sync.RWMutex
	port     serial.Port
	portName string
	baudRate int
}

func (m *serialManager) Configure(portName string, baud int) error {
	newPort, err := openSerial(portName, baud)
	if err != nil {
		return err
	}

	m.mu.Lock()
	old := m.port
	m.port = newPort
	m.portName = portName
	m.baudRate = baud
	m.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	return nil
}

func (m *serialManager) Read(buf []byte) (int, error) {
	p := m.currentPort()
	if p == nil {
		return 0, errors.New("serial port not open")
	}
	return p.Read(buf)
}

func (m *serialManager) Write(data []byte) error {
	p := m.currentPort()
	if p == nil {
		return errors.New("serial port not open")
	}
	return writeAll(p, data)
}

func (m *serialManager) Close() {
	m.mu.Lock()
	old := m.port
	m.port = nil
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (m *serialManager) Snapshot() (string, int) {
	m.mu.RLock()
	name := m.portName
	baud := m.baudRate
	m.mu.RUnlock()
	return name, baud
}

func (m *serialManager) currentPort() serial.Port {
	m.mu.RLock()
	p := m.port
	m.mu.RUnlock()
	return p
}

type packetLogger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func (l *packetLogger) SetFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	l.mu.Lock()
	old := l.file
	l.file = f
	l.path = path
	l.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	return nil
}

func (l *packetLogger) Log(direction string, data []byte) {
	if len(data) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	line := fmt.Sprintf("%s %s len=%d hex=%s\n", time.Now().Format(time.RFC3339Nano), direction, len(data), strings.ToUpper(hex.EncodeToString(data)))
	_, _ = l.file.WriteString(line)
}

func (l *packetLogger) Close() {
	l.mu.Lock()
	old := l.file
	l.file = nil
	l.path = ""
	l.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

type bridgeApp struct {
	logger       *log.Logger
	configPath   string
	cfg          bridgeConfig
	socketMode   string
	remoteAddr   string
	serial       *serialManager
	hub          *clientHub
	hub4com      *hub4comManager
	ftdi         *ftdiManager
	cache        *fifoBuffer
	dataLog      *packetLogger
	clientSignal chan struct{}
	stopCh       chan struct{}
	stopOnce     sync.Once
	ln           net.Listener
	serialWrite  sync.Mutex
	stdinMu      sync.Mutex
	configMu     sync.Mutex
}

// main is the entry point. It parses flags, loads config and dispatches to
// tray/web mode (-tray) or the classic console CLI mode.
func main() {
	// No arguments at all: behave as the tray app with the project config,
	// i.e. "bridge_hub_tray.exe -tray -config bridge_com23.json". Any explicit
	// argument (including just -tray or just -config) keeps the classic flag
	// parsing and its defaults.
	if len(os.Args) == 1 {
		os.Args = append(os.Args, "-tray", "-config", "bridge_com23.json")
	}
	configFile := flag.String("config", defaultConfigPath(), "Local config file path")
	comPort := flag.String("p", "", "COM port number or name (e.g. 3 or COM3)")
	baudRate := flag.Int("b", 0, "Baud rate")
	logFile := flag.String("log", "", "Optional log file name")
	targetIP := flag.String("ip", "", "TCP bind/listen IP address")
	targetPort := flag.Int("P", 0, "TCP listen port")
	scanPorts := flag.Bool("scan", false, "Scan COM ports and select one from console menu")
	listPorts := flag.Bool("list", false, "List COM ports and exit")
	hub4comPath := flag.String("hub4com", "", "Path to hub4com.exe; when set, bridge spawns and supervises hub4com")
	hubPhysical := flag.String("phy", "", "Physical COM port hub4com opens (hub4com mode)")
	hubSlave := flag.String("hub-slave", "", "com0com pair end hub4com attaches to")
	hubSlaveExtra := flag.String("hub-slave-extra", "", "Comma-separated extra hub4com slave ports")
	trayMode := flag.Bool("tray", false, "Run in system tray without a console window (build with -H windowsgui)")
	webPort := flag.Int("web", 0, "Web UI port (default 18080)")
	autoStart := flag.Bool("autostart", true, "Auto-start bridge in tray/web mode")
	flag.Parse()

	if *listPorts {
		if err := printCOMPortList(); err != nil {
			fmt.Fprintf(os.Stderr, "list COM ports failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	stored, err := loadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config file %s: %v\n", *configFile, err)
		os.Exit(1)
	}

	cfg := mergeConfig(stored, *comPort, *baudRate, *logFile, *targetIP, *targetPort, *hub4comPath, *hubPhysical, *hubSlave, *hubSlaveExtra)
	if *scanPorts {
		selected, scanErr := scanAndSelectCOMPort()
		if scanErr != nil {
			fmt.Fprintf(os.Stderr, "scan failed: %v\n", scanErr)
			os.Exit(1)
		}
		cfg.ComPort = selected
	}

	if cfg.BaudRate <= 0 {
		cfg.BaudRate = defaultBaudRate
	}
	if cfg.TargetPort <= 0 {
		cfg.TargetPort = defaultTCPPort
	}
	if cfg.WebPort <= 0 {
		cfg.WebPort = 18080
	}
	if *webPort > 0 {
		cfg.WebPort = *webPort
	}

	logger, closeLogger, err := newLogger(cfg.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLogger()

	if *trayMode {
		runTrayMode(*configFile, cfg, logger, *autoStart)
		return
	}

	cfg.ComPort = normalizePortName(cfg.ComPort)
	if cfg.ComPort == "" {
		logger.Fatal("missing required -p COM port parameter")
	}
	if cfg.BaudRate <= 0 {
		logger.Fatal("invalid baud rate, must be > 0")
	}
	if cfg.TargetPort <= 0 || cfg.TargetPort > 65535 {
		logger.Fatal("invalid TCP port, must be between 1 and 65535")
	}
	ipSpecified := flagProvided("ip")
	if ipSpecified && strings.TrimSpace(cfg.TargetIP) == "" {
		logger.Fatal("-ip provided but value is empty")
	}

	app, err := newBridgeApp(*configFile, cfg, logger)
	if err != nil {
		logger.Fatal(err)
	}
	app.run()
	app.Stop()
}

// runTrayMode starts the web UI + system tray and blocks until the user quits.
func runTrayMode(configFile string, cfg bridgeConfig, logger *log.Logger, autoStart bool) {
	ctrl := newAppController(configFile, cfg, logger, cfg.WebPort)
	if autoStart {
		if err := ctrl.Start(); err != nil {
			logger.Printf("[tray] autostart failed: %v", err)
		}
	}
	srv, _, err := startWebServer(ctrl, logger)
	if err != nil {
		logger.Printf("[web] failed to start web UI: %v", err)
	} else {
		defer srv.Close()
	}
	runTray(ctrl, logger)
}

// newBridgeApp builds a fully initialized bridgeApp (serial + TCP listener +
// optional hub4com supervisor) and starts its background goroutines. It is the
// shared constructor for both CLI and tray/web modes; errors are returned
// instead of exiting the process.
func newBridgeApp(configPath string, cfg bridgeConfig, logger *log.Logger) (*bridgeApp, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ComPort = normalizePortName(cfg.ComPort)
	hubPhysicalPort := normalizePortName(cfg.HubPhysicalPort)
	hubSlavePort := normalizePortName(cfg.HubSlavePort)

	serialMgr := &serialManager{}
	var hub4comMgr *hub4comManager

	// Opt-in FTDI CBUS VBUS control. It is constructed before hub4com starts
	// so the boards can be detected while the serial ports are still closed.
	// Skipped entirely unless ftdi_enabled is set, so with the feature off
	// nothing here changes any existing behaviour.
	var ftdiMgr *ftdiManager
	if cfg.FTDIEnabled() {
		ftdiMgr = newFTDIManager(ftdiOptions{
			Boards: cfg.FtdiBoards,
			// Legacy single-board keys act as the defaults for boards that
			// have no explicit ftdi_boards entry.
			DefaultPin:       cfg.FtdiCbusPin,
			DefaultActiveLow: cfg.FtdiActiveLow,
			DefaultSerial:    cfg.FtdiSerial,
		}, logger)
		if err := ftdiMgr.Validate(); err != nil {
			return nil, fmt.Errorf("ftdi setup failed: %w", err)
		}
		for _, b := range ftdiMgr.Boards() {
			logger.Printf("ftdi: detected %s serial=%s (%s)", b.Info.TypeName(), b.Info.Serial, b.Info.Description)
		}
		if ftdiMgr.BoardCount() == 0 {
			logger.Printf("ftdi: no board detected yet; boards are picked up automatically once plugged in")
		}
	}

	hubMode := strings.TrimSpace(cfg.Hub4comPath) != ""
	if hubMode {
		if hubSlavePort == "" {
			hubSlavePort = peerPortName(normalizePortName(cfg.ComPort))
		}
		if hubPhysicalPort == "" {
			return nil, fmt.Errorf("hub4com mode requires -phy <physical COM port> (or hub_physical_port in config)")
		}
		if hubSlavePort == "" {
			return nil, fmt.Errorf("cannot derive hub4com slave port from -p %s; specify -hub-slave", cfg.ComPort)
		}
		hubLog := strings.TrimSpace(cfg.Hub4comLogFile)
		if hubLog == "" && strings.TrimSpace(cfg.LogFile) != "" {
			hubLog = filepath.Join(filepath.Dir(cfg.LogFile), "hub4com.log")
		}
		var extraSlaves []string
		for _, s := range cfg.HubExtraSlaves {
			if n := normalizePortName(s); n != "" {
				extraSlaves = append(extraSlaves, n)
			}
		}
		hub4comMgr = newHub4comManager(cfg.Hub4comPath, hubPhysicalPort, hubSlavePort, normalizePortName(cfg.ComPort), extraSlaves, cfg.BaudRate, hubLog, logger)
		hub4comMgr.onReady = func() error {
			serialMgr.Close()
			return serialMgr.Configure(hub4comMgr.currentBridgePort(), hub4comMgr.currentBaud())
		}
		if err := hub4comMgr.Start(); err != nil {
			return nil, fmt.Errorf("hub4com setup failed: %w", err)
		}
		select {
		case err := <-hub4comMgr.Ready():
			if err != nil {
				hub4comMgr.Stop()
				return nil, fmt.Errorf("hub4com startup failed: %w (is the physical port in use by another app?)", err)
			}
		case <-time.After(25 * time.Second):
			hub4comMgr.Stop()
			return nil, fmt.Errorf("hub4com startup timed out")
		}
		logger.Printf("hub4com mode: physical=%s -> hub4com -> slaves %v, bridge serial=%s",
			hubPhysicalPort, append(append([]string{}, hubSlavePort), extraSlaves...), normalizePortName(cfg.ComPort))
	} else {
		if err := serialMgr.Configure(cfg.ComPort, cfg.BaudRate); err != nil {
			return nil, fmt.Errorf("failed opening serial port %s: %w", cfg.ComPort, err)
		}
	}

	if ftdiMgr != nil {
		// Attached only now: the manager needs hub4com's restart machinery to
		// free the serial port, which does not exist until hub4com has started.
		ftdiMgr.hub = hub4comMgr
		ftdiMgr.StartWatcher()
		logger.Printf("ftdi mode enabled: CBUS pins drive the DUT VBUS on %d board(s); each toggle interrupts the serial bridge for about %s",
			ftdiMgr.BoardCount(), hubRestartBackoff)
	}

	modeName := "server"
	listenAddress := net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.TargetPort))
	remoteAddress := ""
	var ln net.Listener
	if strings.TrimSpace(cfg.TargetIP) != "" {
		modeName = "client"
		remoteAddress = net.JoinHostPort(cfg.TargetIP, strconv.Itoa(cfg.TargetPort))
	} else {
		var err error
		ln, err = net.Listen("tcp", listenAddress)
		if err != nil {
			serialMgr.Close()
			if hub4comMgr != nil {
				hub4comMgr.Stop()
			}
			return nil, fmt.Errorf("failed to listen on %s: %w", listenAddress, err)
		}
	}

	app := &bridgeApp{
		logger:       logger,
		configPath:   configPath,
		cfg:          cfg,
		socketMode:   modeName,
		remoteAddr:   remoteAddress,
		serial:       serialMgr,
		hub:          newClientHub(),
		hub4com:      hub4comMgr,
		ftdi:         ftdiMgr,
		cache:        newFIFOBuffer(maxCacheBytes),
		dataLog:      &packetLogger{},
		clientSignal: make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		ln:           ln,
	}

	if strings.TrimSpace(cfg.DataLogFile) != "" {
		if err := app.dataLog.SetFile(cfg.DataLogFile); err != nil {
			logger.Printf("warning: failed to open data log file %s: %v", cfg.DataLogFile, err)
		} else {
			logger.Printf("data log enabled: %s", cfg.DataLogFile)
		}
	}

	if err := app.saveConfig(); err != nil {
		app.Stop()
		return nil, fmt.Errorf("failed to save config file %s: %w", configPath, err)
	}

	go app.serialToClients()
	go app.controlLoop()

	if app.socketMode == "server" {
		logger.Printf("bridge started [server mode]: serial=%s baud=%d <-> tcp listen=%s", cfg.ComPort, cfg.BaudRate, listenAddress)
	} else {
		logger.Printf("bridge started [client mode]: serial=%s baud=%d -> tcp=%s", cfg.ComPort, cfg.BaudRate, app.remoteAddr)
	}
	logger.Printf("cache enabled: up to %d bytes while no client connected", maxCacheBytes)
	logger.Printf("shortcut: press Ctrl+A to open menu")
	logger.Printf("fallback command: type /menu then Enter if terminal does not deliver Ctrl+A")
	return app, nil
}

// run blocks serving until Stop() is called (CLI mode).
func (a *bridgeApp) run() {
	if a.socketMode == "server" {
		a.acceptLoop()
	} else {
		a.connectLoop()
	}
}

// mainCLI is the original console entry point, kept for full CLI compatibility.
func mainCLI() {
	configFile := flag.String("config", defaultConfigPath(), "Local config file path")
	comPort := flag.String("p", "", "COM port number or name (e.g. 3 or COM3)")
	baudRate := flag.Int("b", 0, "Baud rate")
	logFile := flag.String("log", "", "Optional log file name")
	targetIP := flag.String("ip", "", "TCP bind/listen IP address")
	targetPort := flag.Int("P", 0, "TCP listen port")
	scanPorts := flag.Bool("scan", false, "Scan COM ports and select one from console menu")
	listPorts := flag.Bool("list", false, "List COM ports and exit")
	hub4comPath := flag.String("hub4com", "", "Path to hub4com.exe; when set, bridge spawns and supervises hub4com to share a physical COM port")
	hubPhysical := flag.String("phy", "", "Physical COM port hub4com opens (hub4com mode)")
	hubSlave := flag.String("hub-slave", "", "com0com pair end hub4com attaches to; default: peer of -p (hub4com mode)")
	hubSlaveExtra := flag.String("hub-slave-extra", "", "Comma-separated extra hub4com slave ports for other consumers (e.g. local serial monitor); each needs its own com0com pair (hub4com mode)")
	flag.Parse()

	if *listPorts {
		if err := printCOMPortList(); err != nil {
			fmt.Fprintf(os.Stderr, "list COM ports failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	stored, err := loadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config file %s: %v\n", *configFile, err)
		os.Exit(1)
	}

	cfg := mergeConfig(stored, *comPort, *baudRate, *logFile, *targetIP, *targetPort, *hub4comPath, *hubPhysical, *hubSlave, *hubSlaveExtra)
	if *scanPorts {
		selected, scanErr := scanAndSelectCOMPort()
		if scanErr != nil {
			fmt.Fprintf(os.Stderr, "scan failed: %v\n", scanErr)
			os.Exit(1)
		}
		cfg.ComPort = selected
	}

	if cfg.BaudRate <= 0 {
		cfg.BaudRate = defaultBaudRate
	}
	if cfg.TargetPort <= 0 {
		cfg.TargetPort = defaultTCPPort
	}

	logger, closeLogger, err := newLogger(cfg.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer closeLogger()

	cfg.ComPort = normalizePortName(cfg.ComPort)
	if cfg.ComPort == "" {
		logger.Fatal("missing required -p COM port parameter")
	}
	if cfg.BaudRate <= 0 {
		logger.Fatal("invalid baud rate, must be > 0")
	}
	if cfg.TargetPort <= 0 || cfg.TargetPort > 65535 {
		logger.Fatal("invalid TCP port, must be between 1 and 65535")
	}

	ipSpecified := flagProvided("ip")
	if ipSpecified && strings.TrimSpace(cfg.TargetIP) == "" {
		logger.Fatal("-ip provided but value is empty")
	}

	serialMgr := &serialManager{}
	var hub4comMgr *hub4comManager
	hubMode := strings.TrimSpace(cfg.Hub4comPath) != ""
	if hubMode {
		hubPhysicalPort := normalizePortName(cfg.HubPhysicalPort)
		hubSlavePort := normalizePortName(cfg.HubSlavePort)
		if hubSlavePort == "" {
			hubSlavePort = peerPortName(normalizePortName(cfg.ComPort))
		}
		if hubPhysicalPort == "" {
			logger.Fatal("hub4com mode requires -phy <physical COM port> (or hub_physical_port in config)")
		}
		if hubSlavePort == "" {
			logger.Fatalf("cannot derive hub4com slave port from -p %s; specify -hub-slave", cfg.ComPort)
		}
		hubLog := strings.TrimSpace(cfg.Hub4comLogFile)
		if hubLog == "" && strings.TrimSpace(cfg.LogFile) != "" {
			hubLog = filepath.Join(filepath.Dir(cfg.LogFile), "hub4com.log")
		}
		var extraSlaves []string
		for _, s := range cfg.HubExtraSlaves {
			if n := normalizePortName(s); n != "" {
				extraSlaves = append(extraSlaves, n)
			}
		}
		hub4comMgr = newHub4comManager(cfg.Hub4comPath, hubPhysicalPort, hubSlavePort, normalizePortName(cfg.ComPort), extraSlaves, cfg.BaudRate, hubLog, logger)
		hub4comMgr.onReady = func() error {
			serialMgr.Close()
			return serialMgr.Configure(hub4comMgr.currentBridgePort(), hub4comMgr.currentBaud())
		}
		if err := hub4comMgr.Start(); err != nil {
			logger.Fatalf("hub4com setup failed: %v", err)
		}
		select {
		case err := <-hub4comMgr.Ready():
			if err != nil {
				hub4comMgr.Stop()
				logger.Fatalf("hub4com startup failed: %v (is the physical port in use by another app?)", err)
			}
		case <-time.After(25 * time.Second):
			hub4comMgr.Stop()
			logger.Fatal("hub4com startup timed out")
		}
		logger.Printf("hub4com mode: physical=%s -> hub4com -> slaves %v, bridge serial=%s",
			hubPhysicalPort, append(append([]string{}, hubSlavePort), extraSlaves...), normalizePortName(cfg.ComPort))
	} else {
		if err := serialMgr.Configure(cfg.ComPort, cfg.BaudRate); err != nil {
			logger.Fatalf("failed opening serial port %s: %v", cfg.ComPort, err)
		}
	}

	modeName := "server"
	listenAddress := net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.TargetPort))
	remoteAddress := ""
	var ln net.Listener

	if ipSpecified {
		modeName = "client"
		remoteAddress = net.JoinHostPort(cfg.TargetIP, strconv.Itoa(cfg.TargetPort))
	} else {
		ln, err = net.Listen("tcp", listenAddress)
		if err != nil {
			serialMgr.Close()
			if hub4comMgr != nil {
				hub4comMgr.Stop()
			}
			logger.Fatalf("failed to listen on %s: %v", listenAddress, err)
		}
	}

	app := &bridgeApp{
		logger:       logger,
		configPath:   *configFile,
		cfg:          cfg,
		socketMode:   modeName,
		remoteAddr:   remoteAddress,
		serial:       serialMgr,
		hub:          newClientHub(),
		hub4com:      hub4comMgr,
		cache:        newFIFOBuffer(maxCacheBytes),
		dataLog:      &packetLogger{},
		clientSignal: make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		ln:           ln,
	}

	if strings.TrimSpace(cfg.DataLogFile) != "" {
		if err := app.dataLog.SetFile(cfg.DataLogFile); err != nil {
			app.logger.Printf("warning: failed to open data log file %s: %v", cfg.DataLogFile, err)
		} else {
			app.logger.Printf("data log enabled: %s", cfg.DataLogFile)
		}
	}

	if err := app.saveConfig(); err != nil {
		app.Stop()
		app.logger.Fatalf("failed to save config file %s: %v", *configFile, err)
	}

	go app.serialToClients()
	go app.controlLoop()

	if app.socketMode == "server" {
		app.logger.Printf("bridge started [server mode]: serial=%s baud=%d <-> tcp listen=%s", cfg.ComPort, cfg.BaudRate, listenAddress)
	} else {
		app.logger.Printf("bridge started [client mode]: serial=%s baud=%d -> tcp=%s", cfg.ComPort, cfg.BaudRate, app.remoteAddr)
	}
	app.logger.Printf("cache enabled: up to %d bytes while no client connected", maxCacheBytes)
	app.logger.Printf("shortcut: press Ctrl+A to open menu")
	app.logger.Printf("fallback command: type /menu then Enter if terminal does not deliver Ctrl+A")

	if app.socketMode == "server" {
		app.acceptLoop()
	} else {
		app.connectLoop()
	}
	app.Stop()
}

func (a *bridgeApp) connectLoop() {
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		if a.hub.Count() > 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		conn, err := net.DialTimeout("tcp", a.remoteAddr, 3*time.Second)
		if err != nil {
			a.logger.Printf("tcp connect failed (%s): %v", a.remoteAddr, err)
			time.Sleep(reconnectDelay)
			continue
		}

		a.hub.Add(conn)
		notifyClientConnected(a.clientSignal)
		a.logger.Printf("tcp connected: %s", a.remoteAddr)
		go a.clientToSerial(conn)

		for {
			select {
			case <-a.stopCh:
				return
			default:
			}
			if a.hub.Count() == 0 {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (a *bridgeApp) acceptLoop() {
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			select {
			case <-a.stopCh:
				return
			default:
			}
			a.logger.Printf("accept failed: %v", err)
			time.Sleep(reconnectDelay)
			continue
		}

		a.hub.Add(conn)
		notifyClientConnected(a.clientSignal)
		a.logger.Printf("client connected: %s (total=%d)", conn.RemoteAddr(), a.hub.Count())
		go a.clientToSerial(conn)
	}
}

func (a *bridgeApp) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
		if a.ln != nil {
			_ = a.ln.Close()
		}
		a.hub.CloseAll()
		a.serial.Close()
		if a.hub4com != nil {
			a.hub4com.Stop()
		}
		if a.ftdi != nil {
			a.ftdi.Stop()
		}
		a.dataLog.Close()
	})
}

func (a *bridgeApp) serialToClients() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		a.drainCacheIfNeeded()

		n, err := a.serial.Read(buf)
		if err != nil {
			if isTimeout(err) || strings.Contains(strings.ToLower(err.Error()), "not open") {
				select {
				case <-a.clientSignal:
					a.drainCacheIfNeeded()
				default:
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}

			select {
			case <-a.stopCh:
				return
			default:
			}

			a.logger.Printf("serial read failed: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if n == 0 {
			continue
		}

		chunk := append([]byte(nil), buf[:n]...)
		a.dataLog.Log("RX_SERIAL", chunk)

		if a.hub.Count() == 0 {
			a.cache.Append(chunk)
			continue
		}

		a.drainCacheIfNeeded()
		a.hub.Broadcast(chunk, a.logger)
	}
}

func (a *bridgeApp) drainCacheIfNeeded() {
	if a.hub.Count() == 0 || a.cache.Len() == 0 {
		return
	}

	cached := a.cache.Drain()
	if len(cached) == 0 {
		return
	}

	a.logger.Printf("flushing %d cached bytes to %d clients", len(cached), a.hub.Count())
	a.hub.Broadcast(cached, a.logger)
}

func (a *bridgeApp) clientToSerial(conn net.Conn) {
	defer func() {
		a.hub.Remove(conn)
		_ = conn.Close()
		a.logger.Printf("client disconnected: %s (total=%d)", conn.RemoteAddr(), a.hub.Count())
	}()

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			a.serialWrite.Lock()
			writeErr := a.serial.Write(chunk)
			a.serialWrite.Unlock()
			if writeErr != nil {
				a.logger.Printf("serial write failed from %s: %v", conn.RemoteAddr(), writeErr)
				return
			}
			a.dataLog.Log("TX_SERIAL", chunk)
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				a.logger.Printf("client read failed from %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
	}
}

func (a *bridgeApp) controlLoop() {
	reader := bufio.NewReader(os.Stdin)
	lineBuf := make([]byte, 0, 64)

	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			// No interactive stdin (e.g. -H windowsgui build): the Ctrl+A
			// menu is unavailable, so stop instead of spamming errors.
			a.logger.Printf("control input unavailable (%v); interactive menu disabled", err)
			return
		}

		if b == ctrlA {
			a.stdinMu.Lock()
			a.showMenu(reader)
			a.stdinMu.Unlock()
			lineBuf = lineBuf[:0]
			continue
		}

		lineBuf = append(lineBuf, b)
		if b == '\n' {
			cmd := strings.TrimSpace(string(lineBuf))
			if strings.EqualFold(cmd, "/menu") {
				a.stdinMu.Lock()
				a.showMenu(reader)
				a.stdinMu.Unlock()
			}
			lineBuf = lineBuf[:0]
		}
	}
}

func (a *bridgeApp) showMenu(reader *bufio.Reader) {
	for {
		fmt.Println()
		fmt.Println("========== Bridge Menu ==========")
		fmt.Println("1) Stop and exit")
		fmt.Println("2) Close serial port and select another COM port")
		fmt.Println("3) Change baudrate")
		fmt.Println("4) Send a local file to all socket clients")
		fmt.Println("5) Log rx/tx data to file")
		fmt.Println("0) Back")
		fmt.Print("Select [0-5]: ")

		choice, err := readLine(reader)
		if err != nil {
			a.logger.Printf("menu input error: %v", err)
			return
		}

		switch normalizeMenuChoice(choice) {
		case "1":
			a.logger.Println("menu: stop and exit")
			a.Stop()
			return
		case "2":
			a.menuSelectComPort(reader)
		case "3":
			a.menuChangeBaudrate(reader)
		case "4":
			a.menuSendFile(reader)
		case "5":
			a.menuEnableDataLog(reader)
		case "0", "":
			return
		default:
			fmt.Println("Invalid choice")
		}

		select {
		case <-a.stopCh:
			return
		default:
		}
	}
}

func normalizeMenuChoice(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return ""
	}

	for _, r := range trimmed {
		if r >= '0' && r <= '9' {
			return string(r)
		}
	}

	return ""
}

func (a *bridgeApp) menuSelectComPort(reader *bufio.Reader) {
	ports, err := serial.GetPortsList()
	if err != nil {
		a.logger.Printf("list COM ports failed: %v", err)
		return
	}
	if len(ports) == 0 {
		a.logger.Println("no serial ports found")
		return
	}

	sort.Strings(ports)
	friendlyNames := getCOMFriendlyNames(ports)
	fmt.Println("Available COM ports:")
	for i, p := range ports {
		display := p
		if friendly, ok := friendlyNames[strings.ToUpper(p)]; ok && strings.TrimSpace(friendly) != "" {
			display = fmt.Sprintf("%s - %s", p, friendly)
		}
		fmt.Printf("  %d) %s\n", i+1, display)
	}
	fmt.Printf("Select COM port [1-%d]: ", len(ports))

	line, err := readLine(reader)
	if err != nil {
		a.logger.Printf("read COM selection failed: %v", err)
		return
	}
	idx, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || idx < 1 || idx > len(ports) {
		a.logger.Println("invalid COM selection")
		return
	}

	selected := normalizePortName(ports[idx-1])

	if a.hub4com != nil {
		// hub4com mode: the selected port is the PHYSICAL port; hub4com
		// restarts with it and the bridge serial reopens automatically.
		a.configMu.Lock()
		a.cfg.HubPhysicalPort = selected
		a.configMu.Unlock()
		if err := a.saveConfig(); err != nil {
			a.logger.Printf("save config failed: %v", err)
		}
		a.hub4com.RestartPhysical(selected)
		a.logger.Printf("hub4com physical port switched to %s", selected)
		return
	}

	_, baud := a.serial.Snapshot()
	if err := a.serial.Configure(selected, baud); err != nil {
		a.logger.Printf("reopen serial failed: %v", err)
		return
	}

	a.configMu.Lock()
	a.cfg.ComPort = selected
	a.configMu.Unlock()
	if err := a.saveConfig(); err != nil {
		a.logger.Printf("save config failed: %v", err)
	}
	a.logger.Printf("serial switched to %s @ %d", selected, baud)
}

func (a *bridgeApp) menuChangeBaudrate(reader *bufio.Reader) {
	fmt.Println("Common baud rates:")
	for i, rate := range commonBaudRates {
		fmt.Printf("  %d) %d\n", i+1, rate)
	}
	fmt.Print("Select number or input custom baudrate: ")

	line, err := readLine(reader)
	if err != nil {
		a.logger.Printf("read baud input failed: %v", err)
		return
	}

	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		a.logger.Println("empty baud input")
		return
	}

	baud, parseErr := strconv.Atoi(trimmed)
	if parseErr == nil && baud >= 1 && baud <= len(commonBaudRates) {
		baud = commonBaudRates[baud-1]
	} else {
		baud, parseErr = strconv.Atoi(trimmed)
		if parseErr != nil || baud <= 0 {
			a.logger.Println("invalid baudrate")
			return
		}
	}

	portName, _ := a.serial.Snapshot()

	a.configMu.Lock()
	a.cfg.BaudRate = baud
	a.configMu.Unlock()
	if err := a.saveConfig(); err != nil {
		a.logger.Printf("save config failed: %v", err)
	}

	if a.hub4com != nil {
		// hub4com mode: restart hub4com with the new baud; the bridge
		// serial reopens automatically via onReady.
		a.hub4com.SetBaud(baud)
		a.logger.Printf("hub4com baudrate changed: %d (bridge serial reopens)", baud)
		return
	}

	if err := a.serial.Configure(portName, baud); err != nil {
		a.logger.Printf("reopen serial failed: %v", err)
		return
	}
	a.logger.Printf("serial baudrate changed: %s @ %d", portName, baud)
}

func (a *bridgeApp) menuSendFile(reader *bufio.Reader) {
	fmt.Print("Input file name to send (Enter to cancel): ")
	path, err := readLine(reader)
	if err != nil {
		a.logger.Printf("read file path failed: %v", err)
		return
	}

	path = strings.TrimSpace(path)
	if path == "" {
		a.logger.Println("send file canceled")
		return
	}

	content, err := os.ReadFile(path)
	if err != nil {
		a.logger.Printf("read file failed: %v", err)
		return
	}

	if len(content) == 0 {
		a.logger.Printf("file is empty: %s", path)
		return
	}

	a.hub.Broadcast(content, a.logger)
	a.dataLog.Log("TX_SOCKET_FILE", content)
	a.logger.Printf("sent file %s (%d bytes) to %d clients", path, len(content), a.hub.Count())
}

func (a *bridgeApp) menuEnableDataLog(reader *bufio.Reader) {
	fmt.Print("Input data log file name (Enter to cancel): ")
	path, err := readLine(reader)
	if err != nil {
		a.logger.Printf("read data log file path failed: %v", err)
		return
	}

	path = strings.TrimSpace(path)
	if path == "" {
		a.logger.Println("data log setup canceled")
		return
	}

	if err := a.dataLog.SetFile(path); err != nil {
		a.logger.Printf("open data log file failed: %v", err)
		return
	}

	a.configMu.Lock()
	a.cfg.DataLogFile = path
	a.configMu.Unlock()
	if err := a.saveConfig(); err != nil {
		a.logger.Printf("save config failed: %v", err)
	}
	a.logger.Printf("data log enabled: %s", path)
}

func (a *bridgeApp) saveConfig() error {
	a.configMu.Lock()
	cfg := a.cfg
	a.configMu.Unlock()
	return saveConfig(a.configPath, cfg)
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func openSerial(portName string, baud int) (serial.Port, error) {
	mode := &serial.Mode{BaudRate: baud}
	sp, err := serial.Open(portName, mode)
	if err != nil {
		return nil, err
	}
	if err := sp.SetReadTimeout(serialReadWindow); err != nil {
		_ = sp.Close()
		return nil, err
	}
	return sp, nil
}

func mergeConfig(stored bridgeConfig, comPort string, baudRate int, logFile, targetIP string, targetPort int, hub4comPath, hubPhysical, hubSlave, hubSlaveExtra string) bridgeConfig {
	cfg := stored

	if strings.TrimSpace(comPort) != "" {
		cfg.ComPort = comPort
	}
	if baudRate > 0 {
		cfg.BaudRate = baudRate
	}
	if strings.TrimSpace(logFile) != "" {
		cfg.LogFile = logFile
	}
	if strings.TrimSpace(targetIP) != "" {
		cfg.TargetIP = targetIP
	}
	if targetPort > 0 {
		cfg.TargetPort = targetPort
	}
	if strings.TrimSpace(hub4comPath) != "" {
		cfg.Hub4comPath = hub4comPath
	}
	if strings.TrimSpace(hubPhysical) != "" {
		cfg.HubPhysicalPort = hubPhysical
	}
	if strings.TrimSpace(hubSlave) != "" {
		cfg.HubSlavePort = hubSlave
	}
	if strings.TrimSpace(hubSlaveExtra) != "" {
		var extras []string
		for _, s := range strings.Split(hubSlaveExtra, ",") {
			if s = strings.TrimSpace(s); s != "" {
				extras = append(extras, s)
			}
		}
		cfg.HubExtraSlaves = extras
	}

	return cfg
}

func loadConfig(path string) (bridgeConfig, error) {
	var cfg bridgeConfig

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}

	if len(strings.TrimSpace(string(b))) == 0 {
		return cfg, nil
	}

	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func saveConfig(path string, cfg bridgeConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	return os.WriteFile(path, b, 0o644)
}

func defaultConfigPath() string {
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		return filepath.Join(local, defaultConfig)
	}

	home, err := os.UserHomeDir()
	if err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "AppData", "Local", defaultConfig)
	}

	return defaultConfig
}

func scanAndSelectCOMPort() (string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return "", err
	}

	if len(ports) == 0 {
		return "", errors.New("no serial ports found")
	}

	sort.Strings(ports)
	friendlyNames := getCOMFriendlyNames(ports)
	fmt.Println("Detected COM ports (including USB virtual COM ports):")
	for i, p := range ports {
		display := p
		if friendly, ok := friendlyNames[strings.ToUpper(p)]; ok && strings.TrimSpace(friendly) != "" {
			display = fmt.Sprintf("%s - %s", p, friendly)
		}
		fmt.Printf("  %d) %s\n", i+1, display)
	}

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Select COM port [1-%d]: ", len(ports))
		line, readErr := readLine(reader)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}

		idx, convErr := strconv.Atoi(strings.TrimSpace(line))
		if convErr != nil || idx < 1 || idx > len(ports) {
			fmt.Println("Invalid choice, try again.")
			if errors.Is(readErr, io.EOF) {
				return "", errors.New("input closed before valid selection")
			}
			continue
		}

		return normalizePortName(ports[idx-1]), nil
	}
}

func printCOMPortList() error {
	ports, err := serial.GetPortsList()
	if err != nil {
		return err
	}

	if len(ports) == 0 {
		fmt.Println("No COM ports found.")
		return nil
	}

	sort.Strings(ports)
	friendlyNames := getCOMFriendlyNames(ports)
	for i, p := range ports {
		display := p
		if friendly, ok := friendlyNames[strings.ToUpper(p)]; ok && strings.TrimSpace(friendly) != "" {
			display = fmt.Sprintf("%s - %s", p, friendly)
		}
		fmt.Printf("%d) %s\n", i+1, display)
	}

	return nil
}

func notifyClientConnected(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func flagProvided(name string) bool {
	prefix := "-" + name
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}

type clientHub struct {
	mu      sync.RWMutex
	clients map[net.Conn]struct{}
}

func newClientHub() *clientHub {
	return &clientHub{clients: make(map[net.Conn]struct{})}
}

func (h *clientHub) Add(conn net.Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
}

func (h *clientHub) Remove(conn net.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

func (h *clientHub) Count() int {
	h.mu.RLock()
	count := len(h.clients)
	h.mu.RUnlock()
	return count
}

func (h *clientHub) Broadcast(data []byte, logger *log.Logger) {
	conns := h.snapshot()
	for _, conn := range conns {
		_ = conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
		if err := writeAll(conn, data); err != nil {
			logger.Printf("client write failed: %s: %v", conn.RemoteAddr(), err)
			h.Remove(conn)
			_ = conn.Close()
		}
	}
}

func (h *clientHub) CloseAll() {
	conns := h.snapshot()
	for _, conn := range conns {
		_ = conn.Close()
		h.Remove(conn)
	}
}

func (h *clientHub) snapshot() []net.Conn {
	h.mu.RLock()
	conns := make([]net.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	return conns
}

type fifoBuffer struct {
	data  []byte
	limit int
}

func newFIFOBuffer(limit int) *fifoBuffer {
	return &fifoBuffer{limit: limit}
}

func (b *fifoBuffer) Append(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	if len(chunk) >= b.limit {
		b.data = append(b.data[:0], chunk[len(chunk)-b.limit:]...)
		return
	}

	overflow := len(b.data) + len(chunk) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}

	b.data = append(b.data, chunk...)
}

func (b *fifoBuffer) Drain() []byte {
	if len(b.data) == 0 {
		return nil
	}
	out := append([]byte(nil), b.data...)
	b.data = b.data[:0]
	return out
}

func (b *fifoBuffer) Len() int {
	return len(b.data)
}

func writeAll(w io.Writer, data []byte) error {
	written := 0
	for written < len(data) {
		n, err := w.Write(data[written:])
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		written += n
	}
	return nil
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func normalizePortName(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return trimmed
	}

	if runtime.GOOS == "windows" {
		upper := strings.ToUpper(trimmed)
		if strings.HasPrefix(upper, "COM") {
			return upper
		}
		if _, err := strconv.Atoi(trimmed); err == nil {
			return "COM" + trimmed
		}
	}

	return trimmed
}

func newLogger(path string) (*log.Logger, func(), error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stdout, "[bridge] ", log.LstdFlags), func() {}, nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}

	mw := io.MultiWriter(ignoreErrWriter{os.Stdout}, f)
	logger := log.New(mw, "[bridge] ", log.LstdFlags)
	return logger, func() { _ = f.Close() }, nil
}

// ignoreErrWriter adapts a writer so that write errors (e.g. os.Stdout on a
// -H windowsgui build with no console) never abort an io.MultiWriter, which
// would otherwise skip the remaining writers (the log file).
type ignoreErrWriter struct{ w io.Writer }

func (e ignoreErrWriter) Write(p []byte) (int, error) {
	_, _ = e.w.Write(p)
	return len(p), nil
}

func getCOMFriendlyNames(ports []string) map[string]string {
	result := make(map[string]string)
	if runtime.GOOS != "windows" || len(ports) == 0 {
		return result
	}

	known := make(map[string]struct{}, len(ports))
	for _, p := range ports {
		known[strings.ToUpper(strings.TrimSpace(p))] = struct{}{}
	}

	// First try native metadata from the serial enumerator (fast, no PowerShell startup).
	details, err := enumerator.GetDetailedPortsList()
	if err == nil {
		for _, d := range details {
			if d == nil {
				continue
			}
			name := strings.ToUpper(strings.TrimSpace(d.Name))
			if _, ok := known[name]; !ok {
				continue
			}
			if strings.TrimSpace(d.Product) == "" {
				continue
			}
			result[name] = strings.TrimSpace(d.Product)
		}
	}

	if len(result) == len(known) {
		return result
	}

	psScript := `$OutputEncoding = [System.Text.UTF8Encoding]::new();
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new();
$map = @{};
Get-CimInstance Win32_SerialPort -ErrorAction SilentlyContinue | ForEach-Object {
	if ($_.DeviceID) {
		$map[$_.DeviceID.ToUpper()] = $_.Name;
	}
};
try {
	Get-PnpDevice -Class Ports -PresentOnly -ErrorAction SilentlyContinue | ForEach-Object {
		$friendly = $_.FriendlyName;
		if ($friendly -match '\((COM\d+)\)') {
			$id = $matches[1].ToUpper();
			if (-not $map.ContainsKey($id) -or [string]::IsNullOrWhiteSpace($map[$id])) {
				$map[$id] = $friendly;
			}
		}
	};
} catch {
}
$map.GetEnumerator() | ForEach-Object { "$($_.Key)|$($_.Value)" }`
	ctx, cancel := context.WithTimeout(context.Background(), nameQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", psScript)
	out, err := cmd.Output()
	if err != nil {
		return result
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "\ufeff")
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}

		deviceID := strings.ToUpper(strings.TrimSpace(parts[0]))
		name := strings.TrimSpace(parts[1])
		if _, ok := known[deviceID]; !ok {
			continue
		}
		if _, has := result[deviceID]; has {
			continue
		}
		if name == "" {
			continue
		}

		result[deviceID] = name
	}

	return result
}
