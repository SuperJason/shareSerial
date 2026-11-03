//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const createNoWindowFlag = 0x08000000 // CREATE_NO_WINDOW

// hideWindow configures a child process (e.g. hub4com.exe) to run without a
// visible console window. HideWindow sets SW_HIDE; CreationFlags with
// CREATE_NO_WINDOW prevents the console subsystem from allocating a window at
// all — the reliable combination for console children of a GUI parent.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindowFlag,
	}
}
