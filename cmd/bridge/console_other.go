//go:build !windows

package main

// On other platforms the binary keeps the console it was started with; there is
// nothing to allocate and no dialog to fall back to.
func attachConsole(allowAlloc bool) bool { return true }

func messageBox(title, text string) {}