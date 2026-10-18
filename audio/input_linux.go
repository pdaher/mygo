//go:build linux && (amd64 || arm64)

package audio

import (
	"errors"
	"fmt"
	"unsafe"
)

// Linux records through PulseAudio's simple API, which PipeWire serves as
// well, or else ALSA's default device, each read blocking for 10 ms of
// audio, in mono float32 at inputRate, which both convert to.

const (
	linuxInputFrames = inputRate / 100 // per read: 10 ms

	paStreamRecord       = 2
	sndPcmStreamCapture  = 1
	alsaInputLatencyUsec = 40000
)

func openInputDevice() (inputDevice, error) {
	d, perr := openPulseInput()
	if perr == nil {
		return d, nil
	}
	d, aerr := openALSAInput()
	if aerr == nil {
		return d, nil
	}
	return nil, errors.Join(perr, aerr)
}

type pulseInput struct {
	s   uintptr
	buf []float32
}

func openPulseInput() (inputDevice, error) {
	if err := loadPulse(); err != nil {
		return nil, err
	}
	spec := paSampleSpec{format: paSampleFloat32LE, rate: inputRate, channels: 1}
	// Handed over every 10 ms.
	attr := paBufferAttr{
		maxLength: ^uint32(0),
		tLength:   ^uint32(0),
		preBuf:    ^uint32(0),
		minReq:    ^uint32(0),
		fragSize:  linuxInputFrames * 4,
	}
	var code int32
	name := appName()
	s := paSimpleNew(nil, cstr(name), paStreamRecord, nil, cstr(name), &spec, nil, unsafe.Pointer(&attr), &code)
	if s == 0 {
		return nil, fmt.Errorf("audio: PulseAudio: %s", paStrerror(code))
	}
	return &pulseInput{s: s, buf: make([]float32, linuxInputFrames)}, nil
}

func (d *pulseInput) read() ([]float32, error) {
	var code int32
	if paSimpleRead(d.s, unsafe.Pointer(&d.buf[0]), uintptr(len(d.buf)*4), &code) < 0 {
		return nil, fmt.Errorf("audio: PulseAudio: %s", paStrerror(code))
	}
	return d.buf, nil
}

func (d *pulseInput) close() { paSimpleFree(d.s) }

type alsaInput struct {
	pcm uintptr
	buf []float32
}

func openALSAInput() (inputDevice, error) {
	if err := loadALSA(); err != nil {
		return nil, err
	}
	var pcm uintptr
	if e := sndPcmOpen(&pcm, cstr("default"), sndPcmStreamCapture, 0); e < 0 {
		return nil, fmt.Errorf("audio: ALSA: %s", sndStrerror(e))
	}
	if e := sndPcmSetParams(pcm, sndPcmFormatFloatLE, sndPcmAccessRWInterleaved, 1, inputRate, 1, alsaInputLatencyUsec); e < 0 {
		sndPcmClose(pcm)
		return nil, fmt.Errorf("audio: ALSA: %s", sndStrerror(e))
	}
	return &alsaInput{pcm: pcm, buf: make([]float32, linuxInputFrames)}, nil
}

func (d *alsaInput) read() ([]float32, error) {
	for {
		n := sndPcmReadi(d.pcm, unsafe.Pointer(&d.buf[0]), uintptr(len(d.buf)))
		if n >= 0 {
			return d.buf[:n], nil
		}
		// An overrun or a suspend, recovered from by preparing the
		// device again.
		if e := sndPcmRecover(d.pcm, int32(n), 1); e < 0 {
			return nil, fmt.Errorf("audio: ALSA: %s", sndStrerror(e))
		}
	}
}

func (d *alsaInput) close() { sndPcmClose(d.pcm) }
