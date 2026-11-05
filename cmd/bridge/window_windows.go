//go:build windows

package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2"
)

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
)

type winRect struct{ Left, Top, Right, Bottom int32 }

const (
	spiGetWorkArea = 0x0030
	smCxScreen     = 0
	smCyScreen     = 1
)

// desktopWorkArea returns the usable desktop (taskbar excluded). The WebView
// library centres on the full screen using the size it is given, so a window
// taller than the screen puts the title bar above the visible area.
func desktopWorkArea() (x, y, w, h int32) {
	var r winRect
	if ret, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0); ret != 0 &&
		r.Right > r.Left && r.Bottom > r.Top {
		return r.Left, r.Top, r.Right - r.Left, r.Bottom - r.Top
	}
	cx, _, _ := procGetSystemMetrics.Call(smCxScreen)
	cy, _, _ := procGetSystemMetrics.Call(smCyScreen)
	return 0, 0, int32(cx), int32(cy)
}

// openWindow shows the UI in a native WebView2 window and blocks until the
// user closes it. It reports false when the WebView2 runtime is missing so the
// caller can fall back to the default browser.
func openWindow(url string) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Fit the preferred size into the work area with room for the frame, so the
	// title bar is always reachable.
	ax, ay, aw, ah := desktopWorkArea()
	if aw <= 0 || ah <= 0 {
		aw, ah = 1280, 860
	}
	w, h := uint(1280), uint(860)
	if int(w) > int(aw)-80 {
		w = uint(int(aw) - 80)
	}
	if int(h) > int(ah)-80 {
		h = uint(int(ah) - 80)
	}
	if w < 640 {
		w = 640
	}
	if h < 400 {
		h = 400
	}
	fmt.Printf(" window  : %dx%d (work area %dx%d at %d,%d)\n", w, h, aw, ah, ax, ay)

	view := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "Serial Log — TCP bridge",
			Width:  w,
			Height: h,
			Center: false,
		},
	})
	if view == nil {
		return false
	}
	defer view.Destroy()

	// Place it centred inside the work area (not the full screen) and make sure
	// the frame is not pushed off the top edge.
	if hwnd := view.Window(); hwnd != nil {
		x := ax + (aw-int32(w))/2
		y := ay + (ah-int32(h))/2
		if y < ay {
			y = ay
		}
		swpNoZOrder, swpNoActivate := uintptr(0x0004), uintptr(0x0010)
		procSetWindowPos.Call(uintptr(unsafe.Pointer(hwnd)), 0,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate)
	}

	view.Navigate(url)
	view.Run() // blocks on the window message loop
	return true
}
