// Package progress draws the in-place bars a build shows while one long step
// runs, in the shape podman's own pull output uses — a label, a bracketed bar, a
// percentage, and the two sizes.
//
// Every step worth a bar here is a subprocess that says nothing while it works:
// `mke2fs -d` prints its stage lines and then sits on "Copying files into the
// device:" for the whole of the copy, because the stages carrying a progress
// meter are the fast ones, and `curl -s` is silent by construction. So the bars
// are driven by watching the FILE each step is writing grow, rather than by
// parsing anything the step emits — see Watch.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Reporter owns the terminal a bar is drawn on. A nil *Reporter, a nil W, and a
// W that is not a terminal all draw nothing, which is what keeps bars out of CI
// logs and out of a `--quiet` run: the phase lines around them carry the same
// facts one line at a time.
type Reporter struct {
	W io.Writer

	mu sync.Mutex // one bar at a time owns the line
}

// firstDraw is how long a step must run before its bar appears. A build whose
// artifacts are cached finishes each step in milliseconds, and a bar that
// flashes up and is erased again reads as a glitch rather than as progress.
const firstDraw = 200 * time.Millisecond

// redraw is the sampling interval once a bar is up.
const redraw = 120 * time.Millisecond

// Watch draws a bar for one step until the returned stop is called.
//
// sample reports how far the step has got and what it is heading for. It is
// called on a timer and must be cheap — it is a stat in both of this
// repository's uses. Both halves are sampled rather than fixed because a total
// is not always known when the bar starts: a download learns its size from a
// HEAD that may still be in flight.
//
// A total may be an estimate, so the bar never shows more than 99% — one that
// sits at 100% while the step is still running is worse than one that stops
// short. A total of 0 means nobody knows, and the bar then reports the bytes so
// far without a proportion.
func (r *Reporter) Watch(label string, sample func() (done, total int64)) (stop func()) {
	if r == nil || r.W == nil || !isTerminal(r.W) {
		return func() {}
	}
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		timer := time.NewTimer(firstDraw)
		defer timer.Stop()
		select {
		case <-stopped:
			return // the step beat the first draw; nothing was printed
		case <-timer.C:
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		tick := time.NewTicker(redraw)
		defer tick.Stop()
		for {
			done, total := sample()
			r.draw(label, done, total)
			select {
			case <-stopped:
				r.clear()
				return
			case <-tick.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopped)
			<-finished // the line is ours again only once the drawing goroutine is out
		})
	}
}

// barWidth is the drawn width of the bar itself. The whole line stays inside 80
// columns for the labels this repository uses.
const barWidth = 28

// draw renders one frame in place. \r returns to the start of the line and the
// frame is padded, so a frame that shrinks does not leave the tail of the last
// one behind it.
func (r *Reporter) draw(label string, done, total int64) {
	var bar, pct string
	// An estimate that has been beaten is simply wrong, and "6.0 GiB / 5.6 GiB"
	// reads as a bug rather than as a revision. Carry the total up to what the
	// step has actually produced, and let the cap below keep the bar honest about
	// not being finished. A total of 0 is not an estimate but an absence, and
	// stays one.
	if total > 0 && done > total {
		total = done
	}
	switch {
	case total > 0:
		frac := float64(done) / float64(total)
		if frac > 0.99 {
			frac = 0.99 // an estimate can be beaten; see Watch
		}
		filled := int(frac * barWidth)
		bar = "[" + strings.Repeat("=", filled) + ">" + strings.Repeat("-", barWidth-filled-1) + "]"
		pct = fmt.Sprintf(" %3.0f%%", frac*100)
	default:
		bar = "[" + strings.Repeat("-", barWidth) + "]"
	}
	sizes := humanBytes(done)
	if total > 0 {
		sizes += " / " + humanBytes(total)
	}
	fmt.Fprintf(r.W, "\r%s %s%s %s\033[K", label, bar, pct, sizes)
}

// clear removes the bar's line, because the caller's own completion line says
// what happened and two lines saying it would be one too many.
func (r *Reporter) clear() { fmt.Fprint(r.W, "\r\033[K") }

// humanBytes renders a size the way podman's pull does: one decimal, binary
// units, no padding.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// isTerminal reports whether w is a character device — a terminal someone is
// watching rather than a pipe or a file. Asked of the writer itself rather than
// of os.Stderr, so a caller that redirects its output gets the answer for where
// its output actually goes.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
