package main

import (
	"errors"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// fakeRecord replaces the microphone with fn, which the test calls with
// the audio; tests play nothing, which would sound on the machine.
func fakeRecord(t *testing.T) *func([]float32, error) {
	var fn func([]float32, error)
	old := record
	record = func(rate, channels int, f func([]float32, error)) stopper {
		if rate != 48000 || channels != 1 {
			t.Errorf("records %d Hz, %d channels", rate, channels)
		}
		fn = f
		return fakeRecording{}
	}
	t.Cleanup(func() { record = old })
	return &fn
}

type fakeRecording struct{}

func (fakeRecording) Stop() {}

func TestRecordAndSeek(t *testing.T) {
	push := fakeRecord(t)
	r := &recorder{}
	tt := ui.NewTester(r.view, 520, 340)
	if !tt.HasText("Ready to record") {
		t.Fatalf("texts %q", tt.Texts())
	}
	if err := tt.Click("Record"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	// Two seconds of a tone, getting louder.
	for i := range 200 {
		chunk := make([]float32, 480)
		for j := range chunk {
			chunk[j] = float32(i) / 200 * float32(1-2*(j%2))
		}
		(*push)(chunk, nil)
	}
	if !tt.FramePending() {
		t.Error("audio coming doesn't redraw the window")
	}
	tt.Frame()
	if !tt.HasText("Recording") || !tt.HasText("0:02.0") {
		t.Fatalf("while recording: %q", tt.Texts())
	}
	// R stops, as the button does.
	tt.Key(0, ui.KeyR)
	tt.Frame()
	if r.recording() || r.player == nil || r.player.Duration() != 2*time.Second {
		t.Fatalf("after stopping: recording %v, player %v", r.recording(), r.player)
	}
	if !tt.HasText("0:00.0 / 0:02.0") {
		t.Errorf("texts %q", tt.Texts())
	}
	// A click three quarters across the waveform moves there.
	w, ok := tt.Find("Waveform")
	if !ok {
		t.Fatal("no waveform")
	}
	tt.ClickAt(w.X+w.W*3/4, w.Y+w.H/2)
	tt.Frame()
	if got := r.player.Position(); got < 1400*time.Millisecond || got > 1600*time.Millisecond {
		t.Errorf("after a click: position %v, want 1.5 s", got)
	}
	if err := tt.Click("Discard"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if r.player != nil || !tt.HasText("Ready to record") {
		t.Errorf("after discarding: %q", tt.Texts())
	}
}

// A microphone failing ends the recording and says why.
func TestMicrophoneFails(t *testing.T) {
	push := fakeRecord(t)
	r := &recorder{}
	tt := ui.NewTester(r.view, 520, 340)
	tt.Click("Record")
	tt.Frame()
	(*push)(nil, errors.New("audio: no audio input"))
	tt.Frame()
	if r.recording() || !tt.HasText("audio: no audio input") {
		t.Errorf("recording %v, texts %q", r.recording(), tt.Texts())
	}
}

func TestPeaks(t *testing.T) {
	p := peaks([]float32{0, 0, 1, -1, 0.01, 0}, 3)
	if p[0] != 0 || p[1] != 1 || p[2] < 0.16 || p[2] > 0.17 { // -40 dB
		t.Errorf("peaks %v", p)
	}
	if peaks(nil, 4) != nil || peaks([]float32{1}, 0) != nil {
		t.Error("peaks of nothing")
	}
	if got := clock(65*time.Second + 340*time.Millisecond); got != "1:05.3" {
		t.Errorf("clock = %q", got)
	}
}
