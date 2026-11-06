//go:build !windows

package main

// Non-Windows builds run in a normal terminal: nothing to hide, and no message
// box to fall back to.
func hideOwnConsole() bool { return false }

func messageBox(title, text string) {}
