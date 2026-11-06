package main

// Self-description for agents.
//
// An agent that has just found this process (fixed port, or the endpoint file
// written next to the config) should be able to answer three questions with one
// request: what is this, what is it doing right now, and where do I read the
// data. /api/hello is that request in JSON; /llms.txt is the same information as
// prose for clients that would rather read text.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const helloSummary = "Serial terminal window plus TCP bridge. One physical serial port is owned " +
	"by this process: its bytes are rendered in a terminal window, mirrored to three " +
	"TCP endpoints, optionally written to a log file, and served over HTTP for scripts " +
	"and agents."

const helloHow = "A read loop takes bytes from the port, splits them into lines, decodes the " +
	"charset and appends records to a bounded ring buffer. The window, the TCP endpoints and " +
	"the HTTP API all read from that same ring, each with its own queue, so a slow consumer " +
	"never stalls the serial port. Records carry a monotonically increasing sequence number: " +
	"read the newest N with ?tail=N, or everything after a sequence with ?after=SEQ."

// helloDoc is the machine-readable shape. Keep it stable: agents cache it.
type helloDoc struct {
	Schema       int               `json:"schema"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Summary      string            `json:"summary"`
	HowItWorks   string            `json:"how_it_works"`
	StartedAt    string            `json:"started_at"`
	UptimeS      int64             `json:"uptime_s"`
	PID          int               `json:"pid"`
	OS           string            `json:"os"`
	Paths        helloPaths        `json:"paths"`
	State        map[string]any    `json:"state"`
	Buffer       helloBuffer       `json:"buffer"`
	Transports   []helloTransport  `json:"transports"`
	Capabilities []helloCapability `json:"capabilities"`
	Docs         map[string]string `json:"docs"`
	Notes        []string          `json:"notes"`
}

type helloPaths struct {
	Exe        string `json:"exe"`
	Config     string `json:"config"`
	SchemesDir string `json:"schemes_dir"`
	CaptureDir string `json:"capture_dir"`
	Endpoint   string `json:"endpoint_file"`
}

type helloBuffer struct {
	SeqLast     uint64 `json:"seq_last"`
	SeqOldest   uint64 `json:"seq_oldest"`
	Records     int    `json:"records"`
	Bytes       int64  `json:"bytes"`
	CapacityRec int    `json:"capacity_records"`
	CapacityB   int64  `json:"capacity_bytes"`
}

type helloTransport struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Format      string `json:"format"`
	Duplex      bool   `json:"duplex"`
	Recommended string `json:"recommended_for,omitempty"`
}

type helloCapability struct {
	ID      string         `json:"id"`
	Purpose string         `json:"purpose"`
	Call    string         `json:"call"`
	Body    map[string]any `json:"body,omitempty"`
	Note    string         `json:"note,omitempty"`
}

// SetRuntimePaths records where the files live, for the self-description.
func (a *App) SetRuntimePaths(exe, schemesDir string) {
	a.mu.Lock()
	a.exePath = exe
	a.schemesDir = schemesDir
	a.mu.Unlock()
}

func (a *App) runtimePaths() (exe, cfgPath, schemes string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exePath, a.cfgPath, a.schemesDir
}

// Hello builds the self-description. Everything volatile is read live, so an
// agent that caches only the URLs still gets truthful state on every call.
func (a *App) Hello(webPort int) helloDoc {
	exe, cfgPath, schemes := a.runtimePaths()

	st := a.State()
	capture, _ := st["capture"].(map[string]any)
	capDir, _ := capture["dir"].(string)

	host := fmt.Sprintf("http://127.0.0.1:%d", webPort)

	a.mu.Lock()
	cfg := a.cfg
	started := a.startedAt
	a.mu.Unlock()

	count, bytes, last, _ := a.ring.Stats()

	// Writes go to the device; say what the current policy actually allows.
	writeOK := cfg.WritePolicy != "readonly"

	return helloDoc{
		Schema:     1,
		Name:       "serial-log",
		Version:    version,
		Summary:    helloSummary,
		HowItWorks: helloHow,
		StartedAt:  started.Format(time.RFC3339),
		UptimeS:    int64(time.Since(started).Seconds()),
		PID:        os.Getpid(),
		OS:         runtime.GOOS + "/" + runtime.GOARCH,
		Paths: helloPaths{
			Exe:        exe,
			Config:     cfgPath,
			SchemesDir: schemes,
			CaptureDir: capDir,
			Endpoint:   endpointPath(cfgPath),
		},
		State: st,
		Buffer: helloBuffer{
			SeqLast:     last,
			SeqOldest:   a.ring.Oldest(),
			Records:     count,
			Bytes:       bytes,
			CapacityRec: cfg.MaxRecords,
			CapacityB:   cfg.MaxBytes,
		},
		Transports: []helloTransport{
			{
				Name:        "http-text",
				URL:         host + "/api/text",
				Format:      "text/plain, one decoded line per record",
				Duplex:      false,
				Recommended: "agents: history plus cursor-based incremental reads",
			},
			{
				Name:        "http-json",
				URL:         host + "/api/records",
				Format:      "JSON: seq, ts (unix ms), dir (rx/tx/sys), src, text or hex",
				Duplex:      false,
				Recommended: "when timestamps or directions are needed",
			},
			{
				Name:   "raw-tcp",
				URL:    fmt.Sprintf("tcp://127.0.0.1:%d", cfg.TargetPort),
				Format: "unframed bytes, both directions",
				Duplex: true,
			},
			{
				Name:   "tap-tcp",
				URL:    fmt.Sprintf("tcp://127.0.0.1:%d", cfg.TapPort),
				Format: "unframed bytes, read-only mirror of RX and TX",
				Duplex: false,
			},
			{
				Name:   "framed-tcp",
				URL:    fmt.Sprintf("tcp://127.0.0.1:%d", cfg.FramePort),
				Format: "frames: [u32 len][u8 dir 1=RX 2=TX 3=SYS][u64 ts_ms][payload]",
				Duplex: true,
			},
		},
		Capabilities: []helloCapability{
			{
				ID:      "read_history",
				Purpose: "the newest lines, without keeping a file",
				Call:    "GET " + host + "/api/text?tail=200",
				Note:    "tail defaults to 100, capped at 5000; add ts=1 or dir=1 for prefixes",
			},
			{
				ID:      "read_incremental",
				Purpose: "only what arrived since you last looked",
				Call:    "GET " + host + "/api/text?after=<seq>&limit=500",
				Note: "take the next cursor from the X-Seq-Next response header; " +
					"if your cursor is below X-Seq-Oldest you missed records, fall back to tail=",
			},
			{
				ID:      "send",
				Purpose: "type into the device console",
				Call:    "POST " + host + "/api/send",
				Body:    map[string]any{"text": "ls -l\n"},
				Note:    writeNote(writeOK, cfg.WritePolicy),
			},
			{
				ID:      "send_hex",
				Purpose: "send raw bytes, e.g. a control character",
				Call:    "POST " + host + "/api/send",
				Body:    map[string]any{"hex": "03"},
			},
			{
				ID:      "open",
				Purpose: "open or reopen the port with other settings",
				Call:    "POST " + host + "/api/open",
				Body:    map[string]any{"com_port": cfg.ComPort, "baud_rate": cfg.BaudRate},
			},
			{
				ID:      "close",
				Purpose: "release the port",
				Call:    "POST " + host + "/api/close",
			},
			{
				ID:      "capture",
				Purpose: "write incoming text to a timestamped log file",
				Call:    "POST " + host + "/api/capture",
				Body:    map[string]any{"on": true, "dir": capDir},
				Note:    "off by default; the current file path appears in state.capture.path",
			},
			{
				ID:      "list_ports",
				Purpose: "enumerate serial ports on this machine",
				Call:    "GET " + host + "/api/ports",
			},
			{
				ID:      "clear",
				Purpose: "drop the in-memory buffer",
				Call:    "POST " + host + "/api/clear",
				Note:    "does not affect a capture file that is already open",
			},
		},
		Docs: map[string]string{
			"self":   host + "/api/hello",
			"prose":  host + "/llms.txt",
			"ui":     host + "/",
			"readme": "README.md next to the executable",
		},
		Notes: []string{
			"The HTTP API listens on loopback only; there is no authentication.",
			"Colour highlighting is a display-only feature of the window: the bytes in the " +
				"buffer, the capture file and the TCP endpoints are unmodified.",
			"Records older than the ring capacity are evicted; compare your cursor with " +
				"buffer.seq_oldest to detect a gap.",
			"When the port is closed the endpoints stay up and simply produce nothing; " +
				"state.status and state.last_error say why.",
			"Writing while the target spams its console can interleave bytes on the device side; " +
				"the tool sends exactly what it reports.",
		},
	}
}

// writeNote states plainly whether the current policy lets a script write.
func writeNote(ok bool, policy string) string {
	if ok {
		return "write policy is \"" + policy + "\"; bytes are sent verbatim, include your own newline"
	}
	return "write policy is \"readonly\"; a send is refused"
}

// endpointPath is the small file written at startup so a consumer that does not
// know the HTTP port can still find this instance.
func endpointPath(cfgPath string) string {
	if cfgPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfgPath), "serial-log.endpoint.json")
}

// writeEndpointFile drops a small pointer next to the config, so a consumer that
// does not know which port this instance picked can still find it.
func writeEndpointFile(cfgPath string, port int) error {
	p := endpointPath(cfgPath)
	if p == "" {
		return nil
	}
	doc := map[string]any{
		"schema":     1,
		"service":    "serial-log",
		"version":    version,
		"web_port":   port,
		"url":        fmt.Sprintf("http://127.0.0.1:%d/", port),
		"hello":      fmt.Sprintf("http://127.0.0.1:%d/api/hello", port),
		"pid":        os.Getpid(),
		"started_at": time.Now().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, "\n"...), 0o644)
}

// LLMSText renders the same information as prose, for clients that read text
// rather than JSON. Values are interpolated live so the file never goes stale.
func (a *App) LLMSText(webPort int) string {
	h := a.Hello(webPort)
	host := fmt.Sprintf("http://127.0.0.1:%d", webPort)

	state := "closed (nothing is being read)"
	if open, _ := h.State["open"].(bool); open {
		state = "open"
	}
	lastErr, _ := h.State["last_error"].(string)
	if lastErr != "" {
		state += ", last error: " + lastErr
	}
	captureOn, _ := h.State["capture"].(map[string]any)
	capState := "off"
	if on, _ := captureOn["enabled"].(bool); on {
		path, _ := captureOn["path"].(string)
		capState = "on, writing " + path
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# serial-log %s\n\n", h.Version)
	fmt.Fprintf(&b, "%s\n\n", h.Summary)
	fmt.Fprintf(&b, "%s\n\n", h.HowItWorks)

	fmt.Fprintf(&b, "## Right now\n\n")
	fmt.Fprintf(&b, "- port: %s @ %d %d%s%d, %s\n", orDash(h.Port()), h.Baud(), h.State["data_bits"],
		parityLetter(fmt.Sprint(h.State["parity"])), h.State["stop_bits"], state)
	fmt.Fprintf(&b, "- capture: %s\n", capState)
	fmt.Fprintf(&b, "- buffer: %d records (%d bytes), sequence %d..%d (capacity %d)\n",
		h.Buffer.Records, h.Buffer.Bytes, h.Buffer.SeqOldest, h.Buffer.SeqLast, h.Buffer.CapacityRec)
	fmt.Fprintf(&b, "- running since %s (pid %d)\n\n", h.StartedAt, h.PID)

	fmt.Fprintf(&b, "## Read the output\n\n")
	fmt.Fprintf(&b, "The window, the TCP endpoints and the HTTP API all read the same buffer.\n\n")
	fmt.Fprintf(&b, "Newest lines, as plain text:\n\n")
	fmt.Fprintf(&b, "```\ncurl -s \"%s/api/text?tail=200\"\n```\n\n", host)
	fmt.Fprintf(&b, "Only what is new since your last read (no temporary file needed):\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "NEXT=$(curl -s -D - -o /dev/null \"%s/api/text?tail=1\" | grep -i '^x-seq-next' | tr -d '\\r' | cut -d' ' -f2)\n", host)
	fmt.Fprintf(&b, "curl -s \"%s/api/text?after=$NEXT&limit=500\"\n", host)
	fmt.Fprintf(&b, "```\n\n")
	fmt.Fprintf(&b, "`X-Seq-Next` is the cursor for the next call, `X-Seq-Last` the newest sequence, ")
	fmt.Fprintf(&b, "`X-Seq-Oldest` the oldest still retained. If your cursor is below `X-Seq-Oldest`, ")
	fmt.Fprintf(&b, "records were evicted and you should re-read with `tail=`. Add `ts=1` for ")
	fmt.Fprintf(&b, "`HH:MM:SS.mmm` prefixes and `dir=1` for `RX`/`TX`/`SYS`.\n\n")
	fmt.Fprintf(&b, "With timestamps and directions as JSON: `GET %s/api/records?tail=50`.\n\n", host)

	fmt.Fprintf(&b, "## Send to the device\n\n")
	fmt.Fprintf(&b, "```\ncurl -s -X POST %s/api/send -d '{\"text\":\"ls -l\\n\"}'\n", host)
	fmt.Fprintf(&b, "curl -s -X POST %s/api/send -d '{\"hex\":\"03\"}'\n```\n\n", host)
	fmt.Fprintf(&b, "The device echoes what it receives, so what you send appears in the buffer as RX. ")
	fmt.Fprintf(&b, "Write policy is `%s`.\n\n", h.WritePolicy())

	fmt.Fprintf(&b, "## Endpoints\n\n")
	for _, t := range h.Transports {
		fmt.Fprintf(&b, "- `%s` — %s", t.URL, t.Format)
		if t.Duplex {
			b.WriteString(" (duplex)")
		}
		if t.Recommended != "" {
			fmt.Fprintf(&b, "; %s", t.Recommended)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nFull state as JSON: `GET %s/api/state`. This description as JSON: `GET %s/api/hello`.\n\n", host, host)

	fmt.Fprintf(&b, "## Things to know\n\n")
	for _, n := range h.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	fmt.Fprintf(&b, "\nColour schemes live in `%s` as JSON files; they only affect the window.\n", h.Paths.SchemesDir)
	return b.String()
}

// small accessors keep the prose template readable
func (h helloDoc) Port() string   { s, _ := h.State["port"].(string); return s }
func (h helloDoc) Baud() int      { v, _ := h.State["baud"].(int); return v }
func (h helloDoc) DataBits() int  { v, _ := h.State["data_bits"].(int); return v }
func (h helloDoc) StopBits() int  { v, _ := h.State["stop_bits"].(int); return v }
func (h helloDoc) Parity() string { s, _ := h.State["parity"].(string); return s }
func (h helloDoc) WritePolicy() string {
	cfg, _ := h.State["config"].(map[string]any)
	s, _ := cfg["write_policy"].(string)
	return s
}
