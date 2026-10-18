// Recorder records the microphone with package audio and plays the take
// back: its waveform grows as it records, and a playhead crosses it as it
// plays, which a click on it moves. R records and stops, Space plays and
// pauses.
//
//	go run ./examples/recorder
//
// macOS asks for the microphone the first time; a packaged app needs
// NSMicrophoneUsageDescription in macos.infoPlist of mygo.config.ts.
package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/audio"
	"github.com/egoist/mygo/ui"
)

// The take is mono at rate, at most maxLength long.
const (
	rate      = 48000
	maxLength = 10 * time.Minute
)

// record starts a recording; tests replace it.
var record = func(rate, channels int, fn func([]float32, error)) stopper {
	return audio.Record(rate, channels, fn)
}

type stopper interface{ Stop() }

// take is the audio recorded, which the recording's goroutine appends to.
type take struct {
	mu      sync.Mutex
	samples []float32
	err     error
}

func (t *take) snapshot() ([]float32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.samples, t.err
}

type recorder struct {
	rec      stopper // while recording
	take     *take
	player   *audio.Player // of the last take, once recorded
	services ui.Services
}

func (r *recorder) recording() bool { return r.rec != nil }

// startRecording records a new take, dropping the last.
func (r *recorder) startRecording() {
	r.stopPlaying()
	tk := &take{}
	r.take, r.player = tk, nil
	services := r.services
	r.rec = record(rate, 1, func(samples []float32, err error) {
		tk.mu.Lock()
		if err != nil {
			tk.err = err
		} else if len(tk.samples) < int(maxLength.Seconds()*rate) {
			tk.samples = append(tk.samples, samples...)
		}
		tk.mu.Unlock()
		services.Invalidate()
	})
}

// stopRecording ends the take, ready to play.
func (r *recorder) stopRecording() {
	if r.rec == nil {
		return
	}
	r.rec.Stop()
	r.rec = nil
	samples, _ := r.take.snapshot()
	if len(samples) > 0 {
		// The sound shares the take's samples: a chunk the recording's
		// goroutine may still append goes past them, or into a new array,
		// and changes none the sound plays.
		r.player = audio.NewSound(rate, 1, samples).NewPlayer()
	}
}

func (r *recorder) togglePlaying() {
	if r.player == nil {
		return
	}
	if r.player.Playing() {
		r.player.Pause()
	} else {
		r.player.Play()
	}
}

func (r *recorder) stopPlaying() {
	if r.player != nil {
		r.player.Stop()
	}
}

func (r *recorder) view(c *ui.Context) {
	r.services = c.Services()
	t := c.Theme()
	var (
		samples []float32
		err     error
	)
	if r.take != nil {
		samples, err = r.take.snapshot()
	}
	if err != nil && r.recording() {
		// The microphone failed: the recording ended.
		r.rec.Stop()
		r.rec = nil
	}
	playing := r.player != nil && r.player.Playing()
	if playing {
		// The playhead moves, and the button turns back to Play at the end.
		c.After(30 * time.Millisecond)
	}

	if c.Shortcut(0, ui.KeyR) {
		r.toggleRecording()
	}
	if c.Shortcut(0, ui.KeySpace) && !r.recording() {
		r.togglePlaying()
	}

	ui.Column(c).Fill().Padding(24).Gap(16).Children(func() {
		ui.Row(c).Gap(14).AlignItems(ui.Center).Children(func() {
			r.recordButton(c)
			ui.Column(c).Gap(2).Grow(1).Children(func() {
				length := duration(len(samples))
				switch {
				case r.recording():
					ui.Text(c, "Recording").Bold()
					ui.Text(c, clock(length)).Font("SF Mono, Menlo, Consolas, monospace").TextColor(t.Danger)
				case r.player != nil:
					ui.Text(c, "Take").Bold()
					ui.Text(c, clock(r.player.Position())+" / "+clock(length)).Font("SF Mono, Menlo, Consolas, monospace").TextColor(t.TextMuted)
				default:
					ui.Text(c, "Ready to record").Bold()
					ui.Text(c, "Press the button or R").TextColor(t.TextMuted)
				}
			})
		})

		r.waveform(c, samples)

		ui.Row(c).Gap(8).Children(func() {
			label := "Play"
			if playing {
				label = "Pause"
			}
			if ui.PrimaryButton(c.Key("play"), label).Width(80).Disabled(r.player == nil).Clicked() {
				r.togglePlaying()
			}
			if ui.Button(c.Key("rewind"), "Rewind").Disabled(r.player == nil || r.player.Position() == 0).Clicked() {
				r.stopPlaying()
			}
			ui.Spacer(c)
			if ui.Button(c.Key("discard"), "Discard").Disabled(r.player == nil).Clicked() {
				r.stopPlaying()
				r.take, r.player = nil, nil
			}
		})

		if err != nil {
			ui.Text(c, err.Error()).TextColor(t.Danger).FontSize(12)
		}
	})
}

func (r *recorder) toggleRecording() {
	if r.recording() {
		r.stopRecording()
	} else {
		r.startRecording()
	}
}

// recordButton is a round button: a red dot to record, a square to stop.
func (r *recorder) recordButton(c *ui.Context) {
	t := c.Theme()
	label := "Record"
	if r.recording() {
		label = "Stop recording"
	}
	btn := ui.ButtonBase(c.Key("record")).Size(52, 52).Radius(26).Border(2, t.Border).Label(label).Tooltip(label)
	if btn.Hovered() {
		btn.Background(t.SurfaceHover)
	}
	recording := r.recording()
	red := t.Danger
	btn.Draw(func(p *ui.Painter, rect ui.Rect) {
		cx, cy := rect.X+rect.W/2, rect.Y+rect.H/2
		if recording {
			p.Fill(ui.Rect{X: cx - 9, Y: cy - 9, W: 18, H: 18}, red, 4)
		} else {
			p.Fill(ui.Rect{X: cx - 13, Y: cy - 13, W: 26, H: 26}, red, 13)
		}
	})
	if btn.Clicked() {
		r.toggleRecording()
	}
}

// waveform shows the take, as long as it is, and the playhead; a click
// moves the playhead there.
func (r *recorder) waveform(c *ui.Context, samples []float32) {
	t := c.Theme()
	box := ui.Box(c.Key("waveform")).Grow(1).AlignSelf(ui.Stretch).Radius(10).Background(t.Surface).Label("Waveform")
	if r.player != nil {
		box.Cursor(ui.CursorPointer)
	}
	ready := r.player != nil
	var pos float32
	if ready && r.player.Duration() > 0 {
		pos = float32(r.player.Position()) / float32(r.player.Duration())
	}
	if ready {
		x, _, over := box.PointerPosition()
		if w := box.Bounds().W; box.Clicked() && over && w > 0 {
			r.player.Seek(time.Duration(float64(min(max(x/w, 0), 1)) * float64(r.player.Duration())))
		}
	}
	bar, accent := t.TextMuted, t.Accent
	recording := r.recording()
	box.Draw(func(p *ui.Painter, rect ui.Rect) {
		cols := int(rect.W / 3)
		// While recording, the take fills the box up to 30 seconds, then
		// shows its last 30 seconds; recorded, all of it.
		shown := samples
		if recording {
			if limit := 30 * rate; len(shown) > limit {
				shown = shown[len(shown)-limit:]
			}
			cols = max(min(cols, len(shown)*cols/(30*rate)), 0)
		}
		mid := rect.Y + rect.H/2
		for i, peak := range peaks(shown, cols) {
			h := max(peak*(rect.H-16), 2)
			color := bar
			if ready && float32(i)/float32(max(cols, 1)) < pos {
				color = accent
			}
			p.Fill(ui.Rect{X: rect.X + float32(i)*3 + 1, Y: mid - h/2, W: 2, H: h}, color, 1)
		}
		if ready {
			x := rect.X + pos*rect.W
			p.Fill(ui.Rect{X: x - 1, Y: rect.Y + 4, W: 2, H: rect.H - 8}, accent, 1)
		}
	})
}

// peaks returns the loudest of samples in each of n columns, scaled so
// that speech fills them: in decibels from -48 dB.
func peaks(samples []float32, n int) []float32 {
	if n <= 0 || len(samples) == 0 {
		return nil
	}
	out := make([]float32, n)
	for i := range out {
		from, to := i*len(samples)/n, (i+1)*len(samples)/n
		peak := float32(0)
		for _, s := range samples[from:max(to, from)] {
			peak = max(peak, float32(math.Abs(float64(s))))
		}
		if peak > 0 {
			db := 20 * math.Log10(float64(peak))
			out[i] = float32(min(max((db+48)/48, 0), 1))
		}
	}
	return out
}

func duration(frames int) time.Duration {
	return time.Duration(frames) * time.Second / rate
}

// clock shows d as minutes, seconds and tenths: 1:05.3.
func clock(d time.Duration) string {
	tenths := int(d / (100 * time.Millisecond))
	return fmt.Sprintf("%d:%02d.%d", tenths/600, tenths/10%60, tenths%10)
}

func main() {
	r := &recorder{}
	mygo.App.WhenReady(func() {
		mygo.NewWindow(mygo.WindowOptions{
			Title:   "Recorder",
			Width:   520,
			Height:  340,
			Content: ui.View(r.view),
		})
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
