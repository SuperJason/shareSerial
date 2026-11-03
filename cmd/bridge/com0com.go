package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// com0comPair is one virtual serial pair exposed by the com0com driver.
type com0comPair struct {
	Name    string `json:"name"`    // CNCA0 / CNCB0 ...
	Port    string `json:"port"`    // COM161 ...
	Peer    string `json:"peer"`    // peer port name (COM162 for CNCA0) or ""
	Present bool   `json:"present"` // true when this port can be opened
}

type com0comInfo struct {
	SetupcPath string        `json:"setupc_path"`
	Pairs      []com0comPair `json:"pairs"`
	Message    string        `json:"message,omitempty"`
}

var com0comDeviceRe = regexp.MustCompile(`(?i)com0com\s*-\s*serial\s+port\s+emulator\s+(\S+)\s*\(COM(\d+)\)`)

// defaultSetupcPath returns the standard com0com install location on this host.
func defaultSetupcPath() string {
	return `C:\Program Files (x86)\com0com\setupc.exe`
}

// listCOMPairs enumerates com0com virtual port pairs via PnP (no elevation
// needed). It reads "com0com - serial port emulator CNCA0 (COM161)" style
// FriendlyNames and pairs them up by the driver's bus parent.
func listCOMPairs() ([]com0comPair, error) {
	ps := `Get-PnpDevice | Where-Object { $_.FriendlyName -match 'com0com - serial port emulator' } | ForEach-Object { $_.FriendlyName }`
	out, err := runPowerShellCapture(ps)
	if err != nil {
		return nil, err
	}
	byPort := map[string]string{} // port -> pair name (CNCA0 etc.)
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		m := com0comDeviceRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		port := "COM" + m[2]
		byPort[port] = name
		names = append(names, name)
	}
	sort.Strings(names)

	// Pair by driver pair name: CNCAx <-> CNCBx (peer is NOT port+1, the
	// high-block renames broke that assumption).
	nameToPort := map[string]string{}
	for p, n := range byPort {
		nameToPort[n] = p
	}
	pairs := make([]com0comPair, 0, len(names))
	for _, n := range names {
		port := nameToPort[n]
		peer := ""
		if strings.HasPrefix(n, "CNCA") {
			peer = nameToPort["CNCB"+n[4:]]
		} else if strings.HasPrefix(n, "CNCB") {
			peer = nameToPort["CNCA"+n[4:]]
		}
		pairs = append(pairs, com0comPair{Name: n, Port: port, Peer: peer, Present: true})
	}
	return pairs, nil
}

// runSetupcElevated runs setupc.exe via an elevated PowerShell (UAC prompt).
// The user must click Yes on the UAC dialog; returns an error if the call
// cannot be launched.
func runSetupcElevated(args ...string) error {
	setupc := defaultSetupcPath()
	argList := strings.Join(args, " ")
	cmdLine := fmt.Sprintf("& '%s' %s", strings.ReplaceAll(setupc, "'", "''"), argList)
	ps := fmt.Sprintf("Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-Command',\"%s\"", cmdLine)
	_, err := runPowerShellCapture(ps)
	if err != nil {
		return fmt.Errorf("elevated setupc launch failed: %w", err)
	}
	return nil
}

// com0comRename renames a com0com pair end (e.g. CNCA0) to a new port name.
// Requires elevation (UAC prompt).
func com0comRename(pairName, newPort string) error {
	if pairName == "" || newPort == "" {
		return fmt.Errorf("pair name and port required")
	}
	return runSetupcElevated("change", pairName, "PortName="+newPort)
}

// com0comCreate creates a new pair with the two given port names.
// Requires elevation (UAC prompt).
func com0comCreate(portA, portB string) error {
	if portA == "" || portB == "" {
		return fmt.Errorf("both port names required")
	}
	return runSetupcElevated("install", "PortName="+portA, "PortName="+portB)
}

// com0comRemove removes an existing pair (CNCAx CNCBx).
// Requires elevation (UAC prompt).
func com0comRemove(pairA, pairB string) error {
	if pairA == "" || pairB == "" {
		return fmt.Errorf("both pair end names required")
	}
	return runSetupcElevated("remove", pairA, pairB)
}

// com0comStatus returns the full com0com overview for the Web UI.
func com0comStatus() com0comInfo {
	info := com0comInfo{SetupcPath: defaultSetupcPath()}
	pairs, err := listCOMPairs()
	if err != nil {
		info.Message = fmt.Sprintf("list pairs failed: %v", err)
		return info
	}
	info.Pairs = pairs
	return info
}

func runPowerShellCapture(script string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return strings.TrimSpace(string(ee.Stderr)), fmt.Errorf("powershell exit %d: %v", ee.ExitCode(), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// marshalJSON helper so tests can inspect responses.
func toJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
