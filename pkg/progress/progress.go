// Package progress renders optional progress bars for the transfers a
// repository operation performs. It draws two things: the file currently
// moving, and the run as a whole.
//
// A nil *Bars is a working no-op, so a command that was not asked for progress
// passes nil rather than guarding every call site. The same is true of a nil
// *Task, which is what a nil *Bars hands back.
//
// Nothing here assumes a terminal. When the output is not one — a CI log, a
// redirected run — the display degrades to an occasional plain line rather than
// writing escape sequences into a file nobody can read.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Renderer tuning.
const (
	// ttyInterval is the shortest gap between two redraws on a terminal. Ten
	// frames a second reads as smooth without making a transfer wait on the
	// terminal.
	ttyInterval = 100 * time.Millisecond
	// plainInterval is the gap between two progress lines when the output is
	// not a terminal, where every update is another line in whatever is
	// capturing it. Rare enough to stay readable, often enough to show that a
	// long transfer is still alive.
	plainInterval = 5 * time.Second
	// defaultWidth is assumed when the terminal's width cannot be read.
	defaultWidth = 80
	// minBarWidth and maxBarWidth bound the drawn bar, so a narrow terminal
	// still shows a bar and a wide one does not fill with block characters.
	minBarWidth = 8
	maxBarWidth = 28
	// minNameWidth keeps a recognizable piece of the filename even when the
	// figures beside it leave almost no room.
	minNameWidth = 8
)

// ANSI control sequences. Only three are needed: move the cursor up, erase
// everything below it, and return to the start of the line.
const (
	escCursorUp   = "\x1b[%dA"
	escEraseBelow = "\x1b[0J"
)

// Bars is a progress display: one line for the transfer in flight and one for
// the run as a whole. It is safe for concurrent use, because the operations it
// measures (verify, check) transfer several files at once.
type Bars struct {
	w        io.Writer
	tty      bool
	interval time.Duration
	width    int

	mu         sync.Mutex
	label      string  // what the overall bar is counting ("uploading", ...)
	items      int     // items the run expects to complete
	itemsDone  int     // items completed so far
	totalBytes int64   // bytes the run expects to move, 0 when unknown
	doneBytes  int64   // bytes credited by completed tasks
	active     []*Task // tasks in flight, oldest first
	started    time.Time
	lastDraw   time.Time
	drawn      int // lines the display currently occupies
}

// New returns a Bars that draws on w. When w is a terminal the display is
// redrawn in place; otherwise it falls back to a plain line every few seconds.
func New(w io.Writer) *Bars {
	b := &Bars{w: w, interval: plainInterval, width: defaultWidth}
	f, ok := w.(*os.File)
	if !ok {
		return b
	}
	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return b
	}
	b.tty = true
	b.interval = ttyInterval
	if cols, _, err := term.GetSize(fd); err == nil && cols > 0 {
		b.width = cols
	}
	return b
}

// Start declares the work the overall bar measures: items files totalling bytes
// (0 when the byte count is not known, in which case the bar counts items).
// Calling it again begins a fresh run, so a command that transfers in two
// distinct phases gets an overall bar describing the phase it is in.
func (b *Bars) Start(label string, items int, bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.label, b.items, b.totalBytes = label, items, bytes
	b.itemsDone, b.doneBytes = 0, 0
	b.active = nil
	b.started = time.Now()
	b.lastDraw = time.Time{}
	b.draw(true)
}

// Task starts one file's transfer of size bytes (0 when the size is not known)
// and returns the handle its progress is reported through. Several tasks may be
// in flight at once; the display shows the oldest and counts the others.
func (b *Bars) Task(label string, size int64) *Task {
	return b.newTask(label, size, true)
}

// Aside starts a task the overall bar does not count — work the run did not
// plan for, such as re-reading a file that was just written. It is shown while
// it runs, but completing it neither advances the item count nor credits bytes,
// so the overall figures keep describing the work that was announced.
func (b *Bars) Aside(label string, size int64) *Task {
	return b.newTask(label, size, false)
}

func (b *Bars) newTask(label string, size int64, counted bool) *Task {
	if b == nil {
		return nil
	}
	t := &Task{bars: b, label: label, size: size, counted: counted}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.active = append(b.active, t)
	b.draw(true)
	return t
}

// Item credits one completed item that moved no bytes through this process — a
// server-side relocation, a deletion — so the overall count covers the whole
// run rather than only its transfers.
func (b *Bars) Item() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.itemsDone++
	b.draw(false)
}

// Finish erases the display. It may be called more than once, and the Bars can
// be reused for another run afterwards.
func (b *Bars) Finish() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.erase()
	b.items, b.itemsDone, b.totalBytes, b.doneBytes = 0, 0, 0, 0
	b.active = nil
}

// Writer returns a writer that prints through the display: the bars are erased,
// w receives the bytes, and the bars are drawn again below them. Output written
// to w directly would be overwritten by the next redraw, so a command that
// prints while transferring routes its output through here.
func (b *Bars) Writer(w io.Writer) io.Writer {
	if b == nil {
		return w
	}
	return &interleaved{bars: b, w: w}
}

// interleaved is the writer Bars.Writer returns.
type interleaved struct {
	bars *Bars
	w    io.Writer
}

func (i *interleaved) Write(p []byte) (int, error) {
	i.bars.mu.Lock()
	defer i.bars.mu.Unlock()
	i.bars.erase()
	n, err := i.w.Write(p)
	i.bars.draw(true)
	return n, err
}

// Task is one file's transfer. Its byte count feeds both the task bar and,
// while it is in flight, the overall one.
type Task struct {
	bars  *Bars
	label string
	phase string
	size  int64

	// done is the bytes moved in the current phase, and is what both bars
	// count. A phase change, or a reader being rewound for a second pass over
	// the same file, takes it back to zero: the pass really is starting again,
	// and a bar that stayed full would claim work that is about to be redone.
	done int64
	// peak is the furthest done has reached, used to credit the overall bar for
	// a task whose size was not known in advance.
	peak int64
	// counted is false for a task started with Aside, whose completion the
	// overall bar ignores.
	counted bool
	ended   bool
}

// Phase names the stage the task has reached ("download", "upload") and
// restarts its byte count. Copying a file moves it twice — out of the source
// and into the destination — and each pass is its own bar.
func (t *Task) Phase(name string) {
	if t == nil {
		return
	}
	t.bars.mu.Lock()
	defer t.bars.mu.Unlock()
	t.phase = name
	t.done = 0
	t.bars.draw(true)
}

// Add records n more bytes moved. A negative n rewinds the count, which is what
// a reader seeked back to the start amounts to.
func (t *Task) Add(n int64) {
	if t == nil || n == 0 {
		return
	}
	t.bars.mu.Lock()
	defer t.bars.mu.Unlock()
	t.done += n
	if t.done < 0 {
		t.done = 0
	}
	if t.done > t.peak {
		t.peak = t.done
	}
	t.bars.draw(false)
}

// Done completes the task: it leaves the display, and its size counts towards
// the overall bar. Calling it twice is harmless, so it can be deferred.
func (t *Task) Done() {
	if t == nil {
		return
	}
	t.bars.mu.Lock()
	defer t.bars.mu.Unlock()
	if t.ended {
		return
	}
	t.ended = true

	b := t.bars
	if t.counted {
		credited := t.size
		if credited <= 0 {
			credited = t.peak
		}
		b.doneBytes += credited
		b.itemsDone++
	}
	for i, other := range b.active {
		if other == t {
			b.active = append(b.active[:i], b.active[i+1:]...)
			break
		}
	}
	b.draw(true)
}

// Reader wraps r so that every byte read through it is added to the task.
//
// The wrapper is itself an io.ReadSeeker when r is one: an object store needs a
// seekable body to sign a request, and a reader that cannot seek makes it spill
// the whole file to a temporary copy first. A seek re-points the count at the
// new offset, so a body rewound for a retry re-reports its progress rather than
// counting the same bytes twice.
func (t *Task) Reader(r io.Reader) io.Reader {
	if t == nil {
		return r
	}
	c := counting{task: t, r: r}
	if s, ok := r.(io.Seeker); ok {
		return &countingSeeker{counting: c, s: s}
	}
	return &c
}

// Writer wraps w so that every byte written through it is added to the task.
func (t *Task) Writer(w io.Writer) io.Writer {
	if t == nil {
		return w
	}
	return &countingWriter{task: t, w: w}
}

type counting struct {
	task *Task
	r    io.Reader
	n    int64 // bytes counted through this wrapper
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		c.task.Add(int64(n))
	}
	return n, err
}

type countingSeeker struct {
	counting
	s io.Seeker
}

func (c *countingSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := c.s.Seek(offset, whence)
	if err != nil {
		return pos, err
	}
	// The stream is now at pos, so that — not what has been read through this
	// wrapper — is how far along the file is.
	c.task.Add(pos - c.n)
	c.n = pos
	return pos, nil
}

type countingWriter struct {
	task *Task
	w    io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.task.Add(int64(n))
	}
	return n, err
}

// draw repaints the display. force bypasses the redraw interval on a terminal,
// where a redraw costs nothing worth saving; off a terminal the interval always
// applies, because every update there is another line of log.
//
// It must be called with b.mu held.
func (b *Bars) draw(force bool) {
	if b.items == 0 && len(b.active) == 0 {
		b.erase()
		return
	}
	now := time.Now()
	if !force || !b.tty {
		if now.Sub(b.lastDraw) < b.interval {
			return
		}
	}
	b.lastDraw = now

	if !b.tty {
		fmt.Fprintln(b.w, b.plainLine())
		return
	}
	b.paint(b.lines())
}

// paint replaces the drawn block with lines.
func (b *Bars) paint(lines []string) {
	b.erase()
	for _, l := range lines {
		fmt.Fprintln(b.w, l)
	}
	b.drawn = len(lines)
}

// erase removes the drawn block, leaving the cursor where it started.
func (b *Bars) erase() {
	if b.drawn == 0 {
		return
	}
	fmt.Fprintf(b.w, "\r"+escCursorUp+escEraseBelow, b.drawn)
	b.drawn = 0
}

// lines renders the display: the transfer in flight (when there is one) above
// the run as a whole.
func (b *Bars) lines() []string {
	if len(b.active) == 0 {
		return []string{b.overallLine()}
	}
	return []string{b.taskLine(), b.overallLine()}
}

// taskLine renders the oldest transfer in flight, noting how many others are
// running alongside it.
func (b *Bars) taskLine() string {
	t := b.active[0]
	name := t.label
	if t.phase != "" {
		name = t.phase + " " + name
	}
	if n := len(b.active) - 1; n > 0 {
		name = fmt.Sprintf("%s (+%d more)", name, n)
	}

	var frac float64
	right := HumanBytes(t.done)
	if t.size > 0 {
		frac = float64(t.done) / float64(t.size)
		right = fmt.Sprintf("%s/%s", HumanBytes(t.done), HumanBytes(t.size))
	}
	return compose(b.width, "  "+name, frac, t.size > 0, right)
}

// overallLine renders the run as a whole: how many items are done, how many
// bytes have moved, and — once there is enough to extrapolate from — the rate
// and what is left.
func (b *Bars) overallLine() string {
	done, total := b.doneBytes, b.totalBytes
	for _, t := range b.active {
		if t.counted {
			done += t.done
		}
	}

	byteBased := total > 0
	frac := 0.0
	switch {
	case byteBased:
		frac = float64(done) / float64(total)
	case b.items > 0:
		frac = float64(b.itemsDone) / float64(b.items)
	}
	if frac > 1 {
		frac = 1
	}

	right := fmt.Sprintf("%d/%d", b.itemsDone, b.items)
	if byteBased {
		right += fmt.Sprintf("  %s/%s", HumanBytes(done), HumanBytes(total))
	}
	if rate, eta, ok := b.rate(done, total); ok {
		right += fmt.Sprintf("  %s/s", HumanBytes(rate))
		if eta > 0 {
			right += "  ETA " + shortDuration(eta)
		}
	}

	label := b.label
	if label == "" {
		label = "overall"
	}
	return compose(b.width, "  "+label, frac, true, right)
}

// rate reports the transfer's average speed and, when the total is known, how
// long the rest should take. It reports nothing until the run has been going
// long enough for an average to mean anything.
func (b *Bars) rate(done, total int64) (bytesPerSec int64, eta time.Duration, ok bool) {
	elapsed := time.Since(b.started)
	if b.started.IsZero() || elapsed < time.Second || done <= 0 {
		return 0, 0, false
	}
	perSec := float64(done) / elapsed.Seconds()
	if perSec <= 0 {
		return 0, 0, false
	}
	if total > done {
		eta = time.Duration(float64(total-done) / perSec * float64(time.Second))
	}
	return int64(perSec), eta, true
}

// plainLine is the single line written when the output is not a terminal.
func (b *Bars) plainLine() string {
	label := b.label
	if label == "" {
		label = "progress"
	}
	line := fmt.Sprintf("%s: %d/%d", label, b.itemsDone, b.items)

	done := b.doneBytes
	for _, t := range b.active {
		if t.counted {
			done += t.done
		}
	}
	if b.totalBytes > 0 {
		line += fmt.Sprintf(", %s/%s (%.0f%%)", HumanBytes(done), HumanBytes(b.totalBytes),
			100*float64(done)/float64(b.totalBytes))
	}
	if rate, eta, ok := b.rate(done, b.totalBytes); ok {
		line += fmt.Sprintf(", %s/s", HumanBytes(rate))
		if eta > 0 {
			line += ", ETA " + shortDuration(eta)
		}
	}
	if len(b.active) > 0 {
		line += " — " + b.active[0].label
	}
	return line
}

// compose lays out one line as "<name> [bar] <right>", giving the bar and the
// right-hand figures the room they need and the name whatever is left. A name
// too long for the space keeps its tail, because that is the part that
// identifies a file.
func compose(width int, name string, frac float64, showBar bool, right string) string {
	if width < 1 {
		width = defaultWidth
	}
	barWidth := min(max(width/4, minBarWidth), maxBarWidth)

	bar := ""
	if showBar {
		bar = renderBar(barWidth, frac) + "  "
	}
	// One column is left unused so that a line exactly as wide as the terminal
	// does not wrap onto a second one, which would leave a stray line behind
	// when the display is erased.
	room := max(width-1-runeLen(bar)-runeLen(right)-2, minNameWidth)
	name = truncateTail(name, room)
	return name + strings.Repeat(" ", room-runeLen(name)) + "  " + bar + right
}

// runeLen counts the columns a string occupies. Every character this package
// draws is one column wide, so counting runes is enough.
func runeLen(s string) int { return len([]rune(s)) }

// renderBar draws a proportional bar barWidth columns wide.
func renderBar(barWidth int, frac float64) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(barWidth) + 0.5)
	pct := fmt.Sprintf("%3.0f%%", frac*100)
	return fmt.Sprintf("[%s%s] %s", strings.Repeat("█", filled), strings.Repeat("░", barWidth-filled), pct)
}

// truncateTail shortens s to width runes, keeping the end — the filename at the
// end of a path says more than the pool directory at its start.
func truncateTail(s string, width int) string {
	r := []rune(s)
	if len(r) <= width || width < 2 {
		return s
	}
	return "…" + string(r[len(r)-(width-1):])
}

// shortDuration renders a duration the way a transfer estimate wants to be
// read: no more than two units, and no fractions.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// HumanBytes renders a byte count in the largest unit that keeps it readable.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
