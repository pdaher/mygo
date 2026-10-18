package audio

import (
	"errors"
	"fmt"
	"syscall/js"
	"time"
	"unsafe"
)

// Browsers record through getUserMedia, which asks the user for the page,
// into an AudioContext of its own at inputRate, which resamples the
// microphone; an AudioWorklet posts its first channel back in chunks.

const inputWorklet = `
class MyGoInput extends AudioWorkletProcessor {
	constructor() {
		super();
		this.buf = new Float32Array(%d);
		this.n = 0;
	}
	process(inputs) {
		const ch = inputs[0] && inputs[0][0];
		if (!ch) return true;
		for (let i = 0; i < ch.length; i++) {
			this.buf[this.n++] = ch[i];
			if (this.n === this.buf.length) {
				this.port.postMessage(this.buf, [this.buf.buffer]);
				this.buf = new Float32Array(this.buf.length);
				this.n = 0;
			}
		}
		return true;
	}
}
registerProcessor("mygo-input", MyGoInput);
`

const jsInputFrames = 1024 // per chunk: 21 ms

type jsInput struct {
	ctx, stream js.Value
	onMessage   js.Func
	chunks      chan []float32
}

// await waits for promise, on a goroutine other than the one JavaScript
// calls Go on.
func await(promise js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	done := make(chan result, 1)
	var ok, fail js.Func
	ok = js.FuncOf(func(this js.Value, args []js.Value) any {
		v := js.Undefined()
		if len(args) > 0 {
			v = args[0]
		}
		done <- result{v: v}
		return nil
	})
	fail = js.FuncOf(func(this js.Value, args []js.Value) any {
		msg := "unknown error"
		if len(args) > 0 {
			msg = js.Global().Get("String").Invoke(args[0]).String()
		}
		done <- result{err: errors.New(msg)}
		return nil
	})
	promise.Call("then", ok, fail)
	r := <-done
	ok.Release()
	fail.Release()
	return r.v, r.err
}

func openInputDevice() (inputDevice, error) {
	devices := js.Global().Get("navigator").Get("mediaDevices")
	if !devices.Truthy() || !devices.Get("getUserMedia").Truthy() {
		return nil, errors.New("audio: this page can't use the microphone (it needs https or localhost)")
	}
	class := js.Global().Get("AudioContext")
	if !class.Truthy() {
		return nil, errors.New("audio: this browser has no AudioContext")
	}
	stream, err := await(devices.Call("getUserMedia", map[string]any{
		"audio": map[string]any{"channelCount": 1, "echoCancellation": true, "noiseSuppression": true},
	}))
	if err != nil {
		return nil, fmt.Errorf("audio: the microphone: %v", err)
	}
	d := &jsInput{stream: stream, chunks: make(chan []float32, 16)}
	d.ctx = class.New(map[string]any{"sampleRate": inputRate, "latencyHint": "interactive"})
	if !d.ctx.Get("audioWorklet").Truthy() {
		d.close()
		return nil, errors.New("audio: this browser has no AudioWorklet")
	}
	blob := js.Global().Get("Blob").New([]any{fmt.Sprintf(inputWorklet, jsInputFrames)}, map[string]any{"type": "text/javascript"})
	url := js.Global().Get("URL").Call("createObjectURL", blob)
	_, err = await(d.ctx.Get("audioWorklet").Call("addModule", url))
	js.Global().Get("URL").Call("revokeObjectURL", url)
	if err != nil {
		d.close()
		return nil, fmt.Errorf("audio: loading the AudioWorklet failed: %v", err)
	}
	src := d.ctx.Call("createMediaStreamSource", stream)
	node := js.Global().Get("AudioWorkletNode").New(d.ctx, "mygo-input", map[string]any{
		"numberOfInputs":  1,
		"numberOfOutputs": 0,
	})
	d.onMessage = js.FuncOf(func(this js.Value, args []js.Value) any {
		a := args[0].Get("data")
		chunk := make([]float32, a.Get("length").Int())
		js.CopyBytesToGo(unsafe.Slice((*byte)(unsafe.Pointer(&chunk[0])), len(chunk)*4), js.Global().Get("Uint8Array").New(a.Get("buffer")))
		select {
		case d.chunks <- chunk:
		default:
		}
		return nil
	})
	node.Get("port").Set("onmessage", d.onMessage)
	src.Call("connect", node)
	// The click that started the recording lets the context run.
	d.ctx.Call("resume")
	return d, nil
}

func (d *jsInput) read() ([]float32, error) {
	select {
	case c := <-d.chunks:
		return c, nil
	case <-time.After(100 * time.Millisecond):
		return nil, nil
	}
}

func (d *jsInput) close() {
	if d.stream.Truthy() {
		tracks := d.stream.Call("getTracks")
		for i := range tracks.Length() {
			tracks.Index(i).Call("stop")
		}
	}
	if d.ctx.Truthy() {
		d.ctx.Call("close")
	}
	if d.onMessage.Truthy() {
		d.onMessage.Release()
	}
}
