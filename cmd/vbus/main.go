// Command wisusb-vbus drives the CBUS pin of an FTDI tool board to switch a
// DUT's VBUS on and off, as a standalone single-binary tool.
//
// It shares the D2XX layer with the bridge (internal/ftd2xx) but has no
// dependency on the bridge, hub4com or any config file.
//
// The level is latched by the device: FT_Close does not reset the CBUS state
// (verified on hardware - reopen and FT_GetBitMode without any SetBitMode
// reads back the driven level), so 'on' and 'off' are one-shot commands that
// set the level and exit. This is what distinguishes CBUS GPIO from DTR/RTS
// modem lines. The level persists until another command changes it or the
// device re-enumerates (unplug, driver reload), which returns the pin to its
// EEPROM default.
//
// Usage:
//
//	wisusb-vbus list
//	wisusb-vbus on  [-serial DK0MAQPC] [-pin 0] [-active-low=true] [-wait 30s]
//	wisusb-vbus off [-serial DK0MAQPC] [-pin 0] [-active-low=true] [-wait 30s]
//
// With several boards attached, -serial is required; list shows the serials.
// When the board's COM port is held by another program (the bridge's hub4com,
// a serial terminal, ...), on/off wait for it to be released instead of
// failing immediately; bound the wait with -wait (default 30s, 0 = no wait).
//
// For a board the running bridge manages, prefer the bridge's own control
// (tray menu / Web UI / POST /api/vbus): it coordinates the port with its
// hub4com supervision, while this tool would have to wait out each reopen.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/wiseasy/tcp_serial/internal/ftd2xx"
)

// cbusBitBang is FT_SetBitMode mode 0x20: CBUS0-CBUS3 as a 4-bit port. The
// mask's upper nibble is pin direction (1 = output), the lower nibble the
// output level; the CBUS pins must be programmed as GPIO in the EEPROM.
const cbusBitBang = 0x20

// ftdiSerialMax is the longest serial D2XX reports; a longer one carries the
// Windows FTDIBUS interface-letter suffix that FT_OpenEx does not accept.
const ftdiSerialMax = 8

// openRetryInterval paces the wait for a held COM port to be released.
const openRetryInterval = 250 * time.Millisecond

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "list":
		err = cmdList()
	case "on":
		err = cmdSet(true)
	case "off":
		err = cmdSet(false)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`wisusb-vbus - control a DUT's VBUS through an FTDI tool board's CBUS pin

usage:
  wisusb-vbus list                          show attached FTDI boards
  wisusb-vbus on|off [flags]                set VBUS on/off (level is held by
                                           the device until changed or the
                                           board is unplugged)

flags (on/off):
  -serial string   board serial number (required when several boards are attached)
  -pin int         CBUS pin driving VBUS, 0-3 (default 0)
  -active-low      true = pin low turns VBUS on (default true)
  -wait duration   how long to wait when the COM port is held by another
                   program (default 30s; 0 = fail immediately)

If the bridge is running and managing the board, use its tray menu / Web UI /
POST /api/vbus instead: it frees and restores the port around the toggle
without waiting.
`)
}

// flags holds the shared per-command options.
type flags struct {
	serial    string
	pin       int
	activeLow bool
	wait      time.Duration
}

func parseFlags(args []string) *flags {
	fs := flag.NewFlagSet("", flag.ExitOnError)
	f := &flags{}
	fs.StringVar(&f.serial, "serial", "", "board serial number")
	fs.IntVar(&f.pin, "pin", 0, "CBUS pin driving VBUS (0-3)")
	fs.BoolVar(&f.activeLow, "active-low", true, "true = pin low turns VBUS on")
	fs.DurationVar(&f.wait, "wait", 30*time.Second, "how long to wait for a held COM port (0 = fail immediately)")
	_ = fs.Parse(args)
	return f
}

// normalizeSerial canonicalises a serial for FT_OpenEx: trimmed, upper-cased,
// and with the Windows FTDIBUS interface-letter suffix removed (an FT230X
// with serial DK0MAQPC enumerates as DK0MAQPCA).
func normalizeSerial(serial string) string {
	s := strings.ToUpper(strings.TrimSpace(serial))
	if len(s) > ftdiSerialMax {
		s = s[:len(s)-1]
	}
	return s
}

// cbusMask builds a bit-bang mask for one pin: upper nibble direction (1 =
// output), lower nibble level, only the target pin's bits set so the other
// CBUS pins are never disturbed.
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

// vbusLevel maps a desired VBUS state to the CBUS level that produces it.
func vbusLevel(activeLow, on bool) bool {
	return on != activeLow
}

// boardCandidate is a board the enumeration can name.
type boardCandidate struct {
	info   ftd2xx.Info
	serial string
}

// enumerate lists the boards D2XX can currently name. A held port degrades to
// a placeholder entry (blank serial) which is reported separately: the board
// is present, but only its holder can free it.
func enumerate() (boards []boardCandidate, held int, err error) {
	infos, err := ftd2xx.List()
	if err != nil {
		return nil, 0, err
	}
	for _, i := range infos {
		serial := normalizeSerial(i.Serial)
		if serial == "" {
			held++
			continue
		}
		boards = append(boards, boardCandidate{info: i, serial: serial})
	}
	return boards, held, nil
}

// pickBoard selects the board to act on: an explicit serial wins, then the
// FT-X series part (what the tool boards carry), then a lone device. With
// several candidates and no serial it refuses rather than guesses.
func pickBoard(boards []boardCandidate, serial string) (boardCandidate, error) {
	if serial != "" {
		key := normalizeSerial(serial)
		for _, b := range boards {
			if b.serial == key {
				return b, nil
			}
		}
		return boardCandidate{}, fmt.Errorf("no FTDI board with serial %q (see: wisusb-vbus list)", serial)
	}
	for _, b := range boards {
		if b.info.Type == ftd2xx.DeviceTypeXSeries {
			return b, nil
		}
	}
	if len(boards) == 1 {
		return boards[0], nil
	}
	if len(boards) == 0 {
		return boardCandidate{}, fmt.Errorf("no FTDI board detected; check the USB connection")
	}
	return boardCandidate{}, fmt.Errorf("several FTDI boards attached; pass -serial (see: wisusb-vbus list)")
}

// resolveBoard picks the board, adding a held-port hint when the named serial
// matches nothing and some ports are held.
func resolveBoard(f *flags) (boardCandidate, error) {
	boards, held, err := enumerate()
	if err != nil {
		return boardCandidate{}, err
	}
	board, err := pickBoard(boards, f.serial)
	if err == nil {
		return board, nil
	}
	if f.serial != "" && held > 0 {
		return boardCandidate{}, fmt.Errorf("%w\nThe named board may be among %d device(s) whose COM port is held by another\nprogram (the bridge's hub4com, a serial terminal); its serial is not readable\nwhile held. Free the port, or rely on -wait", err, held)
	}
	return boardCandidate{}, err
}

// openBoard resolves the board and opens it over D2XX, waiting for a held COM
// port to be released when f.wait > 0. Resolution is retried as well: a held
// port hides the serial, so the board often only becomes resolvable after
// the holder lets go.
func openBoard(ctx context.Context, f *flags) (ftd2xx.Device, boardCandidate, error) {
	board, err := resolveBoard(f)
	if err != nil {
		_, held, _ := enumerate()
		if held == 0 || f.wait == 0 {
			return nil, boardCandidate{}, err
		}
	}

	deadline := time.Now().Add(f.wait)
	reported := false
	// Name for messages: the requested serial when the board cannot (yet) be
	// resolved, since a held port hides exactly the serial the caller named.
	name := normalizeSerial(f.serial)
	for {
		if err == nil {
			name = board.serial
			dev, openErr := ftd2xx.Open(board.serial)
			if openErr == nil {
				return dev, board, nil
			}
			if f.wait == 0 || !strings.Contains(openErr.Error(), "FT_DEVICE_NOT_OPENED") {
				return nil, board, openErr
			}
		}
		if !reported {
			fmt.Printf("COM port of %s is held by another program; waiting up to %s...\n", name, f.wait)
			reported = true
		}

		select {
		case <-ctx.Done():
			return nil, board, fmt.Errorf("interrupted while waiting for the COM port of %s", name)
		case <-time.After(openRetryInterval):
		}
		if time.Now().After(deadline) {
			return nil, board, fmt.Errorf("the COM port of %s stayed held for %s; free it or raise -wait", name, f.wait)
		}
		if board, err = resolveBoard(f); err != nil && f.wait == 0 {
			return nil, board, err
		}
	}
}

func cmdList() error {
	boards, held, err := enumerate()
	if err != nil {
		return err
	}
	if len(boards) == 0 && held == 0 {
		fmt.Println("no FTDI boards detected")
		return nil
	}
	for _, b := range boards {
		fmt.Printf("  %-14s serial=%s %q\n", b.info.TypeName(), b.serial, b.info.Description)
	}
	for i := 0; i < held; i++ {
		fmt.Println("  (port held by another program - serial not readable)")
	}
	return nil
}

// cmdSet drives the pin to the requested level and exits: the device holds
// the state after FT_Close, so no process needs to stay running.
func cmdSet(on bool) error {
	f := parseFlags(os.Args[2:])
	if f.pin < 0 || f.pin > 3 {
		return fmt.Errorf("-pin must be 0-3, got %d", f.pin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), f.wait+5*time.Second)
	defer cancel()

	dev, board, err := openBoard(ctx, f)
	if err != nil {
		return err
	}
	defer dev.Close()

	high := vbusLevel(f.activeLow, on)
	mask := cbusMask(f.pin, true, high)
	if err := dev.SetBitMode(mask, cbusBitBang); err != nil {
		return fmt.Errorf("driving CBUS%d failed (are the CBUS pins programmed as GPIO in the EEPROM?): %w", f.pin, err)
	}

	// Read back and verify the pin followed, so a silent failure (wrong pin,
	// different board wiring) is caught here rather than in the test run.
	if got, err := dev.BitMode(); err == nil {
		want := byte(0)
		if high {
			want = 1 << uint(f.pin)
		}
		if got&(1<<uint(f.pin)) != want {
			return fmt.Errorf("CBUS%d did not follow: wrote 0x%02X, read 0x%02X", f.pin, mask, got)
		}
	}

	fmt.Printf("%s CBUS%d -> VBUS %s (held by the device until changed or the board is unplugged)\n",
		board.serial, f.pin, onOff(on))
	return nil
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
