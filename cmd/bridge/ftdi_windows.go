//go:build windows

package main

import (
	"fmt"

	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

// ftd2xxDevice adapts the D2XX handle to the narrow ftdiDevice seam the
// manager uses, keeping D2XX types out of the platform-neutral code.
type ftd2xxDevice struct {
	dev ftd2xx.Device
}

func (d *ftd2xxDevice) SetCBUS(pin int, high bool) error {
	if err := d.dev.SetBitMode(cbusMask(pin, true, high), ftd2xx.BitModeCBUSBitBang); err != nil {
		return fmt.Errorf("CBUS bit-bang rejected on pin %d (are the CBUS pins programmed as GPIO in the EEPROM?): %w", pin, err)
	}
	return nil
}

func (d *ftd2xxDevice) Close() error { return d.dev.Close() }

// openFTDIDevice opens the device by serial number.
//
// It deliberately does not enter bit-bang mode or drive anything: the caller
// drives the pin as its first action, so the device is never left in a state
// the caller did not ask for.
func openFTDIDevice(serial string) (ftdiDevice, error) {
	dev, err := ftd2xx.Open(serial)
	if err != nil {
		return nil, err
	}
	return &ftd2xxDevice{dev: dev}, nil
}
