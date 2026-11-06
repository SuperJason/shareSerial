//go:build windows

package main

// A double-clicked console binary gets a console window of its own, which sits
// behind the serial window and reads as a stray second window. That console has
// exactly one attached process; when the tool is started from cmd, PowerShell or
// Windows Terminal the console is shared and must be left alone, because that is
// where the banner and the diagnostics go.

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procShowWindow            = user32.NewProc("ShowWindow")
	procMessageBoxW           = user32.NewProc("MessageBoxW")
)

// consoleOwned reports whether the console belongs to this process alone, i.e.
// Windows created it because the program was launched without one.
func consoleOwned() bool {
	if h, _, _ := procGetConsoleWindow.Call(); h == 0 {
		return false
	}
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// hideOwnConsole hides the window of a console we own and reports whether it did.
func hideOwnConsole() bool {
	if !consoleOwned() {
		return false
	}
	h, _, _ := procGetConsoleWindow.Call()
	const swHide = 0
	if r, _, _ := procShowWindow.Call(h, swHide); r == 0 {
		return false
	}
	return true
}

// messageBox is the fallback for a failure nobody would see otherwise: with the
// console hidden, a startup error would simply vanish.
func messageBox(title, text string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	b, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const mbIconError = 0x00000010
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), mbIconError)
}
