//go:build windows

package ftd2xx

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Load via NewLazySystemDLL rather than NewLazyDLL so the search is confined to
// System32. A bare DLL name resolved through the normal search order could be
// satisfied by a file planted in the working directory.
var dll = windows.NewLazySystemDLL("ftd2xx.dll")

var (
	procOpenEx            = dll.NewProc("FT_OpenEx")
	procClose             = dll.NewProc("FT_Close")
	procSetBaudRate       = dll.NewProc("FT_SetBaudRate")
	procSetDataChars      = dll.NewProc("FT_SetDataCharacteristics")
	procSetFlowControl    = dll.NewProc("FT_SetFlowControl")
	procSetTimeouts       = dll.NewProc("FT_SetTimeouts")
	procSetLatencyTimer   = dll.NewProc("FT_SetLatencyTimer")
	procPurge             = dll.NewProc("FT_Purge")
	procRead              = dll.NewProc("FT_Read")
	procWrite             = dll.NewProc("FT_Write")
	procSetBitMode        = dll.NewProc("FT_SetBitMode")
	procGetBitMode        = dll.NewProc("FT_GetBitMode")
	procReadEE            = dll.NewProc("FT_ReadEE")
	procCreateDeviceList  = dll.NewProc("FT_CreateDeviceInfoList")
	procGetDeviceInfoList = dll.NewProc("FT_GetDeviceInfoList")
	procGetLibraryVersion = dll.NewProc("FT_GetLibraryVersion")
	procGetDriverVersion  = dll.NewProc("FT_GetDriverVersion")
	procGetComPortNumber  = dll.NewProc("FT_GetComPortNumber")
)

// FT_OpenEx flags.
const openBySerialNumber = 1

// deviceListInfoNode mirrors FT_DEVICE_LIST_INFO_NODE from ftd2xx.h. The C
// struct is four ULONGs, then char[16] and char[64], then the handle pointer;
// the layout below is identical with no implicit padding on amd64.
type deviceListInfoNode struct {
	Flags        uint32
	Type         uint32
	ID           uint32
	LocID        uint32
	SerialNumber [16]byte
	Description  [64]byte
	Handle       uintptr
}

// callStatus invokes a DLL procedure and converts a non-OK FT_STATUS into an
// error. The status is returned as the raw uintptr from the call, so it must
// always be checked through this helper rather than ignored.
func callStatus(proc *windows.LazyProc, args ...uintptr) error {
	r, _, _ := proc.Call(args...)
	if st := Status(r); st != OK {
		return st
	}
	return nil
}

// cString converts a NUL-terminated fixed-size char array to a Go string.
func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// List enumerates the attached FTDI devices.
func List() ([]Info, error) {
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("ftd2xx: loading ftd2xx.dll: %w", err)
	}

	var num uint32
	if err := callStatus(procCreateDeviceList, uintptr(unsafe.Pointer(&num))); err != nil {
		return nil, fmt.Errorf("ftd2xx: FT_CreateDeviceInfoList: %w", err)
	}
	if num == 0 {
		return nil, nil
	}

	nodes := make([]deviceListInfoNode, num)
	count := num
	if err := callStatus(procGetDeviceInfoList,
		uintptr(unsafe.Pointer(&nodes[0])),
		uintptr(unsafe.Pointer(&count)),
	); err != nil {
		return nil, fmt.Errorf("ftd2xx: FT_GetDeviceInfoList: %w", err)
	}
	runtime.KeepAlive(nodes)

	out := make([]Info, 0, count)
	for i := 0; i < int(count); i++ {
		out = append(out, Info{
			Index:       i,
			Type:        nodes[i].Type,
			ID:          nodes[i].ID,
			LocationID:  nodes[i].LocID,
			Serial:      cString(nodes[i].SerialNumber[:]),
			Description: cString(nodes[i].Description[:]),
		})
	}
	return out, nil
}

// LibraryVersion returns the D2XX DLL version as a packed DWORD.
func LibraryVersion() (uint32, error) {
	if err := dll.Load(); err != nil {
		return 0, fmt.Errorf("ftd2xx: loading ftd2xx.dll: %w", err)
	}
	var v uint32
	if err := callStatus(procGetLibraryVersion, uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, fmt.Errorf("ftd2xx: FT_GetLibraryVersion: %w", err)
	}
	return v, nil
}

// DecodeVersion renders a packed D2XX version DWORD as major.minor.build.
func DecodeVersion(v uint32) string {
	return fmt.Sprintf("%d.%d.%d (0x%08X)", v>>16, (v>>8)&0xFF, v&0xFF, v)
}

type winDevice struct {
	handle uintptr
	info   Info
}

// Open opens the device with the given serial number.
//
// Opening by serial number rather than by COM port means the handle survives
// Windows renumbering the port.
func Open(serial string) (Device, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return nil, fmt.Errorf("ftd2xx: serial number is required")
	}
	if err := dll.Load(); err != nil {
		return nil, fmt.Errorf("ftd2xx: loading ftd2xx.dll: %w", err)
	}

	name := append([]byte(serial), 0)
	var handle uintptr
	err := callStatus(procOpenEx,
		uintptr(unsafe.Pointer(&name[0])),
		openBySerialNumber,
		uintptr(unsafe.Pointer(&handle)),
	)
	runtime.KeepAlive(name)
	if err != nil {
		return nil, fmt.Errorf("ftd2xx: FT_OpenEx(%q): %w", serial, err)
	}

	dev := &winDevice{handle: handle, info: Info{Serial: serial}}
	// Best-effort: fill in the descriptive fields for Status() reporting.
	if all, listErr := List(); listErr == nil {
		for _, i := range all {
			if i.Serial == serial {
				dev.info = i
				break
			}
		}
	}
	return dev, nil
}

func (d *winDevice) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	err := callStatus(procRead,
		d.handle,
		uintptr(unsafe.Pointer(&p[0])),
		uintptr(len(p)),
		uintptr(unsafe.Pointer(&n)),
	)
	runtime.KeepAlive(p)
	if err != nil {
		return 0, err
	}
	// A short read means the timeout expired before len(p) bytes arrived. That
	// includes n == 0, which is the ordinary "no data yet" case and must not be
	// reported as an error.
	return int(n), nil
}

func (d *winDevice) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	written := 0
	for written < len(p) {
		var n uint32
		err := callStatus(procWrite,
			d.handle,
			uintptr(unsafe.Pointer(&p[written])),
			uintptr(len(p)-written),
			uintptr(unsafe.Pointer(&n)),
		)
		runtime.KeepAlive(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("ftd2xx: FT_Write made no progress at %d/%d bytes", written, len(p))
		}
		written += int(n)
	}
	return nil
}

func (d *winDevice) SetBaud(baud int) error {
	if err := callStatus(procSetBaudRate, d.handle, uintptr(uint32(baud))); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetBaudRate(%d): %w", baud, err)
	}
	return nil
}

func (d *winDevice) SetLineCoding(bits, stop, parity byte) error {
	if err := callStatus(procSetDataChars, d.handle, uintptr(bits), uintptr(stop), uintptr(parity)); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetDataCharacteristics: %w", err)
	}
	return nil
}

func (d *winDevice) SetFlowControlNone() error {
	if err := callStatus(procSetFlowControl, d.handle, 0, 0, 0); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetFlowControl(FT_FLOW_NONE): %w", err)
	}
	return nil
}

func (d *winDevice) SetTimeouts(readMs, writeMs uint32) error {
	if err := callStatus(procSetTimeouts, d.handle, uintptr(readMs), uintptr(writeMs)); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetTimeouts: %w", err)
	}
	return nil
}

func (d *winDevice) SetLatencyTimer(ms byte) error {
	if ms == 0 {
		ms = 1
	}
	if err := callStatus(procSetLatencyTimer, d.handle, uintptr(ms)); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetLatencyTimer(%d): %w", ms, err)
	}
	return nil
}

func (d *winDevice) Purge(rx, tx bool) error {
	var mask uintptr
	if rx {
		mask |= PurgeRX
	}
	if tx {
		mask |= PurgeTX
	}
	if mask == 0 {
		return nil
	}
	if err := callStatus(procPurge, d.handle, mask); err != nil {
		return fmt.Errorf("ftd2xx: FT_Purge: %w", err)
	}
	return nil
}

func (d *winDevice) SetBitMode(mask, mode byte) error {
	if err := callStatus(procSetBitMode, d.handle, uintptr(mask), uintptr(mode)); err != nil {
		return fmt.Errorf("ftd2xx: FT_SetBitMode(mask=0x%02X mode=0x%02X): %w", mask, mode, err)
	}
	return nil
}

func (d *winDevice) BitMode() (byte, error) {
	var v byte
	if err := callStatus(procGetBitMode, d.handle, uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, fmt.Errorf("ftd2xx: FT_GetBitMode: %w", err)
	}
	runtime.KeepAlive(&v)
	return v, nil
}

func (d *winDevice) ReadEEPROM(word uint32) (uint16, error) {
	var v uint16
	if err := callStatus(procReadEE, d.handle, uintptr(word), uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, fmt.Errorf("ftd2xx: FT_ReadEE(0x%02X): %w", word, err)
	}
	runtime.KeepAlive(&v)
	return v, nil
}

func (d *winDevice) DriverVersion() (uint32, error) {
	var v uint32
	if err := callStatus(procGetDriverVersion, d.handle, uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, fmt.Errorf("ftd2xx: FT_GetDriverVersion: %w", err)
	}
	runtime.KeepAlive(&v)
	return v, nil
}

func (d *winDevice) ComPortNumber() (int, error) {
	var n int32
	if err := callStatus(procGetComPortNumber, d.handle, uintptr(unsafe.Pointer(&n))); err != nil {
		return 0, fmt.Errorf("ftd2xx: FT_GetComPortNumber: %w", err)
	}
	runtime.KeepAlive(&n)
	return int(n), nil
}

func (d *winDevice) Info() Info { return d.info }

func (d *winDevice) Close() error {
	if d.handle == 0 {
		return nil
	}
	h := d.handle
	d.handle = 0
	if err := callStatus(procClose, h); err != nil {
		return fmt.Errorf("ftd2xx: FT_Close: %w", err)
	}
	return nil
}
