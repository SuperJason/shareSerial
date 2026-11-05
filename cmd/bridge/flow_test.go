package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// ---------- fake device ----------

type fakePort struct {
	mu       sync.Mutex
	rx       chan []byte
	written  []byte
	closed   bool
	failRead bool
}

func newFakePort() *fakePort { return &fakePort{rx: make(chan []byte, 256)} }

func (f *fakePort) Read(p []byte) (int, error) {
	f.mu.Lock()
	fail := f.failRead
	f.mu.Unlock()
	if fail {
		return 0, fmt.Errorf("device unplugged")
	}
	select {
	case b := <-f.rx:
		n := copy(p, b)
		if n < len(b) {
			f.rx <- b[n:]
		}
		return n, nil
	case <-time.After(2 * time.Millisecond):
	}
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return 0, io.EOF
	}
	return 0, nil
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fmt.Errorf("port closed")
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakePort) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakePort) SetMode(*serial.Mode) error        { return nil }
func (f *fakePort) SetReadTimeout(time.Duration) error { return nil }

func (f *fakePort) writtenString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.written)
}

func (f *fakePort) setFail(v bool) {
	f.mu.Lock()
	f.failRead = v
	f.mu.Unlock()
}

// ---------- helpers ----------

func installFake(t *testing.T, ports ...*fakePort) {
	t.Helper()
	old := openSerialPort
	i := 0
	openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
		if i >= len(ports) {
			return ports[len(ports)-1], nil
		}
		p := ports[i]
		i++
		return p, nil
	}
	t.Cleanup(func() { openSerialPort = old })
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.ComPort = "COM_TEST"
	cfg.CaptureDir = dir
	cfg.LogFile = ""
	cfg.TargetPort = freePort(t)
	cfg.TapPort = freePort(t)
	cfg.FramePort = freePort(t)
	cfg.WebPort = freePort(t)
	cfg.Window = false
	return cfg
}

func newTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	a, err := newApp(cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(func() {
		a.StopDemo()
		a.Close("test done")
		a.cap.Stop()
	})
	return a
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func records(a *App) []Rec {
	recs, _ := a.Ring().Since(0, 500)
	return recs
}

func findRec(recs []Rec, dir, text string) *Rec {
	for i := range recs {
		if recs[i].Dir == dir && strings.Contains(recs[i].Text, text) {
			return &recs[i]
		}
	}
	return nil
}

// ---------- app pipeline ----------

func TestAppReceivesAndFramesSerialData(t *testing.T) {
	cfg := testConfig(t)
	cfg.CaptureEnabled = true
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)

	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatalf("open: %v", err)
	}
	fp.rx <- []byte("hello\r\npartial")
	waitFor(t, "two rx records", func() bool {
		recs := records(a)
		return findRec(recs, "rx", "hello") != nil && findRec(recs, "rx", "partial") != nil
	})

	recs := records(a)
	hello := findRec(recs, "rx", "hello")
	if hello.Text != "hello" || hello.Partial {
		t.Fatalf("unexpected line record: %+v", *hello)
	}
	partial := findRec(recs, "rx", "partial")
	if !partial.Partial {
		t.Fatalf("idle-flushed line should be marked partial: %+v", *partial)
	}
	if a.rxBytes.Load() < int64(len("hello\r\npartial")) {
		t.Fatalf("rx byte counter not updated: %d", a.rxBytes.Load())
	}

	// The text capture file must contain the same lines (the writer flushes on
	// its own timer, so poll instead of reading once).
	capInfo, _ := a.State()["capture"].(map[string]any)
	capPath, _ := capInfo["path"].(string)
	if capPath == "" {
		t.Fatal("capture path not reported in state")
	}
	var data []byte
	waitFor(t, "text capture flushed", func() bool {
		raw, err := os.ReadFile(capPath)
		if err != nil {
			return false
		}
		data = raw
		return strings.Contains(string(raw), "hello")
	})
	if !strings.Contains(string(data), "RX") {
		t.Fatalf("text capture missing direction tag:\n%s", data)
	}
}

func TestAppSendWritesAndRecordsTX(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}

	if err := a.Send([]byte("AT+VER\r\n"), "local", 0); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := fp.writtenString(); got != "AT+VER\r\n" {
		t.Fatalf("device received %q", got)
	}
	waitFor(t, "tx record", func() bool { return findRec(records(a), "tx", "AT+VER") != nil })
	if rec := findRec(records(a), "tx", "AT+VER"); rec.Src != "local" {
		t.Fatalf("tx source not recorded: %+v", *rec)
	}
	if a.txBytes.Load() != int64(len("AT+VER\r\n")) {
		t.Fatalf("tx counter = %d", a.txBytes.Load())
	}
}

func TestAppSendFailsWhenClosed(t *testing.T) {
	cfg := testConfig(t)
	installFake(t, newFakePort())
	a := newTestApp(t, cfg)
	if err := a.Send([]byte("x"), "local", 0); err == nil {
		t.Fatal("send on a closed port must fail")
	}
}

func TestAppGBKDecoding(t *testing.T) {
	cfg := testConfig(t)
	cfg.Encoding = "gbk"
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	payload := newCodec("gbk").Encode([]byte("电压正常\r\n"))
	// Split mid-character to prove the decoder buffers partial sequences.
	fp.rx <- payload[:1]
	fp.rx <- payload[1:]
	waitFor(t, "gbk line", func() bool { return findRec(records(a), "rx", "电压正常") != nil })
}

// ---------- transports ----------

func startTestTransports(t *testing.T, a *App, cfg Config) {
	t.Helper()
	tr := newTransports(a, a.Logger())
	if err := tr.start(cfg); err != nil {
		t.Fatalf("transports: %v", err)
	}
	t.Cleanup(tr.stop)
}

func dial(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("dial :%d: %v", port, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestRawClientReceivesAndSends(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)

	conn := dial(t, cfg.TargetPort)
	// Wait for the subscriber to be registered before feeding the device.
	waitFor(t, "raw client registered", func() bool { return a.Bus().count() == 1 })

	fp.rx <- []byte("from device\r\n")
	buf := make([]byte, 64)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read from raw client: %v", err)
	}
	if string(buf[:n]) != "from device\r\n" {
		t.Fatalf("client got %q", buf[:n])
	}

	if _, err := conn.Write([]byte("from client\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "device write", func() bool { return strings.Contains(fp.writtenString(), "from client") })
	waitFor(t, "tx record from tcp", func() bool { return findRec(records(a), "tx", "from client") != nil })
	if rec := findRec(records(a), "tx", "from client"); !strings.HasPrefix(rec.Src, "tcp#") {
		t.Fatalf("tx source should name the client: %+v", *rec)
	}
}

func TestRawClientDoesNotReceiveOwnEcho(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)
	conn := dial(t, cfg.TargetPort)
	waitFor(t, "client registered", func() bool { return a.Bus().count() == 1 })

	conn.Write([]byte("echo me"))
	waitFor(t, "device write", func() bool { return strings.Contains(fp.writtenString(), "echo me") })

	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err == nil {
		t.Fatalf("client must not receive its own bytes back, got %q", buf[:n])
	}
}

func TestTapPortIsReadOnlyMirror(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)

	tap := dial(t, cfg.TapPort)
	raw := dial(t, cfg.TargetPort)
	waitFor(t, "two subscribers", func() bool { return a.Bus().count() == 2 })

	fp.rx <- []byte("rx-bytes")
	tapBuf := make([]byte, 64)
	tap.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := tap.Read(tapBuf)
	if err != nil || string(tapBuf[:n]) != "rx-bytes" {
		t.Fatalf("tap got %q err=%v", tapBuf[:n], err)
	}

	// TX from the raw client must also show up on the tap.
	raw.Write([]byte("tx-bytes"))
	tap.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err = tap.Read(tapBuf)
	if err != nil || string(tapBuf[:n]) != "tx-bytes" {
		t.Fatalf("tap should mirror TX too, got %q err=%v", tapBuf[:n], err)
	}

	// Writing on the tap must never reach the device.
	tap.Write([]byte("should-not-pass"))
	time.Sleep(150 * time.Millisecond)
	if strings.Contains(fp.writtenString(), "should-not-pass") {
		t.Fatal("tap client managed to write to the device")
	}
}

func TestFramedPortRoundTrip(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)

	conn := dial(t, cfg.FramePort)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	greeting, err := readFrame(conn)
	if err != nil {
		t.Fatalf("greeting frame: %v", err)
	}
	if greeting.dir != dirSys || !strings.Contains(string(greeting.payload), "COM_TEST") {
		t.Fatalf("unexpected greeting: %+v", greeting)
	}

	fp.rx <- []byte("framed line\r\n")
	f, err := readFrame(conn)
	if err != nil {
		t.Fatalf("rx frame: %v", err)
	}
	if f.dir != dirRx || string(f.payload) != "framed line\r\n" {
		t.Fatalf("unexpected frame: dir=%d payload=%q", f.dir, f.payload)
	}
	if age := time.Since(time.UnixMilli(f.ts)); age < 0 || age > 5*time.Second {
		t.Fatalf("frame timestamp looks wrong: %s", age)
	}

	if _, err := conn.Write(appendFrame(nil, dirTx, time.Now().UnixMilli(), []byte("to-device"))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "framed write to device", func() bool { return strings.Contains(fp.writtenString(), "to-device") })
}

func TestWritePolicyFirst(t *testing.T) {
	cfg := testConfig(t)
	cfg.WritePolicy = "first"
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)

	first := dial(t, cfg.TargetPort)
	waitFor(t, "first client", func() bool { return a.Bus().count() == 1 })
	second := dial(t, cfg.TargetPort)
	waitFor(t, "second client", func() bool { return a.Bus().count() == 2 })

	first.Write([]byte("owner"))
	waitFor(t, "owner write", func() bool { return strings.Contains(fp.writtenString(), "owner") })
	second.Write([]byte("intruder"))
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(fp.writtenString(), "intruder") {
		t.Fatalf("second client must not write under policy=first, device saw %q", fp.writtenString())
	}
}

func TestWritePolicyReadonly(t *testing.T) {
	cfg := testConfig(t)
	cfg.WritePolicy = "readonly"
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	startTestTransports(t, a, cfg)
	conn := dial(t, cfg.TargetPort)
	waitFor(t, "client", func() bool { return a.Bus().count() == 1 })
	conn.Write([]byte("nope"))
	time.Sleep(150 * time.Millisecond)
	if fp.writtenString() != "" {
		t.Fatalf("readonly policy let a write through: %q", fp.writtenString())
	}
}

// ---------- auto reopen ----------

func TestAutoReopenAfterDeviceError(t *testing.T) {
	cfg := testConfig(t)
	cfg.AutoReopen = true
	first := newFakePort()
	second := newFakePort()
	installFake(t, first, second)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	first.setFail(true)
	waitFor(t, "reopen notice", func() bool {
		recs := records(a)
		return findRec(recs, "sys", "reopened") != nil
	})
	second.rx <- []byte("after reopen\r\n")
	waitFor(t, "data after reopen", func() bool { return findRec(records(a), "rx", "after reopen") != nil })
}

func TestOpenRetriesWhenPortBusy(t *testing.T) {
	cfg := testConfig(t)
	cfg.AutoReopen = true
	fp := newFakePort()
	old := openSerialPort
	tries := 0
	openSerialPort = func(name string, mode *serial.Mode) (serialPort, error) {
		tries++
		if tries == 1 {
			return nil, fmt.Errorf("Serial port busy")
		}
		return fp, nil
	}
	t.Cleanup(func() { openSerialPort = old })

	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err == nil {
		t.Fatal("a busy port must be reported to the caller")
	}
	waitFor(t, "background reopen", func() bool {
		open, _ := a.State()["open"].(bool)
		return open
	})
	if tries < 2 {
		t.Fatalf("expected a retry, open attempted %d time(s)", tries)
	}
	fp.rx <- []byte("after retry\r\n")
	waitFor(t, "data after retry", func() bool {
		return findRec(records(a), "rx", "after retry") != nil
	})
}

// ---------- HTTP API ----------

// TestCaptureToggle checks the capture contract: off by default, timestamped
// file name, only records written after it is switched on, clean stop.
func TestCaptureToggle(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	fp.rx <- []byte("before capture\r\n")
	waitFor(t, "first record", func() bool { return findRec(records(a), "rx", "before capture") != nil })

	if on, _, _ := a.cap.Info(); on {
		t.Fatal("capture must start disabled")
	}

	dir := t.TempDir()
	path, err := a.SetCapture(true, dir)
	if err != nil {
		t.Fatalf("start capture: %v", err)
	}
	if !regexp.MustCompile(`_\d{8}_\d{6}\.log$`).MatchString(path) {
		t.Fatalf("file name must carry a timestamp: %s", path)
	}
	// The file has to be recognisable by port, otherwise captures from two
	// ports cannot be told apart.
	if !strings.HasPrefix(filepath.Base(path), cfg.ComPort+"_") {
		t.Fatalf("file name must start with the port name (%s): %s", cfg.ComPort, path)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("capture not written to the requested folder: %s", path)
	}

	fp.rx <- []byte("during capture\r\n")
	waitFor(t, "captured line", func() bool {
		raw, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(raw), "during capture")
	})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "before capture") {
		t.Fatal("records that predate the capture must not be written")
	}
	if !strings.Contains(string(raw), "RX") {
		t.Fatalf("capture line missing direction tag:\n%s", raw)
	}

	a.SetCapture(false, "")
	if on, _, _ := a.cap.Info(); on {
		t.Fatal("capture still enabled after stop")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("captured file disappeared: %v", err)
	}
}

func TestHTTPAPI(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newWebServer(a).handler())
	defer srv.Close()

	get := func(path string) (int, string) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}
	post := func(path, body string) (int, string) {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}

	if code, body := get("/api/state"); code != 200 || !strings.Contains(body, "COM_TEST") {
		t.Fatalf("state: %d %s", code, body)
	}
	if code, body := get("/api/ports"); code != 200 || !strings.Contains(body, "ports") {
		t.Fatalf("ports: %d %s", code, body)
	}

	if code, body := post("/api/send", `{"text":"AT","hex":false,"eol":"crlf"}`); code != 200 {
		t.Fatalf("send: %d %s", code, body)
	}
	if got := fp.writtenString(); got != "AT\r\n" {
		t.Fatalf("device got %q", got)
	}

	if code, body := post("/api/send", `{"text":"41 42","hex":true,"eol":"none"}`); code != 200 {
		t.Fatalf("hex send: %d %s", code, body)
	}
	if got := fp.writtenString(); got != "AT\r\nAB" {
		t.Fatalf("device got %q after hex send", got)
	}
	if code, _ := post("/api/send", `{"text":"41 4","hex":true}`); code != 400 {
		t.Fatal("malformed hex must be rejected")
	}

	fp.rx <- []byte("api line\r\n")
	waitFor(t, "record via API", func() bool {
		_, body := get("/api/records?tail=50")
		return strings.Contains(body, "api line")
	})

	code, body := get("/api/export?format=txt")
	if code != 200 || !strings.Contains(body, "api line") {
		t.Fatalf("export: %d %s", code, body)
	}

	code, body = get("/")
	if code != 200 || !strings.Contains(body, "Serial Log") {
		t.Fatalf("index: %d", code)
	}
	if code, _ := get("/app.js"); code != 200 {
		t.Fatalf("app.js: %d", code)
	}

	var state map[string]any
	_, raw := get("/api/state")
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("state json: %v", err)
	}
	if state["status"] != "open" {
		t.Fatalf("status = %v", state["status"])
	}

	// Clear must empty the log but keep the port open.
	post("/api/clear", "")
	if recs := records(a); len(recs) != 0 {
		t.Fatalf("clear left %d records", len(recs))
	}
	// Close through the API.
	post("/api/close", "")
	if a.State()["open"].(bool) {
		t.Fatal("port still open after /api/close")
	}
}

// TestStreamDeliversLiveRX checks the terminal stream: bytes from the device
// must reach the browser as SSE data frames, decoded, with no TX echo.
func TestStreamDeliversLiveRX(t *testing.T) {
	cfg := testConfig(t)
	fp := newFakePort()
	installFake(t, fp)
	a := newTestApp(t, cfg)
	if err := a.Open(cfg.portParams()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newWebServer(a).handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	waitFor(t, "terminal subscriber", func() bool {
		for _, s := range a.Bus().list() {
			if s["name"] == "terminal" {
				return true
			}
		}
		return false
	})
	fp.rx <- []byte("live bytes\r\n")

	br := bufio.NewReader(res.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var msg struct {
			D string `json:"d"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "data: ")), &msg); err != nil {
			t.Fatalf("bad frame %q: %v", line, err)
		}
		if strings.Contains(msg.D, "live bytes") {
			return // RX reached the terminal
		}
		if strings.Contains(msg.D, "should-not-echo") {
			t.Fatal("TX must not be echoed to the terminal")
		}
	}
	t.Fatal("stream did not deliver the RX chunk")
}

func TestDemoModeFeedsPipeline(t *testing.T) {
	cfg := testConfig(t)
	cfg.ComPort = ""
	installFake(t, newFakePort())
	a := newTestApp(t, cfg)
	a.StartDemo()
	waitFor(t, "demo records", func() bool { return len(records(a)) > 3 })
	a.StopDemo()
	if a.State()["status"] != "closed" {
		t.Fatalf("status after demo = %v", a.State()["status"])
	}
}
