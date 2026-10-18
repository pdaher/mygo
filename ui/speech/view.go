package speech

import (
	"time"

	"github.com/egoist/mygo/internal/l10n"
	"github.com/egoist/mygo/ui"
)

var (
	micIcon  = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"><rect x="5.75" y="1.5" width="4.5" height="8" rx="2.25"/><path d="M3.25 7.5a4.75 4.75 0 0 0 9.5 0M8 12.25v2.25"/></svg>`))
	stopIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor"><rect x="3.5" y="3.5" width="9" height="9" rx="2"/></svg>`))
)

// Button builds a button without a face until hovered, as a toolbar's,
// that starts and stops the State's sessions (Toggle): a microphone while
// idle, a stop sign in the accent color while recording, and a spinner,
// which takes no clicks, while the session stops. Its tooltip and its name
// for assistive technology say what a click does, in the window's
// language. Without an available recognizer it is disabled, saying that
// dictation is unavailable; leave it out where it shouldn't show:
//
//	if app.speech.HasRecognizer() {
//		speech.Button(c, app.speech)
//	}
//
// Its icon is as high as the font size: FontSize sizes it, and Disabled
// disables it for the app's own reasons.
func Button(c *ui.Context, s *State) ui.Element {
	s.bind(c)
	t := c.Theme()
	tx := l10n.In(c.Locale())
	status := s.Status()
	available := s.Available()

	label := tx.StartDictation()
	switch {
	case status == Stopping:
		label = tx.FinishingDictation()
	case status.Capturing():
		label = tx.StopDictation()
	case !available:
		label = tx.DictationUnavailable()
	}
	pad := t.Space(1)
	btn := ui.ButtonBase(c).Padding(pad).Radius(t.Space(1.5)).Label(label).Tooltip(label)
	btn.Role(ui.RoleToggleButton).Checked(status.Active())
	btn.Disabled(!available && !status.Active())
	if btn.Hovered() && (available || status.Active()) {
		btn.Background(t.SurfaceHover)
	}
	btn.Children(func() {
		switch {
		case status == Stopping:
			ui.Spinner(c)
		case status.Capturing():
			ui.Icon(c, stopIcon).TextColor(t.Accent)
		default:
			icon := ui.Icon(c, micIcon)
			if !available {
				icon.TextColor(t.TextMuted)
			}
		}
	})
	if btn.Clicked() {
		s.Toggle()
	}
	return btn
}

// Waveform builds a strip showing the input's levels (State.Levels) as
// bars, the newest at the end, scrolling by one bar every LevelInterval
// while the session records; silence shows as dots. It is 96 DIPs wide
// and as high as the font size: Width and Height size it otherwise. It
// is a decoration that assistive technology does not see.
func Waveform(c *ui.Context, s *State) ui.Element {
	s.bind(c)
	t := c.Theme()
	color := t.TextMuted
	box := ui.Box(c).Width(96).Height(t.Rem(1)).Shrink(0)
	return box.Draw(func(p *ui.Painter, r ui.Rect) {
		s.mu.Lock()
		values := append([]float32(nil), s.lv.values...)
		at, live := s.lv.at, s.lv.live
		s.mu.Unlock()
		paintWaveform(p, r, values, at, live, color)
	})
}

const (
	barWidth = 2
	barGap   = 2
	barPitch = barWidth + barGap
)

// paintWaveform paints values into r, the newest at the right edge, moved
// left by how far the next is due.
func paintWaveform(p *ui.Painter, r ui.Rect, values []float32, at time.Time, live bool, color ui.Color) {
	var shift float32
	if live {
		p.AnimationFrame()
		if !at.IsZero() {
			f := float32(p.Now().Sub(at)) / float32(LevelInterval)
			shift = min(max(f, 0), 1) * barPitch
		}
	}
	mid := r.Y + r.H/2
	p.Clip(r, 0, func() {
		x := r.X + r.W - barWidth - shift
		for i := len(values) - 1; i >= 0 && x+barWidth > r.X; i-- {
			h := values[i] * r.H
			if h < barWidth {
				// Silence: a dot.
				p.Fill(ui.Rect{X: x, Y: mid - barWidth/2, W: barWidth, H: barWidth}, color, barWidth/2)
			} else {
				p.Fill(ui.Rect{X: x, Y: mid - h/2, W: barWidth, H: h}, color, barWidth/2)
			}
			x -= barPitch
		}
	})
}
