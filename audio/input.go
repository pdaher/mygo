package audio

import (
	"errors"
	"math"
	"runtime"
	"sync"
	"time"
)

// Devices capture in mono at inputRate; recordings convert it to the format
// asked for.
const inputRate = 48000

// An inputDevice is the system's default microphone, opened by
// openInputDevice.
type inputDevice interface {
	// read waits for the next samples, mono at inputRate, and returns
	// them, valid until the next read; nil if none came for about 100 ms.
	// An error closes the device, and the recording opens it again.
	read() ([]float32, error)
	close()
}

var (
	openInput = openInputDevice // tests replace it

	// inputStall is how long a device may give nothing, counted in reads
	// that waited 100 ms, before it counts as gone.
	inputStall = 2 * time.Second
)

var errUnsupportedInput = errors.New("audio: no audio input on this platform")

var errInputStalled = errors.New("audio: the microphone stopped giving audio")

// A Recording captures the system's default microphone, from Record until
// Stop.
type Recording struct {
	stop chan struct{}
	once sync.Once
	done chan struct{}
}

// Record starts recording the system's default microphone and returns
// at once. It calls fn on a goroutine of its own with each chunk of
// samples, every 10 to 21 ms: interleaved float32 frames in [-1, 1],
// rate frames a second of channels (1 for mono, 2 for the same sound left
// and right), converted from what the microphone captures. fn may keep
// samples.
//
// Where the microphone can't open, or goes and no other takes its place,
// fn is called once with nil samples and the error, and the recording
// ends. When the default microphone changes, the recording follows it.
//
// The system may ask the user first: macOS ends an app without
// NSMicrophoneUsageDescription in its Info.plist (macos.infoPlist of
// mygo.config.ts), and browsers ask for the page.
func Record(rate, channels int, fn func(samples []float32, err error)) *Recording {
	if channels != 1 && channels != 2 {
		panic("audio: Record takes 1 or 2 channels")
	}
	if rate <= 0 {
		panic("audio: Record takes a positive sample rate")
	}
	r := &Recording{stop: make(chan struct{}), done: make(chan struct{})}
	go r.run(newConverter(rate, channels), fn)
	return r
}

// Stop ends the recording and frees the microphone. It doesn't wait for
// a call of fn under way, which may be the caller; fn is not called after
// that one.
func (r *Recording) Stop() {
	r.once.Do(func() { close(r.stop) })
}

func (r *Recording) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

func (r *Recording) run(cv *converter, fn func([]float32, error)) {
	defer close(r.done)
	// Devices that need it, as COM's, stay on one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	dev, err := openInput()
	if err != nil {
		if !r.stopped() {
			fn(nil, err)
		}
		return
	}
	// got tells that the device gave audio since it opened.
	got, quiet := false, time.Duration(0)
	for !r.stopped() {
		in, err := dev.read()
		if err == nil && in == nil {
			if quiet += 100 * time.Millisecond; quiet >= inputStall {
				err = errInputStalled
			}
		}
		if err != nil {
			dev.close()
			// The microphone went, or the default changed: open the
			// default again, unless this one never worked.
			if !got && err != errDefaultInputChanged {
				if !r.stopped() {
					fn(nil, err)
				}
				return
			}
			if dev, err = openInput(); err != nil {
				if !r.stopped() {
					fn(nil, err)
				}
				return
			}
			got, quiet = false, 0
			continue
		}
		if len(in) == 0 {
			continue
		}
		got, quiet = true, 0
		if out := cv.convert(in); len(out) > 0 && !r.stopped() {
			fn(out, nil)
		}
	}
	dev.close()
}

// errDefaultInputChanged tells the recording to open the new default
// microphone, however soon after opening the last.
var errDefaultInputChanged = errors.New("audio: the default input changed")

// A converter turns mono at inputRate into rate frames a second of
// channels: low-pass filtered when fewer, then interpolated.
type converter struct {
	rate, channels int
	step           float64 // input frames per output frame
	pos            float64 // of the next output frame, from prev
	prev           float32 // the last input frame converted
	lp             []biquad
	tmp            []float32
}

func newConverter(rate, channels int) *converter {
	cv := &converter{rate: rate, channels: channels, step: float64(inputRate) / float64(rate)}
	if rate < inputRate {
		// An eighth-order Butterworth below the new Nyquist frequency,
		// so that what is above it doesn't fold back into what is heard.
		fc := 0.45 * float64(rate) / inputRate
		cv.lp = []biquad{lowPass(fc, 0.5098), lowPass(fc, 0.6013), lowPass(fc, 0.9000), lowPass(fc, 2.5629)}
	}
	return cv
}

func (cv *converter) convert(in []float32) []float32 {
	if cv.rate == inputRate {
		out := make([]float32, 0, len(in)*cv.channels)
		for _, s := range in {
			for range cv.channels {
				out = append(out, s)
			}
		}
		return out
	}
	if cv.lp != nil {
		cv.tmp = append(cv.tmp[:0], in...)
		for i := range cv.lp {
			cv.lp[i].filter(cv.tmp)
		}
		in = cv.tmp
	}
	// Input frames are numbered from prev, 0, to the last of in, len(in).
	at := func(i int) float32 {
		if i == 0 {
			return cv.prev
		}
		return in[i-1]
	}
	n := len(in)
	out := make([]float32, 0, int(float64(n)/cv.step+2)*cv.channels)
	for cv.pos < float64(n) {
		i := int(cv.pos)
		f := float32(cv.pos - float64(i))
		s := at(i) + f*(at(i+1)-at(i))
		for range cv.channels {
			out = append(out, s)
		}
		cv.pos += cv.step
	}
	cv.pos -= float64(n)
	if n > 0 {
		cv.prev = in[n-1]
	}
	return out
}

// A biquad is a filter of two poles and two zeros, in direct form I.
type biquad struct {
	b0, b1, b2, a1, a2 float64
	x1, x2, y1, y2     float64
}

// lowPass returns RBJ's low-pass biquad at fc, a fraction of the sample
// rate, of quality q.
func lowPass(fc, q float64) biquad {
	w := 2 * math.Pi * fc
	alpha := math.Sin(w) / (2 * q)
	cos := math.Cos(w)
	a0 := 1 + alpha
	return biquad{
		b0: (1 - cos) / 2 / a0,
		b1: (1 - cos) / a0,
		b2: (1 - cos) / 2 / a0,
		a1: -2 * cos / a0,
		a2: (1 - alpha) / a0,
	}
}

func (f *biquad) filter(s []float32) {
	for i, v := range s {
		x := float64(v)
		y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
		f.x2, f.x1 = f.x1, x
		f.y2, f.y1 = f.y1, y
		s[i] = float32(y)
	}
}
