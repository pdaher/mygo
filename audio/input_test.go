package audio

import (
	"errors"
	"math"
	"runtime"
	"sync"
	"testing"

	"github.com/egoist/mygo/internal/wait"
)

// fakeInput gives the chunks of reads in turn, then nothing, or err, or
// empty chunks while busy.
type fakeInput struct {
	mu     sync.Mutex
	reads  [][]float32
	err    error // after the reads
	busy   bool
	closed bool
}

func (d *fakeInput) read() ([]float32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.reads) == 0 {
		if d.busy {
			runtime.Gosched()
			return []float32{}, nil
		}
		return nil, d.err
	}
	c := d.reads[0]
	d.reads = d.reads[1:]
	return c, nil
}

func (d *fakeInput) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
}

func (d *fakeInput) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// fakeInputs makes openInput open devs in turn, then fail.
func fakeInputs(t *testing.T, devs ...*fakeInput) {
	t.Helper()
	var mu sync.Mutex
	old := openInput
	openInput = func() (inputDevice, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(devs) == 0 {
			return nil, errors.New("no microphone")
		}
		d := devs[0]
		devs = devs[1:]
		return d, nil
	}
	t.Cleanup(func() { openInput = old })
}

// collect records with a fake input until n samples or an error came.
type collector struct {
	mu      sync.Mutex
	samples []float32
	calls   int
	err     error
}

func (c *collector) fn(s []float32, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.samples = append(c.samples, s...)
	if err != nil {
		c.err = err
	}
}

func (c *collector) got() ([]float32, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]float32(nil), c.samples...), c.calls, c.err
}

func sine(freq float64, n int, from int) []float32 {
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(from+i)/inputRate))
	}
	return s
}

func chunks(freq float64, chunk, count int) [][]float32 {
	var out [][]float32
	for i := range count {
		out = append(out, sine(freq, chunk, i*chunk))
	}
	return out
}

// A recording at 16 kHz gets a third of the frames, the same tone, in as
// many channels as asked.
func TestRecordConverts(t *testing.T) {
	for _, ch := range []int{1, 2} {
		dev := &fakeInput{reads: chunks(440, 480, 100), err: errors.New("unplugged")}
		fakeInputs(t, dev)
		var c collector
		r := Record(16000, ch, c.fn)
		wait.Until(t, "the recording to end", func() bool { _, _, err := c.got(); return err != nil })
		r.Stop()
		s, _, err := c.got()
		if err.Error() != "no microphone" {
			t.Errorf("err = %v, want the failed reopening's", err)
		}
		frames := len(s) / ch
		if want := 100 * 480 / 3; frames < want-2 || frames > want+2 {
			t.Errorf("%d channels: %d frames, want %d", ch, frames, want)
		}
		if ch == 2 {
			for i := 0; i < len(s); i += 2 {
				if s[i] != s[i+1] {
					t.Fatalf("frame %d: left %v, right %v", i/2, s[i], s[i+1])
				}
			}
		}
		// Past the filter's start, the tone's amplitude and its period:
		// 16000/440 frames.
		peak := float32(0)
		crossings := 0
		mono := make([]float32, 0, frames)
		for i := 0; i < len(s); i += ch {
			mono = append(mono, s[i])
		}
		for i := 1000; i < len(mono); i++ {
			peak = max(peak, mono[i])
			if mono[i-1] < 0 && mono[i] >= 0 {
				crossings++
			}
		}
		if peak < 0.45 || peak > 0.55 {
			t.Errorf("peak %v, want 0.5", peak)
		}
		want := float64(len(mono)-1000) * 440 / 16000
		if math.Abs(float64(crossings)-want) > 2 {
			t.Errorf("%d periods, want %.0f", crossings, want)
		}
	}
}

// A tone above the new Nyquist frequency doesn't fold back into what is
// recorded.
func TestRecordFilters(t *testing.T) {
	dev := &fakeInput{reads: chunks(12000, 480, 50)}
	fakeInputs(t, dev)
	var c collector
	r := Record(16000, 1, c.fn)
	wait.Until(t, "the audio", func() bool { s, _, _ := c.got(); return len(s) >= 50*160-2 })
	r.Stop()
	s, _, _ := c.got()
	peak := float32(0)
	for _, v := range s[1000:] {
		peak = max(peak, float32(math.Abs(float64(v))))
	}
	if peak > 0.01 {
		t.Errorf("12 kHz at 16 kHz: peak %v, want it filtered out", peak)
	}
}

// At the device's rate, samples pass as they are.
func TestRecordSameRate(t *testing.T) {
	dev := &fakeInput{reads: [][]float32{{0.1, 0.2}, {0.3}}}
	fakeInputs(t, dev)
	var c collector
	r := Record(inputRate, 1, c.fn)
	wait.Until(t, "the audio", func() bool { s, _, _ := c.got(); return len(s) == 3 })
	r.Stop()
	if s, _, _ := c.got(); s[0] != 0.1 || s[1] != 0.2 || s[2] != 0.3 {
		t.Errorf("samples %v", s)
	}
}

// A microphone that can't open ends the recording with its error, once.
func TestRecordOpenFails(t *testing.T) {
	fakeInputs(t)
	var c collector
	Record(16000, 1, c.fn)
	wait.Until(t, "the error", func() bool { _, _, err := c.got(); return err != nil })
	if _, calls, err := c.got(); calls != 1 || err.Error() != "no microphone" {
		t.Errorf("%d calls, err %v", calls, err)
	}
}

// A device failing before it gave anything isn't opened again; one that
// went after working is, and the recording goes on with the new one.
func TestRecordReopens(t *testing.T) {
	broken := &fakeInput{err: errors.New("broken")}
	fakeInputs(t, broken, &fakeInput{})
	var c collector
	Record(inputRate, 1, c.fn)
	wait.Until(t, "the error", func() bool { _, _, err := c.got(); return err != nil })
	if _, _, err := c.got(); err.Error() != "broken" || !broken.isClosed() {
		t.Errorf("err %v, closed %v", err, broken.isClosed())
	}

	first := &fakeInput{reads: [][]float32{{0.1}}, err: errors.New("unplugged")}
	second := &fakeInput{reads: [][]float32{{0.2}}, busy: true}
	fakeInputs(t, first, second)
	var c2 collector
	r := Record(inputRate, 1, c2.fn)
	wait.Until(t, "audio from the second microphone", func() bool { s, _, _ := c2.got(); return len(s) == 2 })
	r.Stop()
	wait.Until(t, "the second to close", second.isClosed)
	if s, _, err := c2.got(); s[1] != 0.2 || err != nil || !first.isClosed() {
		t.Errorf("samples %v, err %v, first closed %v", s, err, first.isClosed())
	}
}

// A device giving nothing for inputStall counts as gone.
func TestRecordStalls(t *testing.T) {
	dev := &fakeInput{reads: [][]float32{{0.1}}}
	fakeInputs(t, dev)
	var c collector
	Record(inputRate, 1, c.fn)
	wait.Until(t, "the error", func() bool { _, _, err := c.got(); return err != nil })
	if _, _, err := c.got(); err.Error() != "no microphone" || !dev.isClosed() {
		t.Errorf("err %v", err)
	}
}

// Stop frees the microphone, and fn hears no more.
func TestRecordStop(t *testing.T) {
	dev := &fakeInput{}
	fakeInputs(t, dev)
	var c collector
	r := Record(16000, 1, c.fn)
	r.Stop()
	r.Stop()
	<-r.done
	if !dev.isClosed() {
		t.Error("the device is open")
	}
	if _, calls, _ := c.got(); calls != 0 {
		t.Errorf("%d calls after Stop", calls)
	}
}
