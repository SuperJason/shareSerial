package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the persisted bridge configuration. Every field can also be
// overridden on the command line; command-line values win and are written back
// to the config file after resolution (see mergeConfig).
//
// Unknown keys from older config files (hub4com_*, ftdi_*) are ignored by
// encoding/json, so an existing bridge_config.json keeps working.
type Config struct {
	ComPort  string `json:"com_port"`
	BaudRate int    `json:"baud_rate"`
	DataBits int    `json:"data_bits"`
	StopBits int    `json:"stop_bits"` // 1 or 2; 0 means 1
	Parity   string `json:"parity"`    // none|odd|even|mark|space
	Encoding string `json:"encoding"`  // utf-8|gbk|gb18030|big5|shift-jis|latin1

	// LogFile is the bridge's own runtime log (open/close/errors).
	LogFile string `json:"log_file"`
	// CaptureEnabled starts the text log at startup. Off by default; the
	// window's context menu toggles it at runtime.
	CaptureEnabled bool `json:"capture_enabled"`
	// CaptureDir is where capture files go. Each session gets
	// serial-YYYYmmdd-HHMMSS.log.
	CaptureDir string `json:"capture_dir"`
	// DataLogFile is only accepted for backward compatibility with configs
	// written before the capture menu existed; it seeds CaptureDir.
	DataLogFile string `json:"data_log_file,omitempty"`

	// TargetIP/TargetPort configure the primary byte transport. Empty TargetIP
	// means "listen on all interfaces"; a non-empty one makes the bridge a TCP
	// client that dials out (compatibility with the original bridge).
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`

	// TapPort mirrors the raw byte stream (RX and TX) read-only. 0 disables.
	TapPort int `json:"tap_port"`
	// FramePort serves the timestamped framed protocol in both directions.
	// 0 disables.
	FramePort int `json:"frame_port"`
	// WritePolicy decides who may write to the serial port from TCP:
	// "all" (any client), "first" (first connected client owns the write
	// side until it disconnects), "readonly" (TCP clients cannot write).
	WritePolicy string `json:"write_policy"`

	// WebPort serves the log window UI and its HTTP API on loopback.
	WebPort int `json:"web_port"`
	// Window opens the native WebView2 window at startup. false runs headless
	// and only prints the UI URL.
	Window bool `json:"window"`
	// AutoReopen reopens the serial port after a device-side error
	// (unplug/replug) with backoff.
	AutoReopen bool `json:"auto_reopen"`

	// LineIdleMs flushes a partial line after this much silence, so devices
	// that never send a terminator still produce visible rows.
	LineIdleMs int `json:"line_idle_ms"`
	// MaxLineBytes bounds one record. Longer bursts are split into chunks.
	MaxLineBytes int `json:"max_line_bytes"`
	// MaxRecords/MaxBytes bound the in-memory log ring.
	MaxRecords int   `json:"max_records"`
	MaxBytes   int64 `json:"max_bytes"`
}

func defaultConfig() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, "Documents", "serial-log")
	return Config{
		BaudRate:     115200,
		DataBits:     8,
		StopBits:     1,
		Parity:       "none",
		Encoding:     "utf-8",
		LogFile:      filepath.Join(base, "bridge.log"),
		CaptureDir:   base,
		TargetPort:   2601,
		TapPort:      2602,
		FramePort:    2603,
		WritePolicy:  "first",
		WebPort:      18080,
		Window:       true,
		AutoReopen:   true,
		LineIdleMs:   100,
		MaxLineBytes: 8192,
		MaxRecords:   200000,
		MaxBytes:     32 << 20,
	}
}

// Validate fills in defaults and rejects impossible values.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.ComPort) == "" {
		return fmt.Errorf("com_port is required")
	}
	if c.BaudRate <= 0 {
		return fmt.Errorf("baud_rate must be > 0")
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return fmt.Errorf("data_bits must be 5-8")
	}
	if c.StopBits != 1 && c.StopBits != 2 {
		return fmt.Errorf("stop_bits must be 1 or 2")
	}
	switch strings.ToLower(c.Parity) {
	case "none", "odd", "even", "mark", "space":
		c.Parity = strings.ToLower(c.Parity)
	default:
		return fmt.Errorf("parity must be none|odd|even|mark|space")
	}
	switch strings.ToLower(c.Encoding) {
	case "utf-8", "utf8", "gbk", "gb2312", "gb18030", "big5", "shift-jis", "sjis", "euc-jp", "euc-kr", "latin1", "raw":
	default:
		return fmt.Errorf("unsupported encoding %q", c.Encoding)
	}
	for name, p := range map[string]*int{
		"target_port": &c.TargetPort, "tap_port": &c.TapPort,
		"frame_port": &c.FramePort, "web_port": &c.WebPort,
	} {
		if *p < 0 || *p > 65535 {
			return fmt.Errorf("%s must be between 0 and 65535", name)
		}
		if *p == 0 && name != "target_port" && name != "tap_port" && name != "frame_port" {
			return fmt.Errorf("%s is required", name)
		}
	}
	switch strings.ToLower(c.WritePolicy) {
	case "all", "first", "readonly":
		c.WritePolicy = strings.ToLower(c.WritePolicy)
	default:
		return fmt.Errorf("write_policy must be all|first|readonly")
	}
	if c.LineIdleMs <= 0 {
		c.LineIdleMs = 100
	}
	if c.MaxLineBytes <= 0 {
		c.MaxLineBytes = 8192
	}
	if c.MaxRecords <= 0 {
		c.MaxRecords = 200000
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 32 << 20
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	// Configs written before the capture menu may still name a log file.
	if cfg.CaptureDir == "" {
		if cfg.DataLogFile != "" {
			cfg.CaptureDir = filepath.Dir(cfg.DataLogFile)
		} else {
			cfg.CaptureDir = defaultCaptureDir()
		}
	}
	return cfg, nil
}

func saveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// defaultConfigPath keeps the config and the colour schemes next to the
// executable, so the tool stays a portable folder: copy the folder and the
// settings travel with it. Falls back to the profile only when that folder is
// not writable (e.g. installed under Program Files).
func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		probe := filepath.Join(dir, ".write-test")
		if f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			f.Close()
			os.Remove(probe)
			return filepath.Join(dir, "serial-log.json")
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "serial-log", "serial-log.json")
	}
	return "serial-log.json"
}

// parityLetter renders "none"/"odd"/... as the N/O/E/M/S letter used by the UI.
func parityLetter(name string) string {
	switch strings.ToLower(name) {
	case "odd":
		return "O"
	case "even":
		return "E"
	case "mark":
		return "M"
	case "space":
		return "S"
	default:
		return "N"
	}
}

// normalizePortName accepts "3", "com3" or "COM3" and returns "COM3".
func normalizePortName(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return v
	}
	if n, err := strconv.Atoi(v); err == nil {
		return "COM" + strconv.Itoa(n)
	}
	if strings.HasPrefix(strings.ToUpper(v), "COM") {
		return "COM" + strings.ToUpper(v[3:])
	}
	return v
}
