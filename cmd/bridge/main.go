package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const version = "2.0.0"

func main() {
	var (
		configFile = flag.String("config", defaultConfigPath(), "config file path")
		comPort    = flag.String("p", "", "COM port number or name, e.g. 3 or COM3")
		baud       = flag.Int("b", 0, "baud rate")
		scan       = flag.Bool("scan", false, "list COM ports and pick one interactively")
		bindIP     = flag.String("ip", "", "listen address for the raw TCP port (default all interfaces)")
		targetPort = flag.Int("P", 0, "raw TCP port, 0 disables (default 2601)")
		tapPort    = flag.Int("tap", 0, "read-only mirror port, 0 disables (default 2602)")
		framePort  = flag.Int("frame", 0, "framed protocol port, 0 disables (default 2603)")
		wpolicy    = flag.String("wpolicy", "", "TCP write policy: all|first|readonly")
		webPort    = flag.Int("web", 0, "log window UI / HTTP API port on 127.0.0.1 (default 18080)")
		window     = flag.Bool("window", true, "open the native log window")
		noWindow   = flag.Bool("no-window", false, "headless: print the UI URL instead of opening a window")
		logFile    = flag.String("log", "", "runtime log file")
		captureOn  = flag.Bool("capture", false, "start the text capture immediately")
		captureDir = flag.String("capturedir", "", "folder for capture files (default Documents/serial-log)")
		encoding   = flag.String("enc", "", "device charset: utf-8|gbk|gb18030|big5|shift-jis|euc-kr|latin1")
		demo       = flag.Bool("demo", false, "run against a synthetic device instead of a COM port")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Printf("serial-log %s\n", version)
		return
	}

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
	}

	// Colour schemes live beside the config as plain files, so what the menu
	// offers is exactly what is in that folder. The built-in ones are written
	// out on first run; existing files are never touched.
	schemesDir := ""
	if dir := filepath.Dir(*configFile); dir != "" {
		schemesDir = filepath.Join(dir, "schemes")
		if err := seedSchemes(schemesDir); err != nil {
			fmt.Fprintf(os.Stderr, "schemes: %v\n", err)
		}
	}

	// Command-line values win over the config file (only flags the caller
	// actually set, so defaults never clobber a stored config).
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["p"] {
		cfg.ComPort = normalizePortName(*comPort)
	}
	if set["b"] {
		cfg.BaudRate = *baud
	}
	if set["P"] {
		cfg.TargetPort = *targetPort
	}
	if set["tap"] {
		cfg.TapPort = *tapPort
	}
	if set["frame"] {
		cfg.FramePort = *framePort
	}
	if set["wpolicy"] {
		cfg.WritePolicy = *wpolicy
	}
	if set["web"] {
		cfg.WebPort = *webPort
	}
	if set["ip"] {
		cfg.TargetIP = *bindIP
	}
	if set["log"] {
		cfg.LogFile = *logFile
	}
	if *captureOn {
		cfg.CaptureEnabled = true
	}
	if set["capturedir"] {
		cfg.CaptureDir = *captureDir
	}
	if set["enc"] {
		cfg.Encoding = *encoding
	}
	if set["window"] {
		cfg.Window = *window
	}
	if *noWindow {
		cfg.Window = false
	}
	if *scan {
		if picked, err := scanAndSelect(); err == nil && picked != "" {
			cfg.ComPort = picked
		}
	}
	cfg.ComPort = normalizePortName(cfg.ComPort)
	if *demo {
		cfg.ComPort = ""
	}
	if err := cfg.Validate(); err != nil && !*demo {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		fmt.Fprintf(os.Stderr, "pass -p <port> -b <baud> (or -scan) to get started\n")
		os.Exit(2)
	}

	rtlog, closeLog, err := newLogger(cfg.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "log file: %v\n", err)
		closeLog, rtlog = func() {}, log.New(os.Stderr, "", log.LstdFlags)
	}
	defer closeLog()

	app, err := newApp(cfg, rtlog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "startup failed: %v\n", err)
		os.Exit(1)
	}
	app.SetConfigPath(*configFile)
	rtlog.Printf("serial-log %s starting: port=%q baud=%d enc=%s policy=%s",
		version, cfg.ComPort, cfg.BaudRate, cfg.Encoding, cfg.WritePolicy)

	tr := newTransports(app, rtlog)
	if err := tr.start(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "transport: %v\n", err)
		os.Exit(1)
	}

	webSrv, webLn, err := startWeb(app, cfg.WebPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "web UI: %v\n", err)
		os.Exit(1)
	}
	uiURL := fmt.Sprintf("http://127.0.0.1:%d/", cfg.WebPort)
	rtlog.Printf("log window UI: %s", uiURL)

	if !*demo && cfg.ComPort != "" {
		if err := app.Open(cfg.portParams()); err != nil {
			rtlog.Printf("serial open failed: %v", err)
			fmt.Fprintf(os.Stderr, "serial open failed: %v\n", err)
		}
	}
	if *demo {
		app.StartDemo()
	}

	// Persist the resolved settings, mirroring the original bridge behaviour.
	if cfg.ComPort != "" {
		if err := saveConfig(*configFile, cfg); err != nil {
			rtlog.Printf("save config failed: %v", err)
		}
	}

	fmt.Println("----------------------------------------------")
	fmt.Printf(" serial-log %s\n", version)
	fmt.Printf(" port    : %s @ %d %d%s%d\n", orDash(cfg.ComPort), cfg.BaudRate, cfg.DataBits, parityLetter(cfg.Parity), cfg.StopBits)
	if cfg.CaptureEnabled {
		fmt.Printf(" capture : %s\n", cfg.CaptureDir)
	} else {
		fmt.Printf(" capture : off (right-click the window to start)\n")
	}
	fmt.Printf(" raw TCP : %d   tap: %d   framed: %d   policy: %s\n", cfg.TargetPort, cfg.TapPort, cfg.FramePort, cfg.WritePolicy)
	fmt.Printf(" config  : %s\n", *configFile)
	fmt.Printf(" schemes : %s\n", orDash(schemesDir))
	fmt.Printf(" window  : %s\n", uiURL)
	fmt.Println("----------------------------------------------")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		<-sigCh
		close(done)
	}()

	if cfg.Window {
		// Blocks until the window closes; Run() owns the message loop, so the
		// shutdown path below runs on the same goroutine afterwards.
		if !openWindow(uiURL) {
			fmt.Fprintln(os.Stderr, "WebView2 runtime unavailable; open the URL above in a browser")
			<-done
		}
	} else {
		<-done
	}

	tr.stop()
	app.StopDemo()
	app.Close("shutting down")
	app.cap.Stop()
	webSrv.Close()
	_ = webLn
	rtlog.Printf("shutdown complete")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// newLogger opens the runtime log, falling back to stderr when the path is
// unusable (a broken log path must not stop the bridge).
func newLogger(path string) (*log.Logger, func(), error) {
	if strings.TrimSpace(path) == "" {
		return log.New(os.Stderr, "", log.LstdFlags), func() {}, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, func() {}, err
	}
	return log.New(f, "", log.LstdFlags), func() { f.Close() }, nil
}

// scanAndSelect lists the COM ports and asks for a choice on stdin.
func scanAndSelect() (string, error) {
	ports := ListPorts()
	if len(ports) == 0 {
		return "", fmt.Errorf("no serial ports found")
	}
	fmt.Println("Available serial ports:")
	for i, p := range ports {
		fmt.Printf("  %d) %s\n", i+1, p)
	}
	fmt.Print("Select port number: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(strings.TrimSuffix(line, "."))
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(ports) {
		return "", fmt.Errorf("invalid selection %q", line)
	}
	return ports[n-1], nil
}
