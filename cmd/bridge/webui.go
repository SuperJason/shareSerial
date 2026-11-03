package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// appController owns the lifecycle of the bridge inside tray/web mode:
// it can start, stop and restart a bridgeApp instance on demand.
type appController struct {
	mu        sync.Mutex
	cfgPath   string
	cfg       bridgeConfig
	app       *bridgeApp
	logger    *log.Logger
	webPort   int
	startedAt time.Time
	lastErr   string
}

func newAppController(cfgPath string, cfg bridgeConfig, logger *log.Logger, webPort int) *appController {
	return &appController{cfgPath: cfgPath, cfg: cfg, logger: logger, webPort: webPort}
}

// Current returns a snapshot of the running app (may be nil).
func (c *appController) Current() *bridgeApp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.app
}

func (c *appController) running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.app != nil
}

// Start launches the bridge with the current config. It replaces any running
// instance (stop first if needed).
func (c *appController) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app != nil {
		return fmt.Errorf("bridge is already running")
	}
	app, err := newBridgeApp(c.cfgPath, c.cfg, c.logger)
	if err != nil {
		c.lastErr = err.Error()
		return err
	}
	c.app = app
	c.startedAt = time.Now()
	c.lastErr = ""
	go app.run() // acceptLoop/connectLoop (blocking; runs until Stop)
	c.logger.Printf("[tray] bridge started")
	return nil
}

// Stop halts the running bridge instance, if any.
func (c *appController) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.app == nil {
		return fmt.Errorf("bridge is not running")
	}
	app := c.app
	c.app = nil
	app.Stop()
	c.logger.Printf("[tray] bridge stopped")
	return nil
}

// Restart stops the bridge (if running) and starts it again with c.cfg.
func (c *appController) Restart() error {
	c.mu.Lock()
	if c.app != nil {
		app := c.app
		c.app = nil
		app.Stop()
	}
	app, err := newBridgeApp(c.cfgPath, c.cfg, c.logger)
	if err != nil {
		c.lastErr = err.Error()
		c.mu.Unlock()
		return err
	}
	c.app = app
	c.startedAt = time.Now()
	c.lastErr = ""
	c.mu.Unlock()
	go app.run() // acceptLoop/connectLoop (blocking; runs until Stop)
	c.logger.Printf("[tray] bridge restarted")
	return nil
}

// UpdateConfig validates and persists a new config, then applies it to the
// controller (used by PUT /api/config). The bridge must be restarted by the
// caller for the new config to take effect (or Restart() is used directly).
func (c *appController) UpdateConfig(cfg bridgeConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
	return nil
}

func (c *appController) statusMap() map[string]any {
	c.mu.Lock()
	app := c.app
	cfg := c.cfg
	started := c.startedAt
	lastErr := c.lastErr
	c.mu.Unlock()

	st := map[string]any{
		"running":    app != nil,
		"configPath": c.cfgPath,
		"webPort":    c.webPort,
		"startedAt":  "",
		"lastError":  lastErr,
		"clients":    0,
	}
	if !started.IsZero() {
		st["startedAt"] = started.Format(time.RFC3339)
	}
	if app != nil {
		st["clients"] = app.hub.Count()
		st["mode"] = app.socketMode
		if app.hub4com != nil {
			st["hub4com"] = app.hub4com.Status()
		}
		if app.ftdi != nil {
			st["ftdi"] = app.ftdi.Status()
		}
	}
	// LAN IP of this host (informational)
	if ips := localIPv4s(); len(ips) > 0 {
		st["lanIPs"] = ips
	}
	_ = cfg
	return st
}

func localIPv4s() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

// --- HTTP handlers -------------------------------------------------------

type webServer struct {
	ctrl *appController
	log  *log.Logger
}

func (w *webServer) writeJSON(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(code)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *webServer) writeErr(rw http.ResponseWriter, code int, msg string) {
	w.writeJSON(rw, code, map[string]any{"ok": false, "error": msg})
}

func (w *webServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/start", w.handleStart)
	mux.HandleFunc("/api/stop", w.handleStop)
	mux.HandleFunc("/api/restart", w.handleRestart)
	mux.HandleFunc("/api/logs", w.handleLogs)
	mux.HandleFunc("/api/com0com", w.handleCom0comList)
	mux.HandleFunc("/api/com0com/rename", w.handleCom0comRename)
	mux.HandleFunc("/api/com0com/create", w.handleCom0comCreate)
	mux.HandleFunc("/api/com0com/remove", w.handleCom0comRemove)
	mux.HandleFunc("/api/vbus", w.handleVBUS)
	return mux
}

func (w *webServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write([]byte(indexHTML))
}

func (w *webServer) handleStatus(rw http.ResponseWriter, r *http.Request) {
	st := w.ctrl.statusMap()
	st["config"] = w.ctrl.cfg
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": st})
}

func (w *webServer) handleConfig(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": w.ctrl.cfg})
	case http.MethodPut, http.MethodPost:
		var cfg bridgeConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			w.writeErr(rw, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := w.ctrl.UpdateConfig(cfg); err != nil {
			w.writeErr(rw, http.StatusBadRequest, err.Error())
			return
		}
		if err := saveConfig(w.ctrl.cfgPath, w.ctrl.cfg); err != nil {
			w.writeErr(rw, http.StatusInternalServerError, "save config failed: "+err.Error())
			return
		}
		w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": w.ctrl.cfg})
	default:
		w.writeErr(rw, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (w *webServer) handleStart(rw http.ResponseWriter, r *http.Request) {
	if err := w.ctrl.Start(); err != nil {
		w.writeErr(rw, http.StatusConflict, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleStop(rw http.ResponseWriter, r *http.Request) {
	if err := w.ctrl.Stop(); err != nil {
		w.writeErr(rw, http.StatusConflict, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleRestart(rw http.ResponseWriter, r *http.Request) {
	if err := w.ctrl.Restart(); err != nil {
		w.writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *webServer) handleLogs(rw http.ResponseWriter, r *http.Request) {
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		fmt.Sscanf(v, "%d", &lines)
	}
	path := strings.TrimSpace(w.ctrl.cfg.LogFile)
	if path == "" {
		w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": ""})
		return
	}
	content, err := readTail(path, lines)
	if err != nil {
		w.writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": content})
}

func (w *webServer) handleCom0comList(rw http.ResponseWriter, r *http.Request) {
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": com0comStatus()})
}

func (w *webServer) handleCom0comRename(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Pair string `json:"pair"`
		Port string `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeErr(rw, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := com0comRename(req.Pair, req.Port); err != nil {
		w.writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "message": "rename submitted (UAC may have been shown)"})
}

func (w *webServer) handleCom0comCreate(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		PortA string `json:"portA"`
		PortB string `json:"portB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeErr(rw, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := com0comCreate(req.PortA, req.PortB); err != nil {
		w.writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "message": "create submitted (UAC may have been shown)"})
}

func (w *webServer) handleCom0comRemove(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		PairA string `json:"pairA"`
		PairB string `json:"pairB"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.writeErr(rw, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := com0comRemove(req.PairA, req.PairB); err != nil {
		w.writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "message": "remove submitted (UAC may have been shown)"})
}

func readTail(path string, lines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const chunk = 4096
	var buf []byte
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := stat.Size()
	pos := size
	for pos > 0 && len(buf) < 256*1024 {
		read := int64(chunk)
		if read > pos {
			read = pos
		}
		pos -= read
		tmp := make([]byte, read)
		if _, err := f.ReadAt(tmp, pos); err != nil {
			return "", err
		}
		buf = append(tmp, buf...)
		if len(buf) >= 256*1024 {
			break
		}
	}
	// split into lines, keep last N
	all := string(buf)
	parts := strings.Split(all, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n"), nil
}

// startWebServer runs the HTTP server on 127.0.0.1:<port>. Non-blocking.
func startWebServer(ctrl *appController, logger *log.Logger) (*http.Server, net.Listener, error) {
	ws := &webServer{ctrl: ctrl, log: logger}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ctrl.webPort))
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: ws.routes()}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Printf("[web] server error: %v", err)
		}
	}()
	logger.Printf("[web] UI listening on http://127.0.0.1:%d", ctrl.webPort)
	return srv, ln, nil
}

var _ = filepath.Join // keep import if unused in some build modes
