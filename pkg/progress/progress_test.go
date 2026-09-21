package progress

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestBars returns a Bars that renders like a terminal (in place, every
// event) into a buffer, so the drawn lines can be inspected.
func newTestBars() (*Bars, *bytes.Buffer) {
	var buf bytes.Buffer
	b := New(&buf)
	b.tty = true
	b.interval = 0
	b.width = 100
	return b, &buf
}

func TestNilBarsIsANoOp(t *testing.T) {
	var b *Bars
	b.Start("x", 1, 10)
	b.Item()
	task := b.Task("file", 10)
	task.Phase("download")
	task.Add(5)
	task.Done()
	b.Finish()

	if got := task.Reader(strings.NewReader("hi")); got == nil {
		t.Fatal("Reader on a nil task returned nil")
	}
	var buf bytes.Buffer
	if got := b.Writer(&buf); got != io.Writer(&buf) {
		t.Errorf("Writer on nil Bars = %v; want the writer itself", got)
	}
}

func TestOverallCountsTasksAsTheyFinish(t *testing.T) {
	b, buf := newTestBars()
	b.Start("uploading", 2, 300)

	first := b.Task("pool/main/h/hello_1.0_amd64.deb", 100)
	first.Add(50)
	if !strings.Contains(buf.String(), "hello_1.0_amd64.deb") {
		t.Error("the task line does not name the file being transferred")
	}
	first.Done()

	second := b.Task("pool/main/h/hello_2.0_amd64.deb", 200)
	second.Add(100)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.itemsDone != 1 {
		t.Errorf("itemsDone = %d; want 1", b.itemsDone)
	}
	if b.doneBytes != 100 {
		t.Errorf("doneBytes = %d; want 100 (the finished task's size)", b.doneBytes)
	}
	if got := b.overallLine(); !strings.Contains(got, "1/2") || !strings.Contains(got, "200 B/300 B") {
		t.Errorf("overall line = %q; want it to count 1/2 items and 200 B of 300 B", got)
	}
}

func TestDoneIsIdempotent(t *testing.T) {
	b, _ := newTestBars()
	b.Start("uploading", 1, 10)
	task := b.Task("f", 10)
	task.Done()
	task.Done()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.itemsDone != 1 || b.doneBytes != 10 {
		t.Errorf("after two Done calls: items=%d bytes=%d; want 1 and 10", b.itemsDone, b.doneBytes)
	}
}

func TestAsideIsShownButNotCounted(t *testing.T) {
	b, _ := newTestBars()
	b.Start("copying", 1, 100)
	aside := b.Aside("re-read", 100)
	aside.Add(100)

	b.mu.Lock()
	if got := b.overallLine(); !strings.Contains(got, "0 B/100 B") {
		b.mu.Unlock()
		t.Errorf("overall line = %q; want an aside's bytes left out", got)
	}
	b.mu.Unlock()

	aside.Done()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.itemsDone != 0 || b.doneBytes != 0 {
		t.Errorf("an aside credited the overall bar: items=%d bytes=%d", b.itemsDone, b.doneBytes)
	}
}

func TestReaderCountsBytesAndKeepsSeeking(t *testing.T) {
	b, _ := newTestBars()
	b.Start("uploading", 1, 8)
	task := b.Task("f", 8)

	// An object store needs a seekable body; losing that would make it spill
	// the whole upload to a temporary file first.
	r := task.Reader(strings.NewReader("12345678"))
	seeker, ok := r.(io.ReadSeeker)
	if !ok {
		t.Fatal("wrapping a ReadSeeker produced a reader that cannot seek")
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if got := taskDone(b, task); got != 8 {
		t.Errorf("after reading it all, the task counted %d bytes; want 8", got)
	}

	// Rewinding for a second pass (a retry, or the checksum pass an object
	// store makes before uploading) restarts the count.
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got := taskDone(b, task); got != 0 {
		t.Errorf("after rewinding, the task counted %d bytes; want 0", got)
	}
}

func TestWriterCountsBytes(t *testing.T) {
	b, _ := newTestBars()
	b.Start("downloading", 1, 4)
	task := b.Task("f", 4)
	if _, err := io.WriteString(task.Writer(io.Discard), "abcd"); err != nil {
		t.Fatal(err)
	}
	if got := taskDone(b, task); got != 4 {
		t.Errorf("the task counted %d bytes; want 4", got)
	}
}

func TestPhaseRestartsTheCount(t *testing.T) {
	b, _ := newTestBars()
	b.Start("copying", 1, 10)
	task := b.Task("f", 10)
	task.Phase("download")
	task.Add(10)
	task.Phase("upload")
	if got := taskDone(b, task); got != 0 {
		t.Errorf("a new phase left %d bytes on the count; want 0", got)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if got := b.taskLine(); !strings.Contains(got, "upload f") {
		t.Errorf("task line = %q; want it to name the current phase", got)
	}
}

func TestWriterInterleavesOutputWithTheBars(t *testing.T) {
	b, buf := newTestBars()
	b.Start("uploading", 1, 10)
	b.Task("f", 10)
	buf.Reset()

	out := b.Writer(buf)
	if _, err := io.WriteString(out, "upload f\n"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	_, after, found := strings.Cut(buf.String(), "upload f\n")
	if !found {
		t.Fatalf("output %q does not contain the line written", got)
	}
	if !strings.HasPrefix(got, "\r\x1b[") {
		t.Errorf("output %q does not start by erasing the drawn bars", got)
	}
	if !strings.Contains(after, "uploading") {
		t.Error("the bars were not redrawn below the line")
	}
}

func TestPlainOutputIsThrottledAndEscapeFree(t *testing.T) {
	var buf bytes.Buffer
	b := New(&buf) // not a terminal
	if b.tty {
		t.Fatal("a bytes.Buffer was taken for a terminal")
	}
	b.interval = time.Hour

	b.Start("uploading", 2, 200)
	task := b.Task("pool/main/h/hello_1.0_amd64.deb", 100)
	for range 10 {
		task.Add(10)
	}
	task.Done()

	got := buf.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain output carries escape sequences: %q", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("wrote %d lines for one throttled run; want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "uploading: 0/2") {
		t.Errorf("plain line %q does not report the overall position", got)
	}
}

func TestFinishErasesAndResets(t *testing.T) {
	b, buf := newTestBars()
	b.Start("uploading", 1, 10)
	b.Task("f", 10)
	buf.Reset()

	b.Finish()
	if got := buf.String(); !strings.HasPrefix(got, "\r\x1b[") {
		t.Errorf("Finish wrote %q; want it to erase the drawn lines", got)
	}
	buf.Reset()
	b.Finish() // nothing left to erase
	if got := buf.String(); got != "" {
		t.Errorf("a second Finish wrote %q; want nothing", got)
	}
}

func TestComposeFitsTheTerminalWidth(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		line := compose(width, "  pool/main/h/hello_1.0_amd64.deb", 0.5, true, "50 B/100 B")
		if got := len([]rune(line)); got >= width {
			t.Errorf("a %d-column line was rendered %d columns wide: %q", width, got, line)
		}
	}
}

func TestTruncateTailKeepsTheFilename(t *testing.T) {
	got := truncateTail("pool/main/h/hello/hello_1.0_amd64.deb", 20)
	if len([]rune(got)) != 20 {
		t.Errorf("truncateTail returned %d runes; want 20: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "_amd64.deb") {
		t.Errorf("truncateTail = %q; want the filename's tail kept", got)
	}
}

func TestShortDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second: "45s",
		62 * time.Second: "1m02s",
		90 * time.Minute: "1h30m",
		2*time.Hour + 5*time.Minute + 30*time.Second: "2h05m",
	}
	for in, want := range cases {
		if got := shortDuration(in); got != want {
			t.Errorf("shortDuration(%s) = %q; want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:              "0 B",
		512:            "512 B",
		1024:           "1.0 KiB",
		1536:           "1.5 KiB",
		1024 * 1024:    "1.0 MiB",
		3 * 1073741824: "3.0 GiB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q; want %q", in, got, want)
		}
	}
}

// TestConcurrentTasks exercises the locking: verify and check transfer several
// files at once, and the display is written from each of those goroutines.
func TestConcurrentTasks(t *testing.T) {
	b, _ := newTestBars()
	b.Start("verifying", 8, 800)
	out := b.Writer(io.Discard)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := b.Task(fmt.Sprintf("file-%d", i), 100)
			task.Phase("download")
			for range 10 {
				task.Add(10)
			}
			fmt.Fprintf(out, "done file-%d\n", i)
			task.Done()
		}()
	}
	wg.Wait()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.itemsDone != 8 || b.doneBytes != 800 {
		t.Errorf("after 8 concurrent tasks: items=%d bytes=%d; want 8 and 800", b.itemsDone, b.doneBytes)
	}
	if len(b.active) != 0 {
		t.Errorf("%d tasks were left in flight", len(b.active))
	}
}

// taskDone reads a task's current byte count under the display's lock.
func taskDone(b *Bars, t *Task) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return t.done
}
