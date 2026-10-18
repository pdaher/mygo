# Speech

Package `ui/speech` turns what the user says into text, for dictation. A
`speech.State` runs a session: it records the microphone, hands the audio
to a recognizer of your choice (a speech service in the cloud, or a model
on the device), keeps the transcript and reports events. `speech.Button`
starts and stops it, and `speech.Waveform` shows the microphone's level as
it records.

```go
import "github.com/egoist/mygo/ui/speech"

type app struct {
	note   string
	speech *speech.State
}

func (a *app) view(c *ui.Context) {
	for _, ev := range a.speech.Events(c) {
		if ev.Kind == speech.Final {
			a.note += ev.Text
		}
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.TextInput(c, &a.note).Grow(1)
		if a.speech.Status().Capturing() {
			speech.Waveform(c, a.speech)
		}
		speech.Button(c, a.speech)
	})
}

a := &app{speech: speech.New(speech.Options{Recognizer: newCloudRecognizer(client)})}
```

The state never edits text itself. `Partial` events carry the transcript
as the user speaks, for a preview; commit only `Final`.

## Sessions

`Start`, `Stop`, `Cancel` and `Toggle` drive a session, from any goroutine.
`Status` says where it is:

| status | the microphone | the recognizer |
|---|---|---|
| `Idle` | off | no session |
| `Connecting` | records | gets ready, keeping the audio until then |
| `Recording` | records | recognizes |
| `Stopping` | off | sends its last result |

`Status().Active()` is true for all but `Idle`, and `Capturing()` for
`Connecting` and `Recording`. `Start` does nothing while a session runs.
`Stop` waits for the recognizer's last result and ends the session with
`Final`. If the result doesn't come within `Options.StopTimeout` (3
seconds), the session ends with the transcript it has. `Cancel` ends it at
once with `Cancelled`, dropping the transcript.

`Events(c)` returns what happened since its last call, oldest first, and
redraws the window when something happens next. Call it once a frame, in
the view:

| event | when |
|---|---|
| `Started` | a session started |
| `Partial` | the transcript changed; `Text` is all of it so far |
| `Final` | the session ended with `Text` |
| `Cancelled` | `Cancel` ended it |
| `Error` | the recognizer or the microphone failed, with `Err` |

`Transcript` returns the transcript at any time, and `Levels` the
microphone's recent levels.

## Recognizers

A recognizer wraps the service that understands speech. `Start` begins a
session and returns at once. The session connects in the background,
keeps the audio it gets until it is ready, and tells its `Sink` what it
recognizes:

```go
type cloudRecognizer struct{ client *speechapi.Client }

func (r *cloudRecognizer) Start(sink *speech.Sink) (speech.Session, error) {
	s := &cloudSession{sink: sink, audio: make(chan []int16, 64)}
	go s.run(r.client)
	return s, nil
}

type cloudSession struct {
	sink  *speech.Sink
	audio chan []int16
	done  sync.Once
}

func (s *cloudSession) PushAudio(samples []int16) { s.audio <- samples }
func (s *cloudSession) Finish()                   { s.done.Do(func() { close(s.audio) }) }
func (s *cloudSession) Cancel()                   { s.Finish() /* and drop the connection */ }

func (s *cloudSession) run(client *speechapi.Client) {
	stream, err := client.Stream()
	if err != nil {
		s.sink.Error(err)
		return
	}
	s.sink.Ready()
	go func() {
		for samples := range s.audio {
			stream.Send(samples)
		}
		stream.CloseSend()
	}()
	for res := range stream.Results() {
		if res.Final {
			s.sink.Phrase(res.Text + " ")
		} else {
			s.sink.Hypothesis(res.Text)
		}
	}
	s.sink.Finish()
}
```

| `Sink` | what it does |
|---|---|
| `Ready()` | the service takes audio: `Connecting` turns into `Recording` |
| `Hypothesis(text)` | replaces the phrase the service is still guessing |
| `Phrase(text)` | commits a phrase and clears the hypothesis |
| `Finish()` | ends the session with `Final` |
| `Error(err)` | ends the session with `Error` |

Phrases are joined as they are, so include the spaces your language
needs. Calls from a session the state no longer waits for (one cancelled,
timed out or replaced) do nothing. The session's methods are called one
at a time, from any goroutine. `PushAudio` gets signed 16-bit samples, 16
kHz mono unless the recognizer has a `Format() speech.Format` method. A
recognizer with an `Available() bool` method can say it can't work yet, as
before sign-in. The button calls it every frame, so keep it cheap.

Package `speech` comes with no recognizer of its own: the system's
(`SFSpeechRecognizer`, `Windows.Media.SpeechRecognition`) are not wrapped.

## Input

`Options.Input` records the audio. The default, `speech.Microphone()`,
records the system's default microphone with
[`audio.Record`](../audio.md#recording), converted to the recognizer's
format. An `Input` of your own hands any audio to its `AudioSink`, as a
file in tests:

```go
func (f fileInput) Start(format speech.Format, sink *speech.AudioSink) (stop func(), err error) {
	go func() {
		for chunk := range f.chunks(format) {
			sink.Push(chunk)
		}
	}()
	return func() {}, nil
}
```

The system may ask the user before the microphone records. macOS ends an
app without a usage description, so add one in `mygo.config.ts`:

```ts
macos: {
	infoPlist: {
		NSMicrophoneUsageDescription: "Dictate notes.",
	},
},
```

Browsers ask for the page, which must be served over https or from
localhost.

## The button and the waveform

`speech.Button` has no face until hovered, as a toolbar's buttons. It
shows a microphone while idle, a stop sign in the accent color while
recording, and a spinner while the session stops, when it takes no
clicks. Without an available recognizer, it is disabled and says that
dictation is unavailable. Leave it out where it shouldn't show:

```go
if app.speech.HasRecognizer() {
	speech.Button(c, app.speech).FontSize(12)
}
```

Its icon is as high as the font size. `Disabled(true)` disables it for
reasons of your own.

`speech.Waveform` shows the levels as bars, the newest at the end,
scrolling by one bar every 80 ms while recording. Silence shows as dots.
It is 96 DIPs wide and as high as the font size; `Width` and `Height`
change that.

## Accessibility

Assistive technology sees the button as a toggle button, pressed while a
session runs, named by what a click does ("Start dictation", "Stop
dictation") in the window's language. It does not see the waveform.
