//go:build windows

package main

// This binary is linked as a GUI application: double-clicking it produces no
// console window at all, which is the point - the serial window must be the only
// window on screen.
//
// The console is therefore something we have to ask for:
//   - started from a shell, the parent console is reused, so a script that runs
//     serial-log.exe still sees its output (AttachConsole);
//   - started by double-click, or when the parent has no console, -console
//     allocates a fresh one, which is opt-in and never appears by accident;
//   - without any console, startup failures go to a message box instead of
//     vanishing into a closed stdout.

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procAllocConsole  = kernel32.NewProc("AllocConsole")
	procMessageBoxW   = user32.NewProc("MessageBoxW")
)

// attachParentProcess is (DWORD)-1: the console of the parent process.
const attachParentProcess = ^uintptr(0)

// attachConsole makes stdout/stderr usable and reports whether they are.
// allowAlloc permits creating a new console when there is no parent one.
func attachConsole(allowAlloc bool) bool {
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 && allowAlloc {
		if a, _, _ := procAllocConsole.Call(); a != 0 {
			r = 1
		}
	}
	if r == 0 {
		return false
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
	return true
}

// messageBox is the fallback for a failure nobody would see otherwise.
func messageBox(title, text string) {
	t, err1 := syscall.UTF16PtrFromString(title)
	b, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const mbIconError = 0x00000010
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), mbIconError)
}
