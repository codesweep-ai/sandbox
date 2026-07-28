package ports

import "testing"

func TestAllocSkipsReserved(t *testing.T) {
	reserved := map[int]bool{2200: true, 2201: true}
	free := func(int) bool { return false } // nothing listening
	got, err := Alloc(2200, 2299, reserved, free)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2202 {
		t.Errorf("Alloc = %d, want 2202 (first non-reserved)", got)
	}
}

func TestAllocSkipsBusy(t *testing.T) {
	busy := func(p int) bool { return p == 2200 } // 2200 in use
	got, err := Alloc(2200, 2299, nil, busy)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2201 {
		t.Errorf("Alloc = %d, want 2201 (2200 busy)", got)
	}
}

// TestAllocSkipsBusyVMPort: a VM port answered by a forwarder outside this
// instances dir is taken, even though nothing local reserves it. Handing it out
// again would point `ssh <name>` at whichever sandbox already owns the port.
func TestAllocSkipsBusyVMPort(t *testing.T) {
	busy := func(p int) bool { return p == 2300 || p == 2301 }
	got, err := Alloc(2300, 2399, nil, busy)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2302 {
		t.Errorf("Alloc = %d, want 2302 (2300/2301 answered by foreign forwarders)", got)
	}
}

func TestAllocExhausted(t *testing.T) {
	reserved := map[int]bool{2200: true}
	if _, err := Alloc(2200, 2200, reserved, func(int) bool { return false }); err == nil {
		t.Fatal("expected exhaustion error")
	}
}
