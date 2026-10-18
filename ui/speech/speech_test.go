package speech

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	_ "github.com/egoist/mygo/locales/zh-Hans"
	"github.com/egoist/mygo/ui"
)

type fakeSession struct {
	mu        sync.Mutex
	audio     []int16
	finished  bool
	cancelled bool
}

func (f *fakeSession) PushAudio(s []int16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audio = append(f.audio, s...)
}
func (f *fakeSession) Finish() { f.mu.Lock(); f.finished = true; f.mu.Unlock() }
func (f *fakeSession) Cancel() { f.mu.Lock(); f.cancelled = true; f.mu.Unlock() }

type fakeRecognizer struct {
	sink      *Sink
	sess      *fakeSession
	err       error
	format    *Format
	available bool
}

func (r *fakeRecognizer) Start(sink *Sink) (Session, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.sink, r.sess = sink, &fakeSession{}
	return r.sess, nil
}

func (r *fakeRecognizer) Available() bool { return r.available }

type formatRecognizer struct{ *fakeRecognizer }

func (r formatRecognizer) Format() Format { return *r.format }

type fakeInput struct {
	sink    *AudioSink
	format  Format
	stopped bool
	err     error
}

func (in *fakeInput) Start(f Format, sink *AudioSink) (func(), error) {
	if in.err != nil {
		return nil, in.err
	}
	in.sink, in.format, in.stopped = sink, f, false
	return func() { in.stopped = true }, nil
}

// fakeTimer replaces Stop's timer with one the test fires.
type fakeTimer struct {
	f       func()
	stopped bool
}

func fakeTimers(t *testing.T) *[]*fakeTimer {
	var timers []*fakeTimer
	old := afterFunc
	afterFunc = func(d time.Duration, f func()) func() bool {
		ft := &fakeTimer{f: f}
		timers = append(timers, ft)
		return func() bool { ft.stopped = true; return true }
	}
	t.Cleanup(func() { afterFunc = old })
	return &timers
}

func setup(t *testing.T) (*State, *fakeRecognizer, *fakeInput, *[]*fakeTimer) {
	rec := &fakeRecognizer{available: true}
	in := &fakeInput{}
	timers := fakeTimers(t)
	return New(Options{Recognizer: rec, Input: in}), rec, in, timers
}

// drain returns the State's events, as a view's Events does.
func drain(s *State) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.events
	s.events = nil
	return ev
}

func kinds(evs []Event) []EventKind {
	var k []EventKind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestSession(t *testing.T) {
	s, rec, in, timers := setup(t)
	s.Start()
	if s.Status() != Connecting || in.format != DefaultFormat {
		t.Fatalf("status %v, input's format %v", s.Status(), in.format)
	}
	// Audio recorded while connecting reaches the session, which keeps it.
	in.sink.Push([]int16{1, 2, 3})
	rec.sink.Ready()
	if s.Status() != Recording {
		t.Fatalf("status %v after Ready", s.Status())
	}
	in.sink.Push([]int16{4})
	if !slices.Equal(rec.sess.audio, []int16{1, 2, 3, 4}) {
		t.Errorf("session's audio %v", rec.sess.audio)
	}
	rec.sink.Hypothesis("hel")
	rec.sink.Hypothesis("hel") // the same: no event
	rec.sink.Phrase("Hello, ")
	rec.sink.Hypothesis("wor")
	if got := s.Transcript(); got != "Hello, wor" {
		t.Errorf("Transcript = %q", got)
	}
	s.Stop()
	if s.Status() != Stopping || !in.stopped || !rec.sess.finished {
		t.Fatalf("after Stop: status %v, input stopped %v, session finished %v", s.Status(), in.stopped, rec.sess.finished)
	}
	s.Toggle() // ignored while stopping
	if s.Status() != Stopping {
		t.Fatalf("Toggle while stopping: %v", s.Status())
	}
	in.sink.Push([]int16{5}) // after the input stopped: dropped
	rec.sink.Phrase("world.")
	rec.sink.Finish()
	if s.Status() != Idle || !rec.sess.cancelled || !(*timers)[0].stopped {
		t.Errorf("after Finish: status %v, session released %v, timer stopped %v", s.Status(), rec.sess.cancelled, (*timers)[0].stopped)
	}
	want := []Event{
		{Kind: Started},
		{Kind: Partial, Text: "hel"},
		{Kind: Partial, Text: "Hello, "},
		{Kind: Partial, Text: "Hello, wor"},
		{Kind: Partial, Text: "Hello, world."},
		{Kind: Final, Text: "Hello, world."},
	}
	if got := drain(s); !slices.Equal(got, want) {
		t.Errorf("events\n%v, want\n%v", got, want)
	}
	if len(rec.sess.audio) != 4 {
		t.Errorf("audio after the input stopped reached the session: %v", rec.sess.audio)
	}

	// A stale sink does nothing to the next session.
	old := rec.sink
	s.Start()
	old.Phrase("stale")
	old.Finish()
	if s.Status() != Connecting || s.Transcript() != "" {
		t.Errorf("a stale sink changed the session: %v %q", s.Status(), s.Transcript())
	}
}

// Without the last result in time, the session ends with what it has.
func TestStopTimeout(t *testing.T) {
	s, rec, _, timers := setup(t)
	s.Start()
	rec.sink.Ready()
	rec.sink.Phrase("one ")
	rec.sink.Hypothesis("two")
	s.Stop()
	(*timers)[0].f()
	rec.sink.Phrase("late")
	if got := drain(s); got[len(got)-1] != (Event{Kind: Final, Text: "one two"}) || s.Status() != Idle {
		t.Errorf("events %v, status %v", got, s.Status())
	}
	if !rec.sess.cancelled {
		t.Error("the session isn't released")
	}
}

func TestCancel(t *testing.T) {
	s, rec, in, _ := setup(t)
	s.Start()
	rec.sink.Phrase("dropped")
	s.Cancel()
	s.Cancel()
	rec.sink.Finish()
	if got := kinds(drain(s)); !slices.Equal(got, []EventKind{Started, Partial, Cancelled}) {
		t.Errorf("events %v", got)
	}
	if !in.stopped || !rec.sess.cancelled || s.Status() != Idle {
		t.Errorf("input stopped %v, session cancelled %v, status %v", in.stopped, rec.sess.cancelled, s.Status())
	}
}

func TestErrors(t *testing.T) {
	boom := errors.New("boom")

	s, rec, _, _ := setup(t)
	rec.err = boom
	s.Start()
	if got := drain(s); len(got) != 2 || got[1] != (Event{Kind: Error, Err: boom}) || s.Status() != Idle {
		t.Errorf("recognizer failing to start: %v, %v", got, s.Status())
	}

	s, rec, in, _ := setup(t)
	in.err = boom
	s.Start()
	if got := drain(s); len(got) != 2 || got[1].Err != boom || !rec.sess.cancelled {
		t.Errorf("input failing to start: %v, session cancelled %v", got, rec.sess.cancelled)
	}

	s, rec, in, _ = setup(t)
	s.Start()
	in.sink.Error(boom)
	if got := drain(s); got[1].Err != boom || !in.stopped || !rec.sess.cancelled {
		t.Errorf("input failing: %v", got)
	}

	s, rec, in, _ = setup(t)
	s.Start()
	rec.sink.Error(boom)
	if got := drain(s); got[1].Err != boom || !in.stopped {
		t.Errorf("recognizer failing: %v", got)
	}
}

func TestUnavailable(t *testing.T) {
	in := &fakeInput{}
	s := New(Options{Input: in})
	s.Start()
	if s.HasRecognizer() || s.Available() || s.Status() != Idle || in.sink != nil {
		t.Error("a State without a recognizer started")
	}
	rec := &fakeRecognizer{}
	s = New(Options{Recognizer: rec, Input: in})
	s.Toggle()
	if !s.HasRecognizer() || s.Available() || s.Status() != Idle {
		t.Error("an unavailable recognizer started")
	}
}

// The input records in the recognizer's format.
func TestFormat(t *testing.T) {
	f := Format{SampleRate: 24000, Channels: 2}
	in := &fakeInput{}
	s := New(Options{Recognizer: formatRecognizer{&fakeRecognizer{available: true, format: &f}}, Input: in})
	s.Start()
	if in.format != f {
		t.Errorf("format %v, want %v", in.format, f)
	}
	// Levels count frames of both channels.
	in.sink.Push(make([]int16, 2*24000*int(LevelInterval/time.Millisecond)/1000))
	if n := len(s.Levels()); n != 1 {
		t.Errorf("%d levels for one interval of stereo", n)
	}
}

func TestLevels(t *testing.T) {
	s, _, in, _ := setup(t)
	s.Start()
	window := 16000 * int(LevelInterval/time.Millisecond) / 1000
	loud := make([]int16, window)
	loud[10] = 32767
	quiet := make([]int16, window)
	quiet[10] = 100 // -50 dB: noise
	in.sink.Push(loud)
	for range 20 {
		in.sink.Push(quiet)
	}
	lv := s.Levels()
	if len(lv) != 21 || lv[0] < 0.99 {
		t.Fatalf("levels %v", lv)
	}
	// Falling smoothly, then silence.
	if lv[1] < 0.6 || lv[1] >= lv[0] || lv[2] >= lv[1] || lv[20] != 0 {
		t.Errorf("levels %v", lv)
	}
	if level(0.5) <= level(0.1) || level(1) != 1 || level(0) != 0 {
		t.Errorf("level not rising with the peak")
	}
}

func TestPCM16(t *testing.T) {
	if got := PCM16([]float32{0, 1, -1, 2, -2, 0.5}); !slices.Equal(got, []int16{0, 32767, -32767, 32767, -32767, 16384}) {
		t.Errorf("PCM16 = %v", got)
	}
}

func TestButton(t *testing.T) {
	s, rec, _, _ := setup(t)
	var events []Event
	tt := ui.NewTester(func(c *ui.Context) {
		events = append(events, s.Events(c)...)
		ui.Row(c).Children(func() {
			Button(c, s)
			Waveform(c, s)
		})
	}, 300, 60)
	if err := tt.Click("Start dictation"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.Status() != Connecting || !tt.HasText("Stop dictation") {
		t.Fatalf("after a click: %v, %v", s.Status(), tt.Texts())
	}
	rec.sink.Ready()
	rec.sink.Phrase("hi")
	if !tt.FramePending() {
		t.Error("a phrase doesn't redraw the window")
	}
	tt.Frame()
	if err := tt.Click("Stop dictation"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if s.Status() != Stopping || !tt.HasText("Finishing dictation") {
		t.Fatalf("after a second click: %v, %v", s.Status(), tt.Texts())
	}
	tt.Click("Finishing dictation") // ignored
	tt.Frame()
	if s.Status() != Stopping {
		t.Fatal("a click while stopping did something")
	}
	rec.sink.Finish()
	tt.Frame()
	if !tt.HasText("Start dictation") || events[len(events)-1] != (Event{Kind: Final, Text: "hi"}) {
		t.Errorf("after Finish: %v, events %v", tt.Texts(), events)
	}
}

func TestButtonUnavailable(t *testing.T) {
	s := New(Options{Input: &fakeInput{}})
	tt := ui.NewTester(func(c *ui.Context) { Button(c, s) }, 100, 40)
	tt.Click("Dictation unavailable")
	tt.Frame()
	if s.Status() != Idle || !tt.HasText("Dictation unavailable") {
		t.Errorf("%v, %v", s.Status(), tt.Texts())
	}
}

func TestButtonLocalized(t *testing.T) {
	s, _, _, _ := setup(t)
	tt := ui.NewTester(func(c *ui.Context) { Button(c, s) }, 100, 40)
	tt.SetLocale("zh-Hans")
	tt.Frame()
	if !tt.HasText("开始听写") {
		t.Errorf("labels %v", tt.Texts())
	}
}

// selfEnding fails its session from PushAudio, through its sink.
type selfEnding struct {
	fakeSession
	sink  *Sink
	order []string
}

func (f *selfEnding) PushAudio(s []int16) {
	f.order = append(f.order, "push")
	f.sink.Error(errors.New("quota"))
	f.order = append(f.order, "pushed")
}
func (f *selfEnding) Cancel() { f.order = append(f.order, "cancel") }

type selfEndingRecognizer struct{ sess *selfEnding }

func (r *selfEndingRecognizer) Start(sink *Sink) (Session, error) {
	r.sess = &selfEnding{sink: sink}
	return r.sess, nil
}

// A session ending itself from its own method is cancelled after the
// method returns, not inside it.
func TestSessionEndsItself(t *testing.T) {
	fakeTimers(t)
	rec := &selfEndingRecognizer{}
	in := &fakeInput{}
	s := New(Options{Recognizer: rec, Input: in})
	s.Start()
	in.sink.Push([]int16{1})
	if !slices.Equal(rec.sess.order, []string{"push", "pushed", "cancel"}) {
		t.Errorf("calls %v", rec.sess.order)
	}
	if got := drain(s); got[len(got)-1].Kind != Error || s.Status() != Idle {
		t.Errorf("events %v", got)
	}
}

// No audio reaches a session after its Finish.
func TestNoAudioAfterFinish(t *testing.T) {
	s, rec, in, _ := setup(t)
	s.Start()
	sink := in.sink
	sink.Push([]int16{1})
	s.Stop()
	sink.Push([]int16{2})
	if !slices.Equal(rec.sess.audio, []int16{1}) || !rec.sess.finished {
		t.Errorf("audio %v, finished %v", rec.sess.audio, rec.sess.finished)
	}
}
