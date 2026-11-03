// Package ftd2xx is a thin wrapper over FTDI's D2XX driver (ftd2xx.dll).
//
// It exists because CBUS pin control - required for the VBUS plug/unplug
// simulation - is only reachable through D2XX. FTDI AN_373 states that all
// bit-bang modes require the D2XX driver and cannot be used via VCP, while
// noting that "standard reads/writes to the UART pins using the D2XX driver
// can be combined with CBUS bit-bang". D2XX is therefore the only interface
// that exposes the UART and the CBUS pins at the same time.
//
// The package is deliberately low level: it maps the DLL's entry points and
// status codes, and does no retrying, reconnecting or policy. Higher-level
// behaviour lives in the caller.
package ftd2xx

import (
	"errors"
	"fmt"
)

// ErrUnsupported is returned by every entry point on platforms other than
// Windows, where ftd2xx.dll does not exist.
var ErrUnsupported = errors.New("ftd2xx: only supported on Windows")

// Status is an FT_STATUS return code.
//
// Note there is deliberately no "timeout" status: FT_Read signals a read
// timeout by returning OK with fewer bytes than requested, so a zero-byte
// read is a normal no-data condition rather than an error.
type Status uint32

// FT_STATUS values, in the order defined by ftd2xx.h.
const (
	OK                      Status = 0
	InvalidHandle           Status = 1
	DeviceNotFound          Status = 2
	DeviceNotOpened         Status = 3
	IOError                 Status = 4
	InsufficientResources   Status = 5
	InvalidParameter        Status = 6
	InvalidBaudRate         Status = 7
	DeviceNotOpenedForErase Status = 8
	DeviceNotOpenedForWrite Status = 9
	FailedToWriteDevice     Status = 10
	EEPROMReadFailed        Status = 11
	EEPROMWriteFailed       Status = 12
	EEPROMEraseFailed       Status = 13
	EEPROMNotPresent        Status = 14
	EEPROMNotProgrammed     Status = 15
	InvalidArgs             Status = 16
	NotSupported            Status = 17
	OtherError              Status = 18
	DeviceListNotReady      Status = 19
)

var statusText = map[Status]string{
	OK:                      "FT_OK",
	InvalidHandle:           "FT_INVALID_HANDLE",
	DeviceNotFound:          "FT_DEVICE_NOT_FOUND",
	DeviceNotOpened:         "FT_DEVICE_NOT_OPENED",
	IOError:                 "FT_IO_ERROR",
	InsufficientResources:   "FT_INSUFFICIENT_RESOURCES",
	InvalidParameter:        "FT_INVALID_PARAMETER",
	InvalidBaudRate:         "FT_INVALID_BAUD_RATE",
	DeviceNotOpenedForErase: "FT_DEVICE_NOT_OPENED_FOR_ERASE",
	DeviceNotOpenedForWrite: "FT_DEVICE_NOT_OPENED_FOR_WRITE",
	FailedToWriteDevice:     "FT_FAILED_TO_WRITE_DEVICE",
	EEPROMReadFailed:        "FT_EEPROM_READ_FAILED",
	EEPROMWriteFailed:       "FT_EEPROM_WRITE_FAILED",
	EEPROMEraseFailed:       "FT_EEPROM_ERASE_FAILED",
	EEPROMNotPresent:        "FT_EEPROM_NOT_PRESENT",
	EEPROMNotProgrammed:     "FT_EEPROM_NOT_PROGRAMMED",
	InvalidArgs:             "FT_INVALID_ARGS",
	NotSupported:            "FT_NOT_SUPPORTED",
	OtherError:              "FT_OTHER_ERROR",
	DeviceListNotReady:      "FT_DEVICE_LIST_NOT_READY",
}

func (s Status) String() string {
	if t, ok := statusText[s]; ok {
		return t
	}
	return fmt.Sprintf("FT_STATUS(%d)", uint32(s))
}

// Error implements error so callers can return a status directly.
func (s Status) Error() string {
	if s == OK {
		return "ftd2xx: ok"
	}
	return "ftd2xx: " + s.String()
}

// Bit-bang modes for Device.SetBitMode's second argument.
const (
	BitModeReset        = 0x00
	BitModeAsyncBitBang = 0x01
	BitModeMPSSE        = 0x02
	BitModeSyncBitBang  = 0x04
	BitModeMCUHost      = 0x08
	BitModeFastSerial   = 0x10
	// BitModeCBUSBitBang drives CBUS0-CBUS3 as a 4-bit port. Supported on the
	// FT230X (FT-X series) and FT232R/FT232H; the CBUS pins must first be set
	// to "GPIO" in the device EEPROM with FT_PROG, otherwise it is rejected.
	BitModeCBUSBitBang = 0x20
	BitModeSyncFIFO    = 0x40
)

// Line coding values for SetLineCoding.
const (
	Bits8      = 8
	StopBits1  = 0
	ParityNone = 0
)

// Purge masks for Device.Purge.
const (
	PurgeRX = 1
	PurgeTX = 2
)

// Device type codes reported by FT_GetDeviceInfoList.
const (
	DeviceTypeBM      = 0
	DeviceTypeAM      = 1
	DeviceType100AX   = 2
	DeviceTypeUnknown = 3
	DeviceType2232C   = 4
	DeviceType232R    = 5
	DeviceType2232H   = 6
	DeviceType4232H   = 7
	DeviceType232H    = 8
	DeviceTypeXSeries = 9 // FT230X, FT231X, FT234XD
)

var deviceTypeNames = map[uint32]string{
	DeviceTypeBM:      "FT8U100AX/BM",
	DeviceTypeAM:      "FT8U232AM",
	DeviceType100AX:   "FT8U100AX",
	DeviceTypeUnknown: "unknown",
	DeviceType2232C:   "FT2232C/D",
	DeviceType232R:    "FT232R",
	DeviceType2232H:   "FT2232H",
	DeviceType4232H:   "FT4232H",
	DeviceType232H:    "FT232H",
	DeviceTypeXSeries: "FT-X series",
}

// DeviceTypeName maps a device type code to a readable name.
func DeviceTypeName(t uint32) string {
	if n, ok := deviceTypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type(%d)", t)
}

// Info describes one enumerated device.
type Info struct {
	Index       int
	Type        uint32
	ID          uint32
	LocationID  uint32
	Serial      string
	Description string
}

// TypeName returns the readable device type name.
func (i Info) TypeName() string { return DeviceTypeName(i.Type) }

// Device is an open D2XX handle.
//
// Read returns (0, nil) when the read timeout expires with no data available;
// that is a normal condition, not an error. Only transport failures such as
// IOError are reported as errors.
type Device interface {
	// Read reads up to len(p) bytes, blocking until at least one byte arrives
	// or the read timeout set via SetTimeouts expires.
	Read(p []byte) (int, error)
	Write(p []byte) error

	SetBaud(baud int) error
	SetLineCoding(bits, stop, parity byte) error
	SetFlowControlNone() error
	// SetTimeouts sets the read and write timeouts in milliseconds.
	SetTimeouts(readMs, writeMs uint32) error
	// SetLatencyTimer sets the USB latency timer in milliseconds (1-255).
	SetLatencyTimer(ms byte) error
	Purge(rx, tx bool) error

	// SetBitMode writes mask to the device in the given bit-bang mode. A mode
	// of BitModeReset (0) returns the device to normal UART operation.
	SetBitMode(mask, mode byte) error
	// BitMode returns the instantaneous pin values. In CBUS bit-bang mode the
	// low nibble holds CBUS0-CBUS3.
	BitMode() (byte, error)

	// ReadEEPROM reads one 16-bit word from the device EEPROM.
	ReadEEPROM(word uint32) (uint16, error)

	// DriverVersion returns the installed D2XX driver version as a packed
	// DWORD; decode it with DecodeVersion.
	DriverVersion() (uint32, error)

	// ComPortNumber returns the COM port Windows assigned to this device, or
	// (-1, nil) when the device has no VCP port bound.
	ComPortNumber() (int, error)

	Info() Info
	Close() error
}
