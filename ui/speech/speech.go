// Package speech turns what the user says into text, for dictation in
// windows of native UI: a State runs a session that records the
// microphone (an Input) and hands its audio to a Recognizer, a service of
// the app's choosing, which tells it what was said; Button starts and
// stops it, and Waveform shows the microphone's level as it records.
//
//	type app struct {
//		text    string
//		speech  *speech.State
//	}
//
//	func (a *app) view(c *ui.Context) {
//		for _, ev := range a.speech.Events(c) {
//			if ev.Kind == speech.Final {
//				a.text += ev.Text
//			}
//		}
//		ui.Row(c).Gap(6).Children(func() {
//			ui.TextInput(c, &a.text).Grow(1)
//			if a.speech.Status().Capturing() {
//				speech.Waveform(c, a.speech)
//			}
//			speech.Button(c, a.speech)
//		})
//	}
//
//	a.speech = speech.New(speech.Options{Recognizer: myService})
//
// The State never edits text itself: the app commits Final, and may show
// Partial as a preview. Package speech recognizes nothing on its own;
// a Recognizer wraps a cloud service or a local model.
package speech

import (
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
)

// Status is where a State's session is.
type Status int

const (
	// Idle: no session runs.
	Idle Status = iota
	// Connecting: the microphone records while the recognizer gets
	// ready, which keeps what it recorded until then.
	Connecting
	// Recording: the microphone records and the recognizer recognizes.
	Recording
	// Stopping: the microphone stopped, and the state waits for the
	// recognizer's last result.
	Stopping
)

// Active reports whether a session runs: any status but Idle.
func (s Status) Active() bool { return s != Idle }

// Capturing reports whether the microphone records: Connecting or
// Recording.
func (s Status) Capturing() bool { return s == Connecting || s == Recording }

func (s Status) String() string {
	switch s {
	case Connecting:
		return "Connecting"
	case Recording:
		return "Recording"
	case Stopping:
		return "Stopping"
	}
	return "Idle"
}

// EventKind is what an Event tells.
type EventKind int

const (
	// Started: a session started.
	Started EventKind = iota
	// Partial: the transcript changed while the user speaks. Its Text,
	// what was said in the whole session so far, replaces the Partial
	// before it: show it as a preview, and commit only Final.
	Partial
	// Final: the session ended with Text, what was said in all of it.
	Final
	// Cancelled: the session ended with Cancel, its transcript dropped.
	Cancelled
	// Error: the session ended because the recognizer or the input
	// failed, with Err.
	Error
)

func (k EventKind) String() string {
	return [...]string{"Started", "Partial", "Final", "Cancelled", "Error"}[k]
}

// An Event is what happened to a State's session.
type Event struct {
	Kind EventKind
	Text string // the transcript, of Partial and Final
	Err  error  // of Error
}

// Format is the format of the audio a recognizer takes: signed 16-bit
// samples, rate frames a second of Channels interleaved.
type Format struct {
	SampleRate int
	Channels   int
}

// DefaultFormat is what recognizers take unless they say otherwise: 16 kHz
// mono, as most speech services want.
var DefaultFormat = Format{SampleRate: 16000, Channels: 1}

// A Recognizer turns audio into text, as a speech service of the cloud
// or a local model. Start starts a session, which tells sink what it
// recognizes; it is called on the main thread, and must not wait on the
// network: a session connects in the background, keeping the audio it
// gets until then, and calls sink.Ready once connected.
//
// A recognizer may also have the methods
//
//	Format() Format   // the audio it takes, unless DefaultFormat
//	Available() bool  // false while it can't work, as before sign-in
//
// Available runs as the Button builds, every frame: keep it cheap.
type Recognizer interface {
	Start(sink *Sink) (Session, error)
}

// A Session recognizes one dictation. Its methods are called one at a
// time, from any goroutine.
type Session interface {
	// PushAudio takes the next audio, interleaved in the recognizer's
	// Format; the session may keep samples.
	PushAudio(samples []int16)
	// Finish tells the session that the user stopped speaking: it sends
	// what audio it has left, waits for the last result and calls
	// sink.Finish.
	Finish()
	// Cancel ends the session at once, its result unwanted: it frees what
	// it holds, as a connection. It may follow Finish or the session's
	// own Finish or Error.
	Cancel()
}

// Options configure a State.
type Options struct {
	// Recognizer recognizes the sessions; without it, the State starts
	// none and Button shows dictation unavailable.
	Recognizer Recognizer
	// Input records the audio; nil is Microphone().
	Input Input
	// StopTimeout is how long Stop waits for the recognizer's last
	// result before it ends the session with the transcript it has; 0 is
	// 3 seconds.
	StopTimeout time.Duration
}

// A State runs dictation sessions, one at a time: it records audio from
// its Input, hands it to its Recognizer, keeps the transcript and reports
// Events. Its methods are safe from any goroutine.
type State struct {
	o Options

	mu         sync.Mutex
	status     Status
	gen        uint64 // the session's number, which stale sinks don't match
	session    Session
	stopInput  func()
	stopTimer  func() bool
	phrases    string
	hypothesis string
	events     []Event
	services   ui.Services
	lv         levels

	// finishing tells that the session's Finish is called, after which
	// it takes no audio.
	finishing bool

	// calls are the session's method calls waiting, made one at a time,
	// in order, by the goroutine of the first (call); qmu guards them.
	qmu     sync.Mutex
	calls   []func()
	calling bool
}

// afterFunc calls f after d, unless stopped; tests replace it.
var afterFunc = func(d time.Duration, f func()) (stop func() bool) {
	return time.AfterFunc(d, f).Stop
}

// New returns a State with the options o.
func New(o Options) *State {
	if o.Input == nil {
		o.Input = Microphone()
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = 3 * time.Second
	}
	return &State{o: o}
}

// HasRecognizer reports whether the State has a Recognizer.
func (s *State) HasRecognizer() bool { return s.o.Recognizer != nil }

// Available reports whether a session can start: the State has a
// Recognizer, which is available.
func (s *State) Available() bool {
	if s.o.Recognizer == nil {
		return false
	}
	if a, ok := s.o.Recognizer.(interface{ Available() bool }); ok {
		return a.Available()
	}
	return true
}

func (s *State) format() Format {
	if f, ok := s.o.Recognizer.(interface{ Format() Format }); ok {
		return f.Format()
	}
	return DefaultFormat
}

// Status returns where the session is.
func (s *State) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Transcript returns what the session recognized so far: its phrases,
// then what the recognizer guesses the user is saying.
func (s *State) Transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phrases + s.hypothesis
}

// Events returns what happened since the last call, oldest first, and
// redraws c's window whenever something happens next. Call it in the
// view, once a frame, as the window builds; events wait for it.
func (s *State) Events(c *ui.Context) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services = c.Services()
	ev := s.events
	s.events = nil
	return ev
}

// bind redraws c's window when the session changes.
func (s *State) bind(c *ui.Context) {
	s.mu.Lock()
	s.services = c.Services()
	s.mu.Unlock()
}

// emit queues ev; the caller holds mu, and releases it with unlock.
func (s *State) emit(ev Event) {
	s.events = append(s.events, ev)
}

// unlock releases mu and redraws the window.
func (s *State) unlock() {
	sv := s.services
	s.mu.Unlock()
	sv.Invalidate()
}

// Start starts a session, unless one runs or the State can't (Available).
func (s *State) Start() {
	if !s.Available() {
		return
	}
	s.mu.Lock()
	if s.status != Idle {
		s.mu.Unlock()
		return
	}
	s.gen++
	gen := s.gen
	s.status = Connecting
	s.finishing = false
	s.phrases, s.hypothesis = "", ""
	s.lv.reset(s.format().SampleRate)
	s.emit(Event{Kind: Started})
	s.unlock()

	// The recognizer first, which keeps what the input records until it
	// is ready; neither under mu, as either may call its sink at once.
	sess, err := s.o.Recognizer.Start(&Sink{s, gen})
	if err != nil {
		s.fail(gen, err)
		return
	}
	s.mu.Lock()
	if s.gen != gen {
		// Cancelled, or failed, while starting.
		s.mu.Unlock()
		s.call(sess.Cancel)
		return
	}
	s.session = sess
	s.mu.Unlock()

	stop, err := s.o.Input.Start(s.format(), &AudioSink{s, gen})
	if err != nil {
		s.fail(gen, err)
		return
	}
	s.mu.Lock()
	if s.gen != gen || !s.status.Capturing() {
		s.mu.Unlock()
		stop()
		return
	}
	s.stopInput = stop
	s.mu.Unlock()
}

// Stop stops recording and waits for the recognizer's last result, which
// ends the session with Final; after the State's StopTimeout, it ends with
// the transcript it has.
func (s *State) Stop() {
	s.mu.Lock()
	if !s.status.Capturing() {
		s.mu.Unlock()
		return
	}
	gen := s.gen
	s.status = Stopping
	stop, sess := s.stopInput, s.session
	s.stopInput = nil
	s.stopTimer = afterFunc(s.o.StopTimeout, func() { s.finish(gen) })
	s.unlock()
	if stop != nil {
		stop()
	}
	if sess != nil {
		s.call(func() {
			s.mu.Lock()
			current := s.gen == gen
			s.finishing = current
			s.mu.Unlock()
			if current {
				sess.Finish()
			}
		})
	}
}

// Toggle starts a session when none runs, and stops one recording; it
// does nothing while one stops.
func (s *State) Toggle() {
	switch s.Status() {
	case Idle:
		s.Start()
	case Connecting, Recording:
		s.Stop()
	}
}

// Cancel ends the session at once, dropping its transcript, with
// Cancelled.
func (s *State) Cancel() {
	s.mu.Lock()
	if s.status == Idle {
		s.mu.Unlock()
		return
	}
	stop, sess := s.end()
	s.emit(Event{Kind: Cancelled})
	s.unlock()
	release(stop, sess, s)
}

// end ends the session, whose input and session the caller stops once it
// released mu, which it holds.
func (s *State) end() (stop func(), sess Session) {
	s.gen++
	s.status = Idle
	stop, sess = s.stopInput, s.session
	s.stopInput, s.session = nil, nil
	if s.stopTimer != nil {
		s.stopTimer()
		s.stopTimer = nil
	}
	s.lv.end()
	return stop, sess
}

func release(stop func(), sess Session, s *State) {
	if stop != nil {
		stop()
	}
	if sess != nil {
		s.call(sess.Cancel)
	}
}

// finish ends session gen with its transcript, as the recognizer finished
// or Stop's time ran out.
func (s *State) finish(gen uint64) {
	s.mu.Lock()
	if s.gen != gen || s.status == Idle {
		s.mu.Unlock()
		return
	}
	text := s.phrases + s.hypothesis
	stop, sess := s.end()
	s.emit(Event{Kind: Final, Text: text})
	s.unlock()
	release(stop, sess, s)
}

// fail ends session gen with err.
func (s *State) fail(gen uint64, err error) {
	s.mu.Lock()
	if s.gen != gen || s.status == Idle {
		s.mu.Unlock()
		return
	}
	stop, sess := s.end()
	s.emit(Event{Kind: Error, Err: err})
	s.unlock()
	release(stop, sess, s)
}

// call calls a method of the session, after those called before it, one
// at a time: from the calling goroutine, unless a call runs, which makes
// this one after it. A session's method may end its session (through its
// sink), which calls Cancel, without waiting on itself.
func (s *State) call(fn func()) {
	s.qmu.Lock()
	s.calls = append(s.calls, fn)
	if s.calling {
		s.qmu.Unlock()
		return
	}
	s.calling = true
	for len(s.calls) > 0 {
		fn := s.calls[0]
		s.calls = s.calls[1:]
		s.qmu.Unlock()
		fn()
		s.qmu.Lock()
	}
	s.calling = false
	s.qmu.Unlock()
}

// A Sink is how a session tells its State what it recognized, from any
// goroutine. Calls of a session the State stopped waiting for (cancelled,
// timed out, replaced) do nothing.
type Sink struct {
	s   *State
	gen uint64
}

// lock locks the State of the sink's session, and reports whether it is
// still running; if not, it is unlocked.
func (k *Sink) lock() bool {
	k.s.mu.Lock()
	if k.s.gen != k.gen || k.s.status == Idle {
		k.s.mu.Unlock()
		return false
	}
	return true
}

// Ready tells that the recognizer takes audio: Connecting turns into
// Recording.
func (k *Sink) Ready() {
	if !k.lock() {
		return
	}
	if k.s.status == Connecting {
		k.s.status = Recording
	}
	k.s.unlock()
}

// Hypothesis replaces what the recognizer guesses the user is saying, the
// phrase it hasn't settled yet.
func (k *Sink) Hypothesis(text string) {
	if !k.lock() {
		return
	}
	s := k.s
	if s.hypothesis == text {
		s.mu.Unlock()
		return
	}
	s.hypothesis = text
	s.emit(Event{Kind: Partial, Text: s.phrases + s.hypothesis})
	s.unlock()
}

// Phrase commits a phrase the recognizer settled on, after those before
// it, and clears the hypothesis. Phrases are joined as they are: include
// the space between them that the language needs.
func (k *Sink) Phrase(text string) {
	if !k.lock() {
		return
	}
	s := k.s
	s.phrases += text
	s.hypothesis = ""
	s.emit(Event{Kind: Partial, Text: s.phrases})
	s.unlock()
}

// Finish ends the session with its transcript, as Final: after Session's
// Finish, or on its own, as when the service ends the utterance.
func (k *Sink) Finish() { k.s.finish(k.gen) }

// Error ends the session with err.
func (k *Sink) Error(err error) { k.s.fail(k.gen, err) }
