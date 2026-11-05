//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openWindow falls back to the platform browser on non-Windows hosts, where
// the bridge still runs headless. It returns immediately and reports whether a
// browser could be launched; the caller then blocks on signals instead.
func openWindow(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	return true
}
