package ports

import "testing"

func TestAllocSkipsReserved(t *testing.T) {
	reserved := map[int]bool{2200: true, 2201: true}
	free := func(int) bool { return false } // nothing listening
	got, err := Alloc(2200, 2299, false, reserved, free)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2202 {
		t.Errorf("Alloc = %d, want 2202 (first non-reserved)", got)
	}
}

func TestAllocSkipsBusy(t *testing.T) {
	busy := func(p int) bool { return p == 2200 } // 2200 in use
	got, err := Alloc(2200, 2299, false, nil, busy)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2201 {
		t.Errorf("Alloc = %d, want 2201 (2200 busy)", got)
	}
}

func TestAllocVMModeSkipsProbe(t *testing.T) {
	// vmMode must not consult the busy probe (a VM's port is bound later).
	probed := false
	busy := func(int) bool { probed = true; return true }
	got, err := Alloc(2300, 2399, true, nil, busy)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2300 {
		t.Errorf("Alloc(vm) = %d, want 2300", got)
	}
	if probed {
		t.Error("vmMode must not call the busy probe")
	}
}

func TestAllocExhausted(t *testing.T) {
	reserved := map[int]bool{2200: true}
	if _, err := Alloc(2200, 2200, false, reserved, func(int) bool { return false }); err == nil {
		t.Fatal("expected exhaustion error")
	}
}
