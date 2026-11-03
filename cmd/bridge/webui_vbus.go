package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// This file adds the VBUS control surface for the FTDI CBUS feature. It is
// additive: nothing here runs unless ftdi_enabled is set in the config.

var (
	errVBUSBridgeNotRunning = errors.New("bridge is not running")
	errVBUSFeatureDisabled  = errors.New("ftdi cbus control is not enabled (set ftdi_enabled in the config)")
)

// SetVBUS drives one board's CBUS pin, which gates that board's DUT VBUS.
// An empty serial targets the first detected board (single-board callers).
func (c *appController) SetVBUS(serial string, on bool) error {
	c.mu.Lock()
	app := c.app
	c.mu.Unlock()

	if app == nil {
		return errVBUSBridgeNotRunning
	}
	if app.ftdi == nil {
		return errVBUSFeatureDisabled
	}
	if serial == "" {
		boards := app.ftdi.Boards()
		if len(boards) == 0 {
			return fmt.Errorf("ftdi: no board detected; check the USB connection")
		}
		serial = boards[0].Info.Serial
	}
	return app.ftdi.SetVBUS(serial, on)
}

// FTDIStatus returns the FTDI/VBUS subsystem snapshot, including the
// auto-detected board, for the Web UI and tray.
func (c *appController) FTDIStatus() (map[string]any, error) {
	c.mu.Lock()
	app := c.app
	c.mu.Unlock()

	if app == nil {
		return nil, errVBUSBridgeNotRunning
	}
	if app.ftdi == nil {
		return nil, errVBUSFeatureDisabled
	}
	return app.ftdi.Status(), nil
}

// VBUSState reports the last known VBUS state of the first detected board.
// known is false when the state has neither been observed nor commanded, in
// which case on is meaningless.
func (c *appController) VBUSState() (on, known bool, err error) {
	c.mu.Lock()
	app := c.app
	c.mu.Unlock()

	if app == nil {
		return false, false, errVBUSBridgeNotRunning
	}
	if app.ftdi == nil {
		return false, false, errVBUSFeatureDisabled
	}
	boards := app.ftdi.Boards()
	if len(boards) == 0 {
		return false, false, fmt.Errorf("ftdi: no board detected")
	}
	return boards[0].vbusOn, boards[0].vbusKnown, nil
}

// FTDIConfigView returns the FTDI-related default settings as the tray and
// Web UI present them. hubMode reports whether hub4com is in use, since
// without it the feature is force-disabled by validation.
func (c *appController) FTDIConfigView() (enabled, activeLow bool, pin int, hubMode bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.FTDIEnabled(), c.cfg.FtdiActiveLow, c.cfg.FtdiCbusPin, strings.TrimSpace(c.cfg.Hub4comPath) != ""
}

// ApplyFTDIConfig mutates a copy of the current config, persists it and
// restarts the bridge so the new FTDI settings take effect immediately. The
// restart is required: ftdiManager is built once per bridgeApp and cannot be
// reconfigured in place.
func (c *appController) ApplyFTDIConfig(mutate func(*bridgeConfig)) error {
	c.mu.Lock()
	cfg := c.cfg
	running := c.app != nil
	c.mu.Unlock()

	mutate(&cfg)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := saveConfig(c.cfgPath, cfg); err != nil {
		return fmt.Errorf("save config failed: %w", err)
	}

	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()

	if !running {
		return nil
	}
	return c.Restart()
}

// handleVBUS serves the VBUS control endpoint.
//
//	GET  /api/vbus  -> current state
//	POST /api/vbus  {"on": true|false}
func (w *webServer) handleVBUS(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		st, err := w.ctrl.FTDIStatus()
		if err != nil {
			w.writeErr(rw, http.StatusConflict, err.Error())
			return
		}
		w.writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": st})

	case http.MethodPost:
		// On is a pointer so that a body of {} is rejected rather than being
		// silently treated as "turn VBUS off". Serial is optional: omitting it
		// targets the first detected board.
		var req struct {
			On     *bool  `json:"on"`
			Serial string `json:"serial"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.writeErr(rw, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if req.On == nil {
			w.writeErr(rw, http.StatusBadRequest, "field 'on' is required")
			return
		}
		if err := w.ctrl.SetVBUS(strings.TrimSpace(req.Serial), *req.On); err != nil {
			w.writeErr(rw, http.StatusConflict, err.Error())
			return
		}
		w.writeJSON(rw, http.StatusOK, map[string]any{
			"ok":   true,
			"data": map[string]any{"vbusOn": *req.On, "serial": strings.TrimSpace(req.Serial)},
		})

	default:
		w.writeErr(rw, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed; use GET or POST", r.Method))
	}
}
