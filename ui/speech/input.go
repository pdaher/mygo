package speech

import (
	"math"
	"time"

	"github.com/egoist/mygo/audio"
)

// An Input records the audio of sessions. Start starts recording in
// format f, handing the audio to sink from any goroutine until stop is
// called; it is called on the main thread and returns at once.
type Input interface {
	Start(f Format, sink *AudioSink) (stop func(), err error)
}

// An AudioSink takes the audio an Input records for a session. Calls
// after the session ended do nothing.
type AudioSink struct {
	s   *State
	gen uint64
}

// Push hands the next samples to the session, interleaved in the Format
// the Input started with. The session may keep them.
func (a *AudioSink) Push(samples []int16) {
	s := a.s
	s.mu.Lock()
	if s.gen != a.gen || !s.status.Capturing() {
		s.mu.Unlock()
		return
	}
	// The Waveform paints the levels every frame while recording, and
	// needs no build.
	s.lv.push(samples, s.format().Channels)
	s.mu.Unlock()
	s.call(func() {
		// Checked again as the call comes, which may be after Finish or
		// another session.
		s.mu.Lock()
		sess := s.session
		ok := s.gen == a.gen && !s.finishing && sess != nil
		s.mu.Unlock()
		if ok {
			sess.PushAudio(samples)
		}
	})
}

// Error ends the session with err, as when the microphone can't open.
func (a *AudioSink) Error(err error) { a.s.fail(a.gen, err) }

// Microphone returns the Input of the system's default microphone, which
// records with package audio. The system may ask the user first: macOS
// ends an app without NSMicrophoneUsageDescription in its Info.plist
// (macos.infoPlist of mygo.config.ts), and browsers ask for the page.
func Microphone() Input { return microphone{} }

type microphone struct{}

func (microphone) Start(f Format, sink *AudioSink) (func(), error) {
	r := audio.Record(f.SampleRate, f.Channels, func(samples []float32, err error) {
		if err != nil {
			sink.Error(err)
			return
		}
		sink.Push(PCM16(samples))
	})
	return r.Stop, nil
}

// PCM16 converts float samples in [-1, 1] to signed 16-bit ones, clipping
// those beyond.
func PCM16(samples []float32) []int16 {
	out := make([]int16, len(samples))
	for i, v := range samples {
		out[i] = int16(math.Round(float64(min(max(v, -1), 1)) * 32767))
	}
	return out
}

// LevelInterval is how often a State measures the input's level: one
// bar of a Waveform each.
const LevelInterval = 80 * time.Millisecond

const (
	maxLevels = 256
	// Levels are of the peak, in decibels from floorDB, under which is
	// silence, as background noise, to 0 dB, the loudest.
	floorDB = -50.0
	gateDB  = -42.0
	// fall is what a level keeps of the one before it, at most: peaks
	// rise at once and fall smoothly.
	fall = 0.7
)

// levels measures the input's level, LevelInterval at a time. The State's
// mu guards it.
type levels struct {
	values []float32 // oldest first
	window int       // frames an interval
	n      int       // frames of the interval so far
	peak   float64   // of the interval so far
	last   float32
	at     time.Time // when the newest value came
	live   bool      // the session records
}

func (l *levels) reset(rate int) {
	l.values = l.values[:0]
	l.window = max(int(int64(rate)*int64(LevelInterval)/int64(time.Second)), 1)
	l.n, l.peak, l.last, l.at = 0, 0, 0, time.Time{}
	l.live = true
}

func (l *levels) end() { l.live = false }

// push measures samples, interleaved frames of ch channels.
func (l *levels) push(samples []int16, ch int) {
	ch = max(ch, 1)
	for i := 0; i+ch <= len(samples); i += ch {
		for _, v := range samples[i : i+ch] {
			l.peak = max(l.peak, math.Abs(float64(v))/32768)
		}
		if l.n++; l.n >= l.window {
			l.add(level(l.peak))
			l.n, l.peak = 0, 0
		}
	}
}

func (l *levels) add(v float32) {
	v = max(v, l.last*fall)
	if v < 0.02 {
		v = 0
	}
	l.last = v
	if len(l.values) == maxLevels {
		copy(l.values, l.values[1:])
		l.values = l.values[:maxLevels-1]
	}
	l.values = append(l.values, v)
	l.at = time.Now()
}

// level maps a peak in [0, 1] to a level in [0, 1]: 0 below the gate.
func level(peak float64) float32 {
	if peak <= 0 {
		return 0
	}
	db := 20 * math.Log10(peak)
	if db < gateDB {
		return 0
	}
	return float32(min((db-floorDB)/-floorDB, 1))
}

// Levels returns the input's recent levels, oldest first, each in [0, 1]
// over LevelInterval: peaks rise at once and fall smoothly, and
// background noise is 0. They stay after a session, until the next.
func (s *State) Levels() []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]float32(nil), s.lv.values...)
}
