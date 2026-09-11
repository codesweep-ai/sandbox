package progress

import (
	"bytes"
	"strings"
	"testing"
)

// frame renders one bar frame the way the drawing goroutine would, so the tests
// below can read what a terminal would show without being one.
func frame(label string, done, total int64) string {
	var b bytes.Buffer
	(&Reporter{W: &b}).draw(label, done, total)
	return b.String()
}

// TestDrawShowsTheProportion: the whole point of the bar over the phase line it
// sits under is that it says how far along the step is.
func TestDrawShowsTheProportion(t *testing.T) {
	got := frame("  base filesystem", 3<<30, 6<<30)
	for _, want := range []string{"  base filesystem", "[", "=", ">", "-", "]", " 50%", "3.0 GiB / 6.0 GiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame = %q, want it to contain %q", got, want)
		}
	}
	// In place, and padded: a frame that shrinks must not leave the tail of the
	// longer one behind it on the line.
	if !strings.HasPrefix(got, "\r") || !strings.HasSuffix(got, "\033[K") {
		t.Errorf("frame = %q, want it drawn in place over the whole line", got)
	}
}

// TestDrawCapsAnEstimate: the filesystem bar's total is the image's size, and
// ext4 rounds 145k files up to a block each — so the real disk runs over it. A
// bar that reads 100% while the step is still running is worse than one that
// stops at 99%.
func TestDrawCapsAnEstimate(t *testing.T) {
	got := frame("x", 7<<30, 6<<30)
	if !strings.Contains(got, " 99%") {
		t.Errorf("frame past its estimate = %q, want it held at 99%%", got)
	}
	if strings.Contains(got, "100%") {
		t.Errorf("frame = %q, want no 100%% before the step is done", got)
	}
	// And the beaten estimate is revised rather than shown: "7.0 GiB / 6.0 GiB"
	// reads as a bug.
	if !strings.Contains(got, "7.0 GiB / 7.0 GiB") {
		t.Errorf("frame = %q, want the total carried up to what was produced", got)
	}
}

// TestDrawWithoutATotal: a download whose HEAD has not landed yet knows only how
// far it has got, and says that rather than inventing a proportion.
func TestDrawWithoutATotal(t *testing.T) {
	got := frame("  firecracker v1.16.0", 1<<20, 0)
	if strings.Contains(got, "%") {
		t.Errorf("frame with no total = %q, want no percentage", got)
	}
	if !strings.Contains(got, "1.0 MiB") || strings.Contains(got, "/") {
		t.Errorf("frame with no total = %q, want the bytes so far and no proportion", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{1536, "1.5 KiB"},
		{3<<20 + 512<<10, "3.5 MiB"},
		{6 << 30, "6.0 GiB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestWatchDrawsNothingOffATerminal: bars are redrawn in place, so in a CI log
// or a redirected run they would be thousands of lines of the same line. The
// phase lines around them carry the same facts one line at a time.
func TestWatchDrawsNothingOffATerminal(t *testing.T) {
	var b bytes.Buffer
	r := &Reporter{W: &b}
	stop := r.Watch("x", func() (int64, int64) { return 1, 2 })
	stop()
	if b.Len() != 0 {
		t.Errorf("wrote %q to a non-terminal, want nothing", b.String())
	}
	// A nil Reporter is the no-bars case every test and --quiet run takes, and it
	// has to be callable rather than checked for at every call site.
	var none *Reporter
	none.Watch("x", func() (int64, int64) { return 1, 2 })()
}
