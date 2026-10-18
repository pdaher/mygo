package audio

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// macOS records through an Audio Queue on the default input, which
// converts what the microphone captures to mono float32 at inputRate.

const (
	kAudioFormatFlagIsPacked = 8
	aqInputBuffers           = 3
	aqInputFrames            = 1024 // per buffer: 21 ms
)

var (
	aqInLoad sync.Once

	audioQueueNewInput func(format *audioStreamBasicDescription, callback uintptr, user uintptr, runLoop, runLoopMode uintptr, flags uint32, out *uintptr) int32

	// aqInCallback is created once; queues route buffers back to their
	// device by its number, their user data.
	aqInCallback uintptr
	aqInMu       sync.Mutex
	aqInputs     = map[uintptr]*aqInput{}
	aqInNext     uintptr
)

func loadAudioQueueInput() error {
	if err := loadAudioToolbox(); err != nil {
		return err
	}
	aqInLoad.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		purego.RegisterLibFunc(&audioQueueNewInput, lib, "AudioQueueNewInput")
		// The queue's own thread calls it with each buffer filled.
		aqInCallback = purego.NewCallback(func(user, q uintptr, b *audioQueueBuffer, start uintptr, packets uint32, descs uintptr) {
			aqInMu.Lock()
			d := aqInputs[user]
			aqInMu.Unlock()
			if d == nil || d.closed.Load() {
				return
			}
			n := int(b.audioDataByteSize / 4)
			chunk := make([]float32, n)
			copy(chunk, unsafe.Slice((*float32)(b.audioData), n))
			select {
			case d.chunks <- chunk:
			default: // the recording is behind: drop it, as a device would
			}
			audioQueueEnqueueBuffer(q, b, 0, 0)
		})
	})
	if audioQueueNewInput == nil {
		return fmt.Errorf("audio: AudioQueueNewInput not found")
	}
	return nil
}

type aqInput struct {
	id     uintptr
	q      uintptr
	chunks chan []float32
	closed atomic.Bool
}

func openInputDevice() (inputDevice, error) {
	if err := loadAudioQueueInput(); err != nil {
		return nil, err
	}
	desc := audioStreamBasicDescription{
		sampleRate:       inputRate,
		formatID:         kAudioFormatLinearPCM,
		formatFlags:      kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked,
		bytesPerPacket:   4,
		framesPerPacket:  1,
		bytesPerFrame:    4,
		channelsPerFrame: 1,
		bitsPerChannel:   32,
	}
	d := &aqInput{chunks: make(chan []float32, 16)}
	aqInMu.Lock()
	aqInNext++
	d.id = aqInNext
	aqInputs[d.id] = d
	aqInMu.Unlock()
	fail := func(what string, st int32) (inputDevice, error) {
		d.close()
		return nil, fmt.Errorf("audio: %s failed (OSStatus %d)", what, st)
	}
	if st := audioQueueNewInput(&desc, aqInCallback, d.id, 0, 0, 0, &d.q); st != 0 {
		d.q = 0
		return fail("AudioQueueNewInput", st)
	}
	for range aqInputBuffers {
		var b *audioQueueBuffer
		if st := audioQueueAllocateBuffer(d.q, aqInputFrames*4, &b); st != 0 {
			return fail("AudioQueueAllocateBuffer", st)
		}
		if st := audioQueueEnqueueBuffer(d.q, b, 0, 0); st != 0 {
			return fail("AudioQueueEnqueueBuffer", st)
		}
	}
	if st := audioQueueStart(d.q, 0); st != 0 {
		return fail("AudioQueueStart", st)
	}
	return d, nil
}

func (d *aqInput) read() ([]float32, error) {
	select {
	case c := <-d.chunks:
		return c, nil
	case <-time.After(100 * time.Millisecond):
		return nil, nil
	}
}

func (d *aqInput) close() {
	d.closed.Store(true)
	if d.q != 0 {
		audioQueueStop(d.q, true)
		audioQueueDispose(d.q, true)
		d.q = 0
	}
	aqInMu.Lock()
	delete(aqInputs, d.id)
	aqInMu.Unlock()
}
