//go:build !windows

package main

import "time"

// startMenuOpenHook: no menu-open event is available on this platform, so
// fall back to a slow refresh timer feeding the same callback.
func startMenuOpenHook(cb func()) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			cb()
		}
	}()
}
