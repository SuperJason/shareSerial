package main

import "testing"

// TestPhysicalPortPresent validates the port-enumeration logic used by the
// plug/unplug watcher, without needing to physically unplug a device.
func TestPhysicalPortPresent(t *testing.T) {
	// A clearly non-existent port must report absent.
	ghost := &hub4comManager{physicalPort: "COM199"}
	if ghost.physicalPortPresent() {
		t.Fatalf("COM199 should not be enumerated, but physicalPortPresent()=true")
	}

	// The com0com virtual ports should be enumerated (they are always present
	// on the reference machine). This confirms the enumeration path works.
	com := &hub4comManager{physicalPort: "COM161"}
	if !com.physicalPortPresent() {
		t.Fatalf("COM161 should be enumerated (com0com pair), got false")
	}

	// The real Android UART (COM22) is conditional — it may be unplugged or
	// suspended right now, so only log it rather than assert.
	real := &hub4comManager{physicalPort: "COM22"}
	t.Logf("COM22 present = %v (device state dependent)", real.physicalPortPresent())
}
