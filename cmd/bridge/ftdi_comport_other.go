//go:build !windows

package main

// ftdiComPortMap is Windows-only: the D2XX tool boards and their VCP ports
// only exist there.
func ftdiComPortMap() map[string]string { return nil }
