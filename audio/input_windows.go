//go:build windows && (amd64 || arm64)

package audio

import (
	"errors"
	"fmt"
	"time"
	"unsafe"
)

// Windows records through WASAPI in shared mode on the default input,
// converted to mono float32 at inputRate by WASAPI's resampler, opened
// again when the default input changes or the device goes. Every call
// runs on the recording's thread, locked, in COM's multithreaded
// apartment.

var (
	iidIAudioCaptureClient = guid{0xc8adbd64, 0xe71e, 0x48a0, [8]byte{0xa4, 0xde, 0x18, 0x5c, 0x39, 0x5c, 0xd3, 0x17}}
	procCoUninitialize     = ole32.NewProc("CoUninitialize")
	errNoInput             = errors.New("audio: no audio input")
)

const (
	eCapture                 = 1
	speakerFrontCenter       = 0x4
	audclntBufferFlagsSilent = 0x2
)

type wasapiInput struct {
	enumerator, client, capture uintptr
	event                       uintptr
	com                         bool // CoInitializeEx succeeded on this thread
	id                          string
	checked                     time.Time
	buf                         []float32
}

func openInputDevice() (inputDevice, error) {
	d := &wasapiInput{}
	// S_FALSE, already initialized, succeeds as well; each success is
	// undone by a CoUninitialize.
	if hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded); failed(hr) {
		return nil, hresult("CoInitializeEx", hr)
	}
	d.com = true
	if err := d.open(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

func (d *wasapiInput) open() error {
	if hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&d.enumerator))); failed(hr) {
		return hresult("CoCreateInstance(MMDeviceEnumerator)", hr)
	}
	var dev uintptr
	if hr := comCall(d.enumerator, 4, eCapture, eConsole, uintptr(unsafe.Pointer(&dev))); failed(hr) {
		if uint32(hr) == 0x80070490 { // E_NOTFOUND
			return errNoInput
		}
		return hresult("GetDefaultAudioEndpoint", hr)
	}
	defer release(dev)
	id, err := defaultID(d.enumerator, eCapture)
	if err != nil {
		return err
	}
	d.id = id
	if hr := comCall(dev, 3, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&d.client))); failed(hr) {
		if uint32(hr) == 0x80070005 { // E_ACCESSDENIED
			return errors.New("audio: the microphone is turned off for apps in Windows' privacy settings")
		}
		return hresult("IMMDevice.Activate", hr)
	}
	f := waveFormatExtensible{
		formatTag:      waveFormatExtensibleTag,
		channels:       1,
		samplesPerSec:  inputRate,
		avgBytesPerSec: inputRate * 4,
		blockAlign:     4,
		bitsPerSample:  32,
		size:           22,
		validBits:      32,
		channelMask:    speakerFrontCenter,
		subFormat:      ksDataFormatSubtypeFloat,
	}
	const duration = 40 * 10000 // 40 ms, in 100 ns
	flags := uintptr(audclntStreamFlagsEventCB | audclntStreamFlagsNoPersist | audclntStreamFlagsAutoConvert | audclntStreamFlagsSRCQuality)
	if hr := comCall(d.client, 3, audclntShareModeShared, flags, duration, 0, uintptr(unsafe.Pointer(&f)), 0); failed(hr) {
		return hresult("IAudioClient.Initialize", hr)
	}
	if hr := comCall(d.client, 14, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&d.capture))); failed(hr) {
		return hresult("IAudioClient.GetService", hr)
	}
	ev, _, err := procCreateEventW.Call(0, 0, 0, 0)
	if ev == 0 {
		return fmt.Errorf("audio: CreateEvent failed: %v", err)
	}
	d.event = ev
	if hr := comCall(d.client, 13, d.event); failed(hr) {
		return hresult("IAudioClient.SetEventHandle", hr)
	}
	if hr := comCall(d.client, 10); failed(hr) {
		return hresult("IAudioClient.Start", hr)
	}
	d.checked = time.Now()
	return nil
}

func (d *wasapiInput) read() ([]float32, error) {
	if r, _, _ := procWaitForSingleObject.Call(d.event, 100); r != waitObject0 {
		return nil, d.check()
	}
	d.buf = d.buf[:0]
	for {
		var n uint32
		if hr := comCall(d.capture, 5, uintptr(unsafe.Pointer(&n))); failed(hr) {
			return nil, hresult("IAudioCaptureClient.GetNextPacketSize", hr)
		}
		if n == 0 {
			break
		}
		var (
			p     unsafe.Pointer
			flags uint32
		)
		if hr := comCall(d.capture, 3, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&flags)), 0, 0); failed(hr) {
			return nil, hresult("IAudioCaptureClient.GetBuffer", hr)
		}
		if flags&audclntBufferFlagsSilent != 0 {
			for range n {
				d.buf = append(d.buf, 0)
			}
		} else {
			d.buf = append(d.buf, unsafe.Slice((*float32)(p), n)...)
		}
		if hr := comCall(d.capture, 4, uintptr(n)); failed(hr) {
			return nil, hresult("IAudioCaptureClient.ReleaseBuffer", hr)
		}
	}
	return d.buf, d.check()
}

// check reports a change of the default input, which moves a shared
// stream nowhere, every 500 ms.
func (d *wasapiInput) check() error {
	if time.Since(d.checked) < 500*time.Millisecond {
		return nil
	}
	d.checked = time.Now()
	if id, err := defaultID(d.enumerator, eCapture); err != nil || id != d.id {
		return errDefaultInputChanged
	}
	return nil
}

func (d *wasapiInput) close() {
	if d.client != 0 {
		comCall(d.client, 11)
	}
	release(d.capture)
	release(d.client)
	release(d.enumerator)
	d.capture, d.client, d.enumerator = 0, 0, 0
	if d.event != 0 {
		procCloseHandle.Call(d.event)
		d.event = 0
	}
	if d.com {
		procCoUninitialize.Call()
		d.com = false
	}
}
