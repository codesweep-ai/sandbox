package lend

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// A fault is a provider failure the lender answers with in the provider's
// place, so that what a turn driver, a wrapper or a fleet harness does under a
// throttle or an outage can be tried on a throwaway sandbox rather than learned
// from a real one.
//
// It is the operator's and never the caller's, like an origin (R147): it is a
// file the host writes beside the sandbox's loans, and no part of a request can
// arm one. It goes with the instance directory, as a loan does. The lender only
// reads it, and counts what it has served in memory, so its mounts stay
// read-only.

// Fault is one armed failure, for one sandbox.
type Fault struct {
	// ID tells one arming from the next, which is what the served count is
	// kept against. Arming again is a new ID and a fresh count.
	ID string `json:"id"`
	// Slot limits the fault to one of the sandbox's loans. Empty is every slot.
	Slot string `json:"slot,omitempty"`
	// Count is how many calls it answers before traffic flows again.
	Count int `json:"count"`
	// Status is the answer. Zero, with a Hang, drops the connection unanswered.
	Status     int `json:"status,omitempty"`
	RetryAfter int `json:"retry_after,omitempty"` // seconds, sent as Retry-After
	// Hang is how long the call is held before it is answered or dropped.
	Hang string `json:"hang,omitempty"`
	// Body and ContentType replace the lender's own error body. A provider that
	// throttles inside a 200 stream is reproduced this way, with the provider's
	// own event as the body, and the lender needs to know no provider's shapes.
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
}

// FaultsFile is the file name, beside loans.json in the instance directory.
const FaultsFile = "faults.json"

// MaxFaultBody bounds a replacement body. It is a test fixture, not a payload.
const MaxFaultBody = 64 << 10

type faultsDoc struct {
	Faults []Fault `json:"faults"`
}

// Validate reports what is wrong with a fault before it is armed.
func (f Fault) Validate() error {
	if f.Count < 1 {
		return errors.New("a fault answers at least one call")
	}
	if f.Status == 0 && f.Hang == "" {
		return errors.New("a fault needs a status to answer with, a hang, or both")
	}
	if f.Status != 0 && (f.Status < 200 || f.Status > 599) {
		return fmt.Errorf("status %d is not one a provider answers with", f.Status)
	}
	if f.Hang != "" {
		if d, err := time.ParseDuration(f.Hang); err != nil || d <= 0 {
			return fmt.Errorf("hang %q is not a duration such as 30s", f.Hang)
		}
	}
	if f.Slot != "" {
		if _, ok := SlotByID(f.Slot); !ok {
			return fmt.Errorf("unknown slot %q", f.Slot)
		}
	}
	if len(f.Body) > MaxFaultBody {
		return fmt.Errorf("the body is %d bytes, and a fault's is at most %d", len(f.Body), MaxFaultBody)
	}
	return nil
}

// ReadFaults returns the faults armed for the sandbox whose instance directory
// this is. None is not an error.
func ReadFaults(instDir string) ([]Fault, error) {
	data, err := os.ReadFile(filepath.Join(instDir, FaultsFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc faultsDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", FaultsFile, err)
	}
	return doc.Faults, nil
}

// WriteFaults replaces the sandbox's armed faults. An empty list removes the
// file.
func WriteFaults(instDir string, faults []Fault) error {
	path := filepath.Join(instDir, FaultsFile)
	if len(faults) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(faultsDoc{Faults: faults}, "", "  ")
	if err != nil {
		return err
	}
	// Renamed into place: the lender reads this file per call, and must never
	// see half of one.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Faults says whether a lent call is to be failed rather than forwarded.
type Faults interface {
	Next(loan Loan) (Fault, bool)
}

// FileFaults reads the faults under an instances directory, at
// <instances>/<group>/<name>/faults.json.
//
// Read per call and not cached: it is one small file at a known path, absent in
// every case but a test, and a fault has to take effect on the next call.
type FileFaults struct {
	Dir string

	mu     sync.Mutex
	served map[string]int
}

// NewFileFaults returns the faults backed by the instances directory.
func NewFileFaults(dir string) *FileFaults { return &FileFaults{Dir: dir} }

// Next returns the first armed fault that covers this loan and is not spent,
// and counts it as served.
func (f *FileFaults) Next(loan Loan) (Fault, bool) {
	if loan.Group == "" || loan.Name == "" {
		return Fault{}, false
	}
	faults, err := ReadFaults(filepath.Join(f.Dir, loan.Group, loan.Name))
	if err != nil || len(faults) == 0 {
		return Fault{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.served == nil {
		f.served = map[string]int{}
	}
	for _, ft := range faults {
		if ft.Slot != "" && ft.Slot != loan.Slot {
			continue
		}
		if ft.Validate() != nil || f.served[ft.ID] >= ft.Count {
			continue
		}
		f.served[ft.ID]++
		return ft, true
	}
	return Fault{}, false
}

// Served is how many calls an armed fault has answered, for reporting.
func (f *FileFaults) Served(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.served[id]
}

// FaultHeader marks an injected answer, so nothing downstream takes it for the
// provider's.
const FaultHeader = "X-Cs-Sandbox-Fault"

// inject answers a lent call with an armed fault. Nothing is forwarded and no
// credential is read: the call ends here.
func (s *Server) inject(w http.ResponseWriter, r *http.Request, loan Loan, f Fault) {
	s.count(func(st *Stats) { st.Injected++ })
	s.cfg.Log.Warn("injected a fault",
		slog.String("sandbox", loan.Name), slog.String("slot", loan.Slot), slog.String("fault", f.ID),
		slog.Int("status", f.Status), slog.String("hang", f.Hang),
		slog.String("method", r.Method), slog.String("path", r.URL.Path))
	if d, err := time.ParseDuration(f.Hang); err == nil && d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	if f.Status == 0 {
		// A hang with no answer: the connection closes with nothing sent, which
		// is what a provider that dropped the call looks like from the client.
		panic(http.ErrAbortHandler)
	}
	w.Header().Set(FaultHeader, f.ID)
	if f.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(f.RetryAfter))
	}
	if f.Body != "" {
		ct := f.ContentType
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(f.Status)
		_, _ = w.Write([]byte(f.Body))
		return
	}
	writeError(w, f.Status, "injected_fault",
		fmt.Sprintf("cs-sandbox answered this call with an injected %d (fault %s); the provider was not called", f.Status, f.ID))
}
