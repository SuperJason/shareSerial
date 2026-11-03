//go:build windows

package main

import (
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmInitMenuPopup = 0x0117
	trayWindowClass = "SystrayClass"
)

var (
	user32                    = syscall.NewLazyDLL("user32.dll")
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	pFindWindowW              = user32.NewProc("FindWindowW")
	pSetWindowLongPtrW        = user32.NewProc("SetWindowLongPtrW")
	pCallWindowProcW          = user32.NewProc("CallWindowProcW")
	pGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	pGetCurrentProcessId      = kernel32.NewProc("GetCurrentProcessId")

	hookMu   sync.Mutex
	origProc uintptr
	hookCb   func()
)

// startMenuOpenHook runs cb whenever the tray menu is about to be displayed,
// instead of refreshing on a poll timer.
//
// systray shows its menu via TrackPopupMenu, which sends WM_INITMENUPOPUP to
// the owner window right before the menu becomes visible - the standard
// moment for an application to update menu state. The systray library does
// not surface that message, so its window (class "SystrayClass", created in
// this process) is subclassed here: every message is forwarded to the
// original procedure untouched, and WM_INITMENUPOPUP additionally triggers
// the refresh.
//
// cb runs on systray's message-loop thread, synchronously inside the
// WM_INITMENUPOPUP dispatch: whatever it changes is exactly what the user
// sees when the menu appears. It must be fast (the menu display waits for
// it) and must not panic - the panic guard here keeps a broken callback from
// killing the message loop.
func startMenuOpenHook(cb func()) {
	hookMu.Lock()
	hookCb = cb
	hookMu.Unlock()

	go func() {
		hwnd := uintptr(0)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			hwnd = findOwnSystrayWindow()
			if hwnd != 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if hwnd == 0 {
			trayLog.Printf("[tray] menu-open hook not installed: systray window not found")
			return
		}

		hookMu.Lock()
		defer hookMu.Unlock()
		if origProc != 0 {
			return // already subclassed
		}
		proc, _, _ := pSetWindowLongPtrW.Call(
			hwnd,
			^uintptr(3), // GWLP_WNDPROC (-4)
			syscall.NewCallback(trayWndProc),
		)
		if proc == 0 {
			trayLog.Printf("[tray] menu-open hook not installed: subclassing failed")
			return
		}
		origProc = proc
	}()
}

// findOwnSystrayWindow returns the systray window of THIS process. FindWindow
// matches windows of every process, and another systray-based application
// uses the same window class; subclassing a foreign window fails silently.
func findOwnSystrayWindow() uintptr {
	class, err := syscall.UTF16PtrFromString(trayWindowClass)
	if err != nil {
		return 0
	}
	hwnd, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(class)), 0)
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	pGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	cur, _, _ := pGetCurrentProcessId.Call()
	if pid != uint32(cur) {
		return 0
	}
	return hwnd
}

// trayWndProc forwards to the original systray procedure and refreshes the
// menu state on WM_INITMENUPOPUP.
func trayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	if msg == wmInitMenuPopup {
		hookMu.Lock()
		cb := hookCb
		hookMu.Unlock()
		if cb != nil {
			func() {
				defer func() { _ = recover() }()
				cb()
			}()
		}
	}
	if origProc != 0 {
		ret, _, _ := pCallWindowProcW.Call(origProc, hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
	return 0
}
