//go:build !windows

package ftd2xx

// Every entry point reports ErrUnsupported off Windows. The D2XX API is a
// Windows DLL, and the CBUS/VBUS control it exists for is only used on the
// Windows test board.

// List reports that D2XX is unavailable.
func List() ([]Info, error) { return nil, ErrUnsupported }

// LibraryVersion reports that D2XX is unavailable.
func LibraryVersion() (uint32, error) { return 0, ErrUnsupported }

// Open reports that D2XX is unavailable.
func Open(serial string) (Device, error) { return nil, ErrUnsupported }

// DecodeVersion renders a packed D2XX version DWORD as major.minor.build.
func DecodeVersion(v uint32) string { return "unsupported" }
