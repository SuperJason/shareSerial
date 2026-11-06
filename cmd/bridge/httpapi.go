package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed assets
var assetsFS embed.FS

type webServer struct {
	app   *App
	rtlog interface{ Printf(string, ...any) }
	mux   *http.ServeMux
	port  int // actual listening port
}

func newWebServer(a *App) *webServer {
	w := &webServer{app: a, rtlog: a.Logger(), mux: http.NewServeMux()}
	w.routes()
	return w
}

func (w *webServer) handler() http.Handler { return w.mux }

func (w *webServer) routes() {
	w.serveAsset("/", "assets/index.html", "text/html; charset=utf-8")
	w.serveAsset("/app.js", "assets/app.js", "application/javascript; charset=utf-8")
	w.serveAsset("/app.css", "assets/app.css", "text/css; charset=utf-8")
	w.serveAsset("/vendor/xterm.js", "assets/vendor/xterm.js", "application/javascript; charset=utf-8")
	w.serveAsset("/vendor/xterm.css", "assets/vendor/xterm.css", "text/css; charset=utf-8")
	w.serveAsset("/vendor/addon-fit.js", "assets/vendor/addon-fit.js", "application/javascript; charset=utf-8")
	w.serveAsset("/vendor/addon-search.js", "assets/vendor/addon-search.js", "application/javascript; charset=utf-8")
	w.mux.HandleFunc("/favicon.ico", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusNoContent)
	})

	w.mux.HandleFunc("/api/hello", w.handleHello)
	w.mux.HandleFunc("/llms.txt", w.handleLLMS)
	w.mux.HandleFunc("/api/state", w.handleState)
	w.mux.HandleFunc("/api/ports", w.handlePorts)
	w.mux.HandleFunc("/api/records", w.handleRecords)
	w.mux.HandleFunc("/api/text", w.handleText)
	w.mux.HandleFunc("/api/stream", w.handleStream)
	w.mux.HandleFunc("/api/send", w.handleSend)
	w.mux.HandleFunc("/api/open", w.handleOpen)
	w.mux.HandleFunc("/api/close", w.handleClose)
	w.mux.HandleFunc("/api/clear", w.handleClear)
	w.mux.HandleFunc("/api/capture", w.handleCapture)
	w.mux.HandleFunc("/api/schemes", w.handleSchemes)
	w.mux.HandleFunc("/api/scheme/", w.handleScheme)
	w.mux.HandleFunc("/api/export", w.handleExport)
	w.mux.HandleFunc("/api/demo", w.handleDemo)
}

func (w *webServer) serveAsset(path, name, ctype string) {
	data, err := assetsFS.ReadFile(name)
	if err != nil {
		return
	}
	w.mux.HandleFunc(path, func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", ctype)
		rw.Header().Set("Cache-Control", "no-store")
		rw.Write(data)
	})
}

func writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func writeErr(rw http.ResponseWriter, code int, err error) {
	writeJSON(rw, code, map[string]any{"ok": false, "error": err.Error()})
}

// handleHello is what an agent calls first: what this is, what it is doing, and
// where the data is.
func (w *webServer) handleHello(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, w.app.Hello(w.port))
}

// handleLLMS is the same thing as prose, for clients that read text.
func (w *webServer) handleLLMS(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	io.WriteString(rw, w.app.LLMSText(w.port))
}

func (w *webServer) handleState(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, w.app.State())
}

func (w *webServer) handlePorts(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, map[string]any{"ports": ListPorts()})
}

// handleRecords serves history for the initial page fill and for scrolling back.
func (w *webServer) handleRecords(rw http.ResponseWriter, r *http.Request) {
	if tail, _ := strconv.Atoi(r.URL.Query().Get("tail")); tail > 0 {
		recs, last := w.app.Ring().Tail(tail)
		writeJSON(rw, http.StatusOK, map[string]any{
			"records": recs,
			"seq":     last,
			"last":    last,
			"oldest":  w.app.Ring().Oldest(),
		})
		return
	}
	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, last := w.app.Ring().Since(after, limit)
	writeJSON(rw, http.StatusOK, map[string]any{
		"records": recs,
		"seq":     last,
		"last":    w.app.Ring().Last(),
		"oldest":  w.app.Ring().Oldest(),
	})
}

// handleText is the agent-facing reading surface: the same records the window
// shows, as plain lines instead of JSON, so a script does not have to parse
// anything. Two modes:
//
//	/api/text?tail=200            newest 200 lines
//	/api/text?after=1234&limit=200 lines newer than sequence 1234
//
// The response carries X-Seq-Next, which is the cursor to pass back to
// ?after= to get only what arrived since. That is what makes incremental
// reading possible without spooling to a file.
func (w *webServer) handleText(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tail, _ := strconv.Atoi(q.Get("tail"))
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))

	var recs []Rec
	var next uint64
	switch {
	case q.Get("after") != "" || q.Get("since") != "":
		if after == 0 {
			after, _ = strconv.ParseUint(q.Get("since"), 10, 64)
		}
		if limit <= 0 {
			limit = 500
		}
		if limit > 5000 {
			limit = 5000
		}
		recs, next = w.app.Ring().Since(after, limit)
	default:
		if tail <= 0 {
			tail = 100
		}
		if tail > 5000 {
			tail = 5000
		}
		recs, next = w.app.Ring().Tail(tail)
	}

	withTime := q.Get("ts") == "1"
	withDir := q.Get("dir") == "1"

	var b strings.Builder
	for _, rec := range recs {
		text := rec.Text
		if text == "" && rec.Hex != "" {
			text = rec.Hex
		}
		if withTime || withDir {
			if withDir {
				b.WriteString(strings.ToUpper(rec.Dir))
				b.WriteByte(' ')
			}
			if withTime {
				b.WriteString(time.UnixMilli(rec.Ts).Format("15:04:05.000"))
				b.WriteByte(' ')
			}
		}
		b.WriteString(text)
		b.WriteString("\n")
	}

	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Seq-Next", strconv.FormatUint(next, 10))
	rw.Header().Set("X-Seq-Last", strconv.FormatUint(w.app.Ring().Last(), 10))
	rw.Header().Set("X-Seq-Oldest", strconv.FormatUint(w.app.Ring().Oldest(), 10))
	rw.Write([]byte(b.String()))
}

// handleStream is the terminal byte stream: decoded RX chunks as they arrive,
// over SSE. It is deliberately not the record stream - a terminal needs the
// raw bytes (cursor addressing, ANSI colours, partial lines) rather than the
// display rows the log also produces.
//
// TX is not echoed back: a serial console normally echoes what it receives, so
// echoing locally would print every character twice.
func (w *webServer) handleStream(rw http.ResponseWriter, r *http.Request) {
	fl, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	fl.Flush()

	s := w.app.Bus().add("terminal", 4096)
	defer w.app.Bus().remove(s)

	dec := newCodec(w.app.Config().Encoding)
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-keepalive.C:
			if _, err := rw.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			fl.Flush()
		case msg := <-s.ch:
			if msg.dir != dirRx {
				continue
			}
			text := dec.Decode(msg.data)
			if len(text) == 0 {
				continue
			}
			payload, err := json.Marshal(map[string]string{"d": string(text)})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(rw, "data: %s\n\n", payload); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

type sendRequest struct {
	Text string `json:"text"`
	Hex  bool   `json:"hex"`
	EOL  string `json:"eol"` // none|cr|lf|crlf
}

func (w *webServer) handleSend(rw http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	var data []byte
	var err error
	if req.Hex {
		data, err = SendHex(req.Text)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
	} else {
		data = []byte(req.Text)
	}
	switch strings.ToLower(req.EOL) {
	case "cr":
		data = append(data, '\r')
	case "lf":
		data = append(data, '\n')
	case "crlf":
		data = append(data, '\r', '\n')
	case "", "none":
		// no terminator: hex commands usually carry their own framing
	}
	if len(data) == 0 {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "sent": 0})
		return
	}
	if err := w.app.Send(data, "local", 0); err != nil {
		writeErr(rw, http.StatusConflict, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "sent": len(data)})
}

type openRequest struct {
	ComPort  string `json:"com_port"`
	BaudRate int    `json:"baud_rate"`
	DataBits int    `json:"data_bits"`
	StopBits int    `json:"stop_bits"`
	Parity   string `json:"parity"`
	Encoding string `json:"encoding"`
	Save     *bool  `json:"save"`
}

func (w *webServer) handleOpen(rw http.ResponseWriter, r *http.Request) {
	var req openRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	cfg := w.app.Config()
	if req.ComPort != "" {
		cfg.ComPort = normalizePortName(req.ComPort)
	}
	if req.BaudRate > 0 {
		cfg.BaudRate = req.BaudRate
	}
	if req.DataBits > 0 {
		cfg.DataBits = req.DataBits
	}
	if req.StopBits > 0 {
		cfg.StopBits = req.StopBits
	}
	if req.Parity != "" {
		cfg.Parity = req.Parity
	}
	if req.Encoding != "" {
		cfg.Encoding = req.Encoding
	}
	if err := w.app.Reopen(cfg); err != nil {
		writeErr(rw, http.StatusConflict, err)
		return
	}
	if req.Save == nil || *req.Save {
		if path := w.app.ConfigPath(); path != "" {
			if err := saveConfig(path, cfg); err != nil {
				w.rtlog.Printf("save config failed: %v", err)
			}
		}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "state": w.app.State()})
}

func (w *webServer) handleClose(rw http.ResponseWriter, r *http.Request) {
	w.app.Close("closed from UI")
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleClear(rw http.ResponseWriter, r *http.Request) {
	w.app.Ring().Clear()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// seedSchemes writes the built-in schemes next to the config the first time the
// tool runs, so everything in the menu is an ordinary file the user can edit.
// Existing files are never overwritten.
func seedSchemes(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := fs.ReadDir(assetsFS, "assets/schemes")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		dst := filepath.Join(dir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := assetsFS.ReadFile("assets/schemes/" + e.Name())
		if err != nil {
			continue
		}
		os.WriteFile(dst, data, 0o644)
	}
	return nil
}

type schemeInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
}

// schemeName reads just the display name out of a scheme file.
func schemeName(data []byte, fallback string) string {
	var head struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &head); err == nil && strings.TrimSpace(head.Name) != "" {
		return head.Name
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fallback + " (invalid JSON)"
	}
	return fallback
}

// handleSchemes lists the built-in colour schemes plus any the user dropped
// into <config dir>/schemes/*.json.
func (w *webServer) handleSchemes(rw http.ResponseWriter, r *http.Request) {
	// Files on disk win, so editing one changes what the menu shows.
	disk := map[string]schemeInfo{}
	order := []string{}
	if dir := w.app.SchemesDir(); dir != "" {
		if fileEntries, err := os.ReadDir(dir); err == nil {
			for _, e := range fileEntries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				id := strings.TrimSuffix(e.Name(), ".json")
				disk[id] = schemeInfo{ID: id, Name: schemeName(data, id), Builtin: false}
				order = append(order, id)
			}
		}
	}

	out := []schemeInfo{}
	seen := map[string]bool{}
	entries, err := fs.ReadDir(assetsFS, "assets/schemes")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			if sc, ok := disk[id]; ok {
				out = append(out, sc)
				seen[id] = true
				continue
			}
			data, err := assetsFS.ReadFile("assets/schemes/" + e.Name())
			if err != nil {
				continue
			}
			out = append(out, schemeInfo{ID: id, Name: schemeName(data, id), Builtin: true})
			seen[id] = true
		}
	}
	for _, id := range order {
		if seen[id] {
			continue
		}
		out = append(out, disk[id])
	}
	writeJSON(rw, http.StatusOK, map[string]any{"schemes": out})
}

// handleScheme serves one scheme by id, user files first so they can override
// a built-in of the same name.
func (w *webServer) handleScheme(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/scheme/")
	id = strings.TrimSuffix(id, ".json")
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") || strings.ContainsAny(id, " 	") {
		writeErr(rw, http.StatusBadRequest, fmt.Errorf("bad scheme id"))
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	if dir := w.app.SchemesDir(); dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, id+".json")); err == nil {
			rw.Write(data)
			return
		}
	}
	if data, err := assetsFS.ReadFile("assets/schemes/" + id + ".json"); err == nil {
		rw.Write(data)
		return
	}
	writeErr(rw, http.StatusNotFound, fmt.Errorf("unknown scheme %q", id))
}

type captureRequest struct {
	On  *bool  `json:"on"`
	Dir string `json:"dir"`
}

// handleCapture toggles the text capture. Off by default; the window's context
// menu drives this endpoint.
func (w *webServer) handleCapture(rw http.ResponseWriter, r *http.Request) {
	var req captureRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(rw, http.StatusBadRequest, err)
		return
	}
	on := true
	if req.On == nil {
		// No toggle requested: just remember the folder for the next capture.
		if strings.TrimSpace(req.Dir) == "" {
			writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if err := w.app.SetCaptureDir(req.Dir); err != nil {
			writeErr(rw, http.StatusConflict, err)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "dir": req.Dir})
		return
	}
	on = *req.On
	path, err := w.app.SetCapture(on, req.Dir)
	if err != nil {
		writeErr(rw, http.StatusConflict, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "enabled": on, "path": path})
}

// handleExport writes the retained history as a downloadable text log.
func (w *webServer) handleExport(rw http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	recs, _ := w.app.Ring().Since(0, 5000)
	var buf bytes.Buffer
	for {
		if len(recs) == 0 {
			break
		}
		for _, rec := range recs {
			if format == "hex" && rec.Hex != "" {
				fmt.Fprintf(&buf, "%s %-3s %s\n", time.UnixMilli(rec.Ts).Format("2006-01-02 15:04:05.000"), dirLabel(rec.Dir), strings.ReplaceAll(rec.Hex, "\n", " "))
				continue
			}
			fmt.Fprintf(&buf, "%s %-3s %-10s %s\n", time.UnixMilli(rec.Ts).Format("2006-01-02 15:04:05.000"), dirLabel(rec.Dir), rec.Src, rec.Text)
		}
		last := recs[len(recs)-1].Seq
		recs, _ = w.app.Ring().Since(last, 5000)
	}
	name := fmt.Sprintf("serial-log-%s.txt", time.Now().Format("20060102-150405"))
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Content-Disposition", "attachment; filename="+name)
	rw.Write(buf.Bytes())
}

// handleDemo toggles the synthetic source; it exists so the pipeline can be
// exercised end to end without hardware (and is used by the tests).
func (w *webServer) handleDemo(rw http.ResponseWriter, r *http.Request) {
	on := true
	if v := r.URL.Query().Get("on"); v != "" {
		on, _ = strconv.ParseBool(v)
	} else if r.Method == http.MethodPost {
		var body struct {
			On *bool `json:"on"`
		}
		if err := decodeJSON(r, &body); err == nil && body.On != nil {
			on = *body.On
		}
	}
	if on {
		w.app.StartDemo()
	} else {
		w.app.StopDemo()
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "demo": on})
}

func decodeJSON(r *http.Request, v any) error {
	if r.Method == http.MethodGet {
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// startWeb serves the UI on loopback only: the TCP ports are the network
// surface, the HTTP API stays local.
func startWeb(a *App, port int) (*http.Server, net.Listener, error) {
	w := newWebServer(a)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		return nil, nil, err
	}
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		w.port = addr.Port
	}
	srv := &http.Server{Handler: w.handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return srv, ln, nil
}
