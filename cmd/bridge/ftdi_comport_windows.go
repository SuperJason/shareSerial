//go:build windows

package main

import (
	"strings"

	"go.bug.st/serial/enumerator"
)

// ftdiComPortMap maps an upper-cased FTDI serial number to the COM port
// Windows assigned to its VCP interface.
//
// The serial enumerator walks the Ports class and parses FTDIBUS instance IDs
// (FTDIBUS\VID_0403+PID_xxxx+<serial>\0000), so the mapping exists even while
// the port is held open by hub4com — unlike a D2XX enumeration, which
// degrades to placeholder entries in exactly that situation.
//
// The <serial> in the instance ID carries an interface-letter suffix beyond
// the D2XX serial number: an FT230X with serial DK0MAQPC enumerates as
// DK0MAQPCA ('A' = interface A; multi-interface parts append A/B). D2XX
// serials are at most 8 characters, so a 9-or-longer instance serial always
// ends in that suffix. FT_OpenEx(FT_OPEN_BY_SERIAL_NUMBER) expects the bare
// serial, so it is stripped here - otherwise the same board appears under two
// keys and the COM-map-sourced entry can never be opened.
func ftdiComPortMap() map[string]string {
	out := make(map[string]string)
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return out
	}
	for _, d := range details {
		if d == nil || !d.IsUSB {
			continue
		}
		vid := strings.ToUpper(strings.TrimSpace(d.VID))
		serial := strings.TrimSpace(d.SerialNumber)
		if vid != "0403" || serial == "" {
			continue
		}
		serial = strings.ToUpper(serial)
		if len(serial) > 8 {
			serial = serial[:len(serial)-1]
		}
		if serial == "" {
			continue
		}
		out[serial] = strings.TrimSpace(d.Name)
	}
	return out
}
