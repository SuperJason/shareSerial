// Command ftdi_probe verifies that an FTDI device can be driven over D2XX and
// that its CBUS pins respond to CBUS bit-bang mode.
//
// It exists to de-risk the VBUS plug/unplug feature before any of it is wired
// into cmd/bridge. Three things it answers that cannot be determined by reading
// documentation:
//
//  1. Whether D2XX can open the device while Windows still has a VCP COM port
//     enumerated for it, and whether that COM port must be closed first.
//  2. Whether the CBUS pin is really programmed as "GPIO" in the EEPROM, as
//     opposed to a fixed function such as PWREN# which software cannot drive.
//  3. What the pin's resting level is, and whether driving it changes what is
//     read back.
//
// It cannot determine VBUS polarity - that requires a meter on the VBUS rail.
//
// Stop hub4com (or anything else holding the COM port) before running this.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

func main() {
	serial := flag.String("serial", "", "device serial number; omit to list devices and auto-select a single one")
	pin := flag.Int("pin", 0, "CBUS pin to exercise, 0-3")
	cycles := flag.Int("cycles", 0, "after the basic checks, toggle the pin this many times")
	dwell := flag.Duration("dwell", 500*time.Millisecond, "delay between toggle states")
	skipEEPROM := flag.Bool("no-eeprom", false, "skip the EEPROM dump")
	end := flag.String("end", "input", "level to leave the pin at on exit: input, low, or high")
	flag.Parse()

	endState, err := parseEndState(*end)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	if err := run(*serial, *pin, *cycles, *dwell, !*skipEEPROM, endState); err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
}

// endState is the pin condition the probe leaves behind.
type endState int

const (
	endInput endState = iota // tri-state; internal pull-up makes this read high
	endLow
	endHigh
)

func parseEndState(s string) (endState, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "input", "tri", "tristate":
		return endInput, nil
	case "low", "0":
		return endLow, nil
	case "high", "1":
		return endHigh, nil
	}
	return 0, fmt.Errorf("-end must be input, low, or high; got %q", s)
}

func (e endState) apply(dev ftd2xx.Device, pin int) error {
	switch e {
	case endLow:
		return dev.SetBitMode(cbusMask(pin, true, false), ftd2xx.BitModeCBUSBitBang)
	case endHigh:
		return dev.SetBitMode(cbusMask(pin, true, true), ftd2xx.BitModeCBUSBitBang)
	default:
		return dev.SetBitMode(0x00, ftd2xx.BitModeCBUSBitBang)
	}
}

func (e endState) String() string {
	switch e {
	case endLow:
		return "driven low"
	case endHigh:
		return "driven high"
	default:
		return "input (tri-stated, pulled high internally)"
	}
}

func run(serial string, pin, cycles int, dwell time.Duration, dumpEEPROM bool, end endState) error {
	if pin < 0 || pin > 3 {
		return fmt.Errorf("-pin must be 0-3, got %d", pin)
	}

	fmt.Println("=== D2XX library ===")
	if v, err := ftd2xx.LibraryVersion(); err != nil {
		return fmt.Errorf("ftd2xx.dll is not usable: %w", err)
	} else {
		fmt.Printf("DLL version : %s\n", ftd2xx.DecodeVersion(v))
	}

	devices, err := ftd2xx.List()
	if err != nil {
		return err
	}
	fmt.Printf("\n=== Devices (%d) ===\n", len(devices))
	if len(devices) == 0 {
		return fmt.Errorf("no FTDI devices found; check the USB connection and that the FTDI driver is installed")
	}
	for _, d := range devices {
		fmt.Printf("  [%d] %-14s serial=%-12s desc=%q locID=0x%08X\n",
			d.Index, d.TypeName(), orDash(d.Serial), d.Description, d.LocationID)
	}

	target, err := pickTarget(devices, serial)
	if err != nil {
		return err
	}

	fmt.Printf("\n=== Opening serial=%s ===\n", target.Serial)
	dev, err := ftd2xx.Open(target.Serial)
	if err != nil {
		fmt.Println("Could not open the device over D2XX.")
		fmt.Println("If hub4com, a terminal, or any other program currently has the")
		fmt.Println("device's COM port open, close it and retry - D2XX and an open")
		fmt.Println("VCP handle cannot coexist.")
		return err
	}
	defer dev.Close()
	fmt.Println("Opened over D2XX.")

	if v, err := dev.DriverVersion(); err == nil {
		fmt.Printf("Driver      : %s\n", ftd2xx.DecodeVersion(v))
	}
	if n, err := dev.ComPortNumber(); err == nil {
		if n < 0 {
			fmt.Println("VCP port    : none bound")
		} else {
			fmt.Printf("VCP port    : COM%d (still enumerated; D2XX holds the device)\n", n)
		}
	}

	if dumpEEPROM {
		fmt.Println("\n=== EEPROM dump ===")
		dumpEE(dev)
	}

	warnSideEffects(pin, end)

	fmt.Printf("\n=== CBUS%d bit-bang test ===\n", pin)
	if err := cbusTest(dev, pin, end); err != nil {
		return err
	}

	if cycles > 0 {
		fmt.Printf("\n=== Toggling CBUS%d %d times (dwell %s) ===\n", pin, cycles, dwell)
		if err := toggleLoop(dev, pin, cycles, dwell, end); err != nil {
			return err
		}
	}

	fmt.Println("\n=== Done ===")
	fmt.Printf("Pin left %s.\n", end)

	fmt.Println("Software can confirm the pin is driven and reads back correctly, but it")
	fmt.Println("cannot see the VBUS rail. Meter that VBUS while running cycles and")
	fmt.Println("note which driven level turns VBUS on, then set ftdi_active_low to match:")
	fmt.Println("  CBUS high -> VBUS on  =>  ftdi_active_low = false")
	fmt.Println("  CBUS low  -> VBUS on  =>  ftdi_active_low = true")
	return nil
}

func pickTarget(devices []ftd2xx.Info, serial string) (ftd2xx.Info, error) {
	if serial != "" {
		for _, d := range devices {
			if strings.EqualFold(d.Serial, serial) {
				return d, nil
			}
		}
		return ftd2xx.Info{}, fmt.Errorf("no device with serial %q", serial)
	}
	if len(devices) == 1 {
		return devices[0], nil
	}
	return ftd2xx.Info{}, fmt.Errorf("multiple devices present; pass -serial to choose one")
}

// dumpEE prints the first 64 EEPROM words. Interpretation is left to the
// operator, who can cross-check against FT_PROG; the FT-X layout differs from
// the FT232R one and decoding it here would add a large amount of guesswork for
// little benefit.
func dumpEE(dev ftd2xx.Device) {
	var unreadable int
	fmt.Println("  word   value")
	for w := uint32(0); w < 64; w++ {
		v, err := dev.ReadEEPROM(w)
		if err != nil {
			unreadable++
			continue
		}
		fmt.Printf("  0x%02X   0x%04X\n", w, v)
	}
	if unreadable == 64 {
		fmt.Println("  (all reads failed - the EEPROM may not be readable on this part)")
	}
	fmt.Println("  Cross-check the CBUS settings against FT_PROG's CBUS pin table.")
}

// cbusMask builds a CBUS bit-bang mask for one pin.
//
// In CBUS bit-bang mode the mask's upper nibble is the direction (1 = output)
// and the lower nibble is the output level, bit N mapping to CBUSN. Only the
// target pin's bits are set, so the remaining CBUS pins stay tri-stated inputs
// and are not disturbed.
func cbusMask(pin int, output, high bool) byte {
	var m byte
	if output {
		m |= 0x10 << uint(pin)
	}
	if high {
		m |= 0x01 << uint(pin)
	}
	return m
}

// warnSideEffects states plainly what the probe is about to do to the board.
func warnSideEffects(pin int, end endState) {
	fmt.Println("\n!! This probe drives a real pin.")
	fmt.Printf("!! CBUS%d is wired to the VBUS switch, so the connected device will\n", pin)
	fmt.Println("!! see a USB disconnect and reconnect during this test.")
	fmt.Printf("!! On exit the pin will be left %s.\n", end)
	fmt.Println("!! Run this only when nothing else is using the board.")
}

func cbusTest(dev ftd2xx.Device, pin int, end endState) error {
	// Enter bit-bang mode with every CBUS pin as an input. This tri-states the
	// target pin instead of driving it, so the existing external state is
	// observed rather than disturbed - the closest achievable approximation of
	// "leave the pin as we found it".
	if err := dev.SetBitMode(0x00, ftd2xx.BitModeCBUSBitBang); err != nil {
		fmt.Println("  FT_SetBitMode(0x20) was rejected.")
		fmt.Println("  This usually means the CBUS pins are not programmed as 'GPIO'")
		fmt.Println("  in the EEPROM. Reprogram with FT_PROG: CBUS0 -> GPIO.")
		return err
	}
	fmt.Println("  CBUS bit-bang mode enabled.")

	resting, err := dev.BitMode()
	if err != nil {
		return err
	}
	fmt.Printf("  resting level (pin as input) : 0x%02X  CBUS0..3 = %04b\n", resting, resting&0x0F)

	for _, high := range []bool{false, true} {
		mask := cbusMask(pin, true, high)
		if err := dev.SetBitMode(mask, ftd2xx.BitModeCBUSBitBang); err != nil {
			return fmt.Errorf("driving CBUS%d %s: %w", pin, levelName(high), err)
		}
		got, err := dev.BitMode()
		if err != nil {
			return err
		}
		want := byte(0)
		if high {
			want = 1 << uint(pin)
		}
		ok := "OK"
		if got&(1<<uint(pin)) != want {
			ok = "MISMATCH - pin did not follow"
		}
		fmt.Printf("  drive CBUS%d %-4s mask=0x%02X -> read 0x%02X  [%s]\n",
			pin, levelName(high), mask, got, ok)
	}

	// Leave the pin in the requested end state rather than holding whatever
	// level the test happened to finish on.
	if err := end.apply(dev, pin); err != nil {
		return err
	}
	fmt.Printf("  pin left %s\n", end)
	return nil
}

func toggleLoop(dev ftd2xx.Device, pin, cycles int, dwell time.Duration, end endState) error {
	for i := 1; i <= cycles; i++ {
		for _, high := range []bool{false, true} {
			if err := dev.SetBitMode(cbusMask(pin, true, high), ftd2xx.BitModeCBUSBitBang); err != nil {
				return fmt.Errorf("cycle %d: %w", i, err)
			}
			fmt.Printf("  cycle %d/%d: CBUS%d %s\n", i, cycles, pin, levelName(high))
			time.Sleep(dwell)
		}
	}
	return end.apply(dev, pin)
}

func levelName(high bool) string {
	if high {
		return "high"
	}
	return "low"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
