package main

import (
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/getlantern/systray"
)

//go:embed bridge-hub.ico
var appIcon []byte

// maxTrayBoards bounds the per-board submenu slots. The slots are pre-created
// (hidden) when the tray starts, so their position in the menu is fixed —
// systray places items by creation order and dynamically created items would
// land at the bottom, after "退出". Boards are assigned to slots as they are
// detected; surplus slots stay hidden.
const maxTrayBoards = 8

var (
	trayCtrl *appController
	trayLog  *log.Logger
	mStatus  *systray.MenuItem
	mStart   *systray.MenuItem
	mStop    *systray.MenuItem
	mRestart *systray.MenuItem
	mWeb     *systray.MenuItem
	mCfg     *systray.MenuItem
	mQuit    *systray.MenuItem

	// FTDI per-board submenu slots, between the summary line and the global
	// settings menu.
	mVbusSummary *systray.MenuItem
	boardSlots   []*boardMenu

	// FTDI global settings submenu. Changes persist to the config file and
	// restart the bridge, matching what the Web UI's config form does.
	mFtdiCfg    *systray.MenuItem
	mFtdiEnable *systray.MenuItem
	mFtdiActive *systray.MenuItem
	mFtdiPin    *systray.MenuItem
)

// boardMenu owns one board submenu slot: the menu items and the board
// currently assigned to the slot. Assignment changes as boards come and go;
// the click handlers act on whatever serial is assigned at click time.
type boardMenu struct {
	mu     sync.Mutex
	serial string

	root      *systray.MenuItem
	status    *systray.MenuItem
	on        *systray.MenuItem
	off       *systray.MenuItem
	serialLbl *systray.MenuItem
}

// runTray blocks until the tray is closed (systray.Run).
func runTray(ctrl *appController, logger *log.Logger) {
	trayCtrl = ctrl
	trayLog = logger
	systray.Run(onTrayReady, onTrayExit)
}

func onTrayReady() {
	systray.SetIcon(appIcon)
	systray.SetTitle("Serial Bridge Hub")
	systray.SetTooltip("COM22 -> TCP 桥接 · hub4com 托管")
	mStatus = systray.AddMenuItem("状态: 未知", "")
	mStatus.Disable()
	systray.AddSeparator()
	mStart = systray.AddMenuItem("启动桥接", "Start bridge")
	mStop = systray.AddMenuItem("停止桥接", "Stop bridge")
	mRestart = systray.AddMenuItem("重启桥接", "Restart bridge")
	systray.AddSeparator()
	mVbusSummary = systray.AddMenuItem("DUT VBUS: —", "FTDI CBUS pin state")
	mVbusSummary.Disable()

	// Board slots: created here, before the settings items, so they sit
	// between the summary and the settings; hidden until a board is assigned.
	for i := 0; i < maxTrayBoards; i++ {
		boardSlots = append(boardSlots, newBoardMenuSlot(i+1))
	}

	mFtdiCfg = systray.AddMenuItem("VBUS IO 配置", "Global FTDI CBUS defaults; restarts the bridge on change")
	mFtdiEnable = mFtdiCfg.AddSubMenuItemCheckbox("启用 VBUS 控制", "ftdi_enabled; restarts the bridge on change", true)
	mFtdiActive = mFtdiCfg.AddSubMenuItemCheckbox("低电平有效 (active low)", "ftdi_active_low; restarts the bridge on change", false)
	mFtdiPin = mFtdiCfg.AddSubMenuItem("CBUS 引脚: 0", "ftdi_cbus_pin; click to cycle 0-3")
	systray.AddSeparator()
	mWeb = systray.AddMenuItem("打开 Web UI", fmt.Sprintf("Open http://127.0.0.1:%d", trayCtrl.webPort))
	mCfg = systray.AddMenuItem("打开配置文件", "Open config file in notepad")
	systray.AddSeparator()
	mQuit = systray.AddMenuItem("退出", "Quit")

	// Refresh the whole menu whenever it is about to be displayed instead of
	// polling on a timer: the state the user sees is read at the moment the
	// menu opens. Click handlers also refresh directly after acting.
	startMenuOpenHook(func() {
		updateTrayStatus()
	})

	go trayEventLoop()
	go updateTrayStatus() // initial paint for the tooltip-level state
}

func onTrayExit() {
	_ = trayCtrl.Stop()
}

// trayEventLoop dispatches menu clicks. Status refreshes are driven by the
// menu-open hook (see startMenuOpenHook), not by a ticker.
func trayEventLoop() {
	for {
		select {
		case <-mStart.ClickedCh:
			if err := trayCtrl.Start(); err != nil {
				trayLog.Printf("[tray] start failed: %v", err)
			}
			updateTrayStatus()
		case <-mStop.ClickedCh:
			if err := trayCtrl.Stop(); err != nil {
				trayLog.Printf("[tray] stop: %v", err)
			}
			updateTrayStatus()
		case <-mRestart.ClickedCh:
			if err := trayCtrl.Restart(); err != nil {
				trayLog.Printf("[tray] restart failed: %v", err)
			}
			updateTrayStatus()
		case <-mFtdiEnable.ClickedCh:
			applyFTDIConfigFromTray("ftdi_enabled", func(cfg *bridgeConfig) {
				cfg.FtdiEnabled = boolPtr(!cfg.FTDIEnabled())
			})
			updateTrayStatus()
		case <-mFtdiActive.ClickedCh:
			applyFTDIConfigFromTray("ftdi_active_low", func(cfg *bridgeConfig) {
				cfg.FtdiActiveLow = !cfg.FtdiActiveLow
			})
			updateTrayStatus()
		case <-mFtdiPin.ClickedCh:
			applyFTDIConfigFromTray("ftdi_cbus_pin", func(cfg *bridgeConfig) {
				cfg.FtdiCbusPin = (cfg.FtdiCbusPin + 1) % cbusPinCount
			})
			updateTrayStatus()
		case <-mWeb.ClickedCh:
			openInBrowser(fmt.Sprintf("http://127.0.0.1:%d", trayCtrl.webPort))
		case <-mCfg.ClickedCh:
			if err := openConfigInEditor(trayCtrl.cfgPath); err != nil {
				trayLog.Printf("[tray] open config file: %v", err)
			}
		case <-mQuit.ClickedCh:
			_ = trayCtrl.Stop()
			systray.Quit()
			return
		}
	}
}

// newBoardMenuSlot creates one board submenu at a fixed position, hidden.
func newBoardMenuSlot(n int) *boardMenu {
	root := systray.AddMenuItem(fmt.Sprintf("板卡 %d", n), "One detected FTDI tool board")
	bm := &boardMenu{
		root:      root,
		status:    root.AddSubMenuItem("DUT VBUS: —", "FTDI CBUS pin state"),
		on:        root.AddSubMenuItem("插入 DUT USB (VBUS ON)", "Simulate DUT USB plug-in"),
		off:       root.AddSubMenuItem("拔出 DUT USB (VBUS OFF)", "Simulate DUT USB unplug"),
		serialLbl: root.AddSubMenuItem("序列号: —", "Board serial number"),
	}
	bm.status.Disable()
	bm.on.Disable()
	bm.off.Disable()
	bm.serialLbl.Disable()
	root.Hide()
	go bm.loop()
	return bm
}

func (bm *boardMenu) loop() {
	for {
		select {
		case <-bm.on.ClickedCh:
			bm.act(true)
		case <-bm.off.ClickedCh:
			bm.act(false)
		}
	}
}

func (bm *boardMenu) act(on bool) {
	bm.mu.Lock()
	serial := bm.serial
	bm.mu.Unlock()
	if serial == "" {
		return
	}
	if err := trayCtrl.SetVBUS(serial, on); err != nil {
		trayLog.Printf("[tray] VBUS %s failed for %s: %v", onOff(on), serial, err)
	}
	updateTrayStatus()
}

func updateTrayStatus() {
	if trayCtrl == nil {
		return
	}
	run := trayCtrl.running()
	label := "状态: 已停止"
	if run {
		label = "状态: 运行中"
	}
	if mStatus != nil {
		mStatus.SetTitle(label)
	}
	if mStart != nil {
		mStart.SetTitle(ternary(run, "启动桥接 (已运行)", "启动桥接"))
		mStart.Enable()
	}
	if mStop != nil {
		if run {
			mStop.Enable()
		} else {
			mStop.Disable()
		}
	}
	updateTrayVBUS()
	updateTrayFTDIConfig()
}

// applyFTDIConfigFromTray persists an FTDI setting change and restarts the
// bridge so it takes effect. The checkbox state itself is not set here: the
// next refresh (menu open, or directly after the click) reads the config back
// and aligns the checkmarks, which also reverts the mark when the change
// failed.
func applyFTDIConfigFromTray(name string, mutate func(*bridgeConfig)) {
	if err := trayCtrl.ApplyFTDIConfig(mutate); err != nil {
		trayLog.Printf("[tray] changing %s failed: %v", name, err)
		return
	}
	trayLog.Printf("[tray] %s changed; bridge restarted to apply it", name)
}

// updateTrayFTDIConfig reflects the FTDI settings in the submenu. Reading them
// from the controller on every refresh (rather than tracking local state)
// keeps the checkmarks consistent with changes made from the Web UI or the
// config file too.
func updateTrayFTDIConfig() {
	if mFtdiCfg == nil || mFtdiEnable == nil || mFtdiActive == nil || mFtdiPin == nil {
		return
	}

	enabled, activeLow, pin, hubMode := trayCtrl.FTDIConfigView()

	if enabled {
		mFtdiEnable.Check()
	} else {
		mFtdiEnable.Uncheck()
	}
	if activeLow {
		mFtdiActive.Check()
	} else {
		mFtdiActive.Uncheck()
	}
	mFtdiPin.SetTitle(fmt.Sprintf("CBUS 引脚: %d", pin))

	// Without hub4com there is no restart window to free the serial port, so
	// validation force-disables the feature; the toggles would be misleading.
	if hubMode {
		mFtdiEnable.Enable()
	} else {
		mFtdiEnable.Disable()
	}
}

// updateTrayVBUS assigns the detected boards to the submenu slots and
// refreshes every slot's state. A board's slot is keyed by position in the
// board list (order is stable across refreshes), and surplus slots are hidden.
func updateTrayVBUS() {
	if mVbusSummary == nil {
		return
	}

	st, err := trayCtrl.FTDIStatus()
	if err != nil {
		if errors.Is(err, errVBUSFeatureDisabled) {
			// Say why the items are unavailable, rather than leaving a bare dash.
			mVbusSummary.SetTitle("DUT VBUS: 未启用 (ftdi_enabled=false)")
		} else {
			mVbusSummary.SetTitle("DUT VBUS: 桥接未运行")
		}
		for _, bm := range boardSlots {
			bm.clear()
		}
		return
	}

	boardsRaw, _ := st["boards"].([]map[string]any)
	if len(boardsRaw) == 0 {
		mVbusSummary.SetTitle("DUT VBUS: 未检测到板卡")
		for _, bm := range boardSlots {
			bm.clear()
		}
		return
	}

	on, off, unknown := 0, 0, 0
	for i, raw := range boardsRaw {
		serial, _ := raw["serial"].(string)
		com, _ := raw["comPort"].(string)
		known, _ := raw["vbusKnown"].(bool)
		vbusOn, _ := raw["vbusOn"].(bool)
		pin, _ := raw["pin"].(int)

		switch {
		case !known:
			unknown++
		case vbusOn:
			on++
		default:
			off++
		}

		if i < len(boardSlots) {
			bm := boardSlots[i]
			bm.assign(serial, com, known, vbusOn, pin)
		}
	}
	for i := len(boardsRaw); i < len(boardSlots); i++ {
		boardSlots[i].clear()
	}

	mVbusSummary.SetTitle(fmt.Sprintf("DUT VBUS: %d 板卡 · %d ON / %d OFF / %d 未知",
		len(boardsRaw), on, off, unknown))
}

// assign fills a slot with one board's state and shows it.
func (bm *boardMenu) assign(serial, com string, known, vbusOn bool, pin int) {
	bm.mu.Lock()
	bm.serial = serial
	bm.mu.Unlock()

	bm.root.SetTitle(boardMenuTitle(com, serial))
	bm.serialLbl.SetTitle(fmt.Sprintf("序列号: %s", serial))

	switch {
	case !known:
		bm.status.SetTitle(fmt.Sprintf("DUT VBUS: 未知 · CBUS%d", pin))
	case vbusOn:
		bm.status.SetTitle(fmt.Sprintf("DUT VBUS: 已插入 (ON) · CBUS%d", pin))
	default:
		bm.status.SetTitle(fmt.Sprintf("DUT VBUS: 已拔出 (OFF) · CBUS%d", pin))
	}

	bm.on.Enable()
	bm.off.Enable()
	// Leave only the action that changes something clickable.
	if known && vbusOn {
		bm.on.Disable()
	}
	if known && !vbusOn {
		bm.off.Disable()
	}
	bm.root.Show()
}

// clear empties a slot and hides it from the menu.
func (bm *boardMenu) clear() {
	bm.mu.Lock()
	bm.serial = ""
	bm.mu.Unlock()
	bm.root.Hide()
}

// boardMenuTitle names a board's submenu. The COM port is the friendliest
// handle the user has; the serial number is the fallback.
func boardMenuTitle(com, serial string) string {
	if com != "" {
		return com
	}
	if serial != "" {
		return serial
	}
	return "板卡"
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func openInBrowser(url string) {
	_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
}

// openConfigInEditor opens the config file for editing.
//
// Notepad cannot be spawned directly on every machine: on Windows 11 with the
// Store Notepad, notepad.exe exists only as an app-execution alias under
// WindowsApps, which Go's exec.LookPath cannot resolve (the package
// directories deny stat), so exec.Command("notepad.exe", ...) fails with
// "executable file not found". cmd's `start` goes through ShellExecute
// instead, which understands aliases; the FileProtocolHandler fallback opens
// the file with its default associated application.
func openConfigInEditor(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	if err := exec.Command("cmd", "/c", "start", "notepad", abs).Start(); err == nil {
		return nil
	}
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", abs).Start()
}
