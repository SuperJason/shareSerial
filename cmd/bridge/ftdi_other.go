//go:build !windows

package main

import (
	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

// openFTDIDevice reports that D2XX is unavailable. The tool board and its D2XX
// driver are Windows-only, so every other platform takes this path.
func openFTDIDevice(serial string) (ftdiDevice, error) {
	return nil, ftd2xx.ErrUnsupported
}
