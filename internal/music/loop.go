// Package music gives the channel one continuous music track. The music is a
// long ADTS AAC file that plays as a loop on the wall clock. Every video
// segment a viewer fetches has its own audio taken out and the slice of the
// loop that is on air at that moment put in, so the music plays straight
// through every change of listing while the player still receives ordinary
// segments with picture and sound together.
package music

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const samplesPerFrame = 1024

var sampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// Loop is an indexed ADTS AAC file.
type Loop struct {
	f          *os.File
	offsets    []int64 // byte offset of each frame, plus the end of the last
	sampleRate int
}

// Open indexes the ADTS file at path.
func Open(path string) (*Loop, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	l := &Loop{f: f}
	if err := l.index(); err != nil {
		f.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	return l, nil
}

func (l *Loop) index() error {
	st, err := l.f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	var hdr [7]byte
	var off int64
	for off+7 <= size {
		if _, err := l.f.ReadAt(hdr[:], off); err != nil {
			return err
		}
		if hdr[0] != 0xFF || hdr[1]&0xF6 != 0xF0 {
			return fmt.Errorf("no ADTS sync at byte %d", off)
		}
		idx := int(hdr[2]>>2) & 0x0F
		if idx >= len(sampleRates) {
			return fmt.Errorf("bad sample rate index %d at byte %d", idx, off)
		}
		if l.sampleRate == 0 {
			l.sampleRate = sampleRates[idx]
		} else if l.sampleRate != sampleRates[idx] {
			return fmt.Errorf("sample rate changes at byte %d", off)
		}
		n := int64(hdr[3]&0x03)<<11 | int64(hdr[4])<<3 | int64(hdr[5])>>5
		if n < 7 || off+n > size {
			return fmt.Errorf("bad frame length %d at byte %d", n, off)
		}
		l.offsets = append(l.offsets, off)
		off += n
	}
	if len(l.offsets) == 0 {
		return errors.New("no ADTS frames")
	}
	l.offsets = append(l.offsets, off)
	return nil
}

// Close releases the file.
func (l *Loop) Close() error { return l.f.Close() }

// Frames is the number of AAC frames in the loop.
func (l *Loop) Frames() int { return len(l.offsets) - 1 }

// SampleRate is the loop's sample rate in Hz.
func (l *Loop) SampleRate() int { return l.sampleRate }

// frameAt is the index (not wrapped) of the frame playing at wall-clock ms.
func (l *Loop) frameAt(ms int64) int64 {
	return ms * int64(l.sampleRate) / (samplesPerFrame * 1000)
}

// frameTicks is when frame i (not wrapped) starts, in 90 kHz ticks of wall
// clock. It is computed from the index, so it never drifts.
func (l *Loop) frameTicks(i int64) int64 {
	return i * samplesPerFrame * 90000 / int64(l.sampleRate)
}

// frameSize is the byte length of frame i, wrapping around the loop's end.
func (l *Loop) frameSize(i int64) int {
	k := i % int64(l.Frames())
	return int(l.offsets[k+1] - l.offsets[k])
}

// frame reads frame i of the loop, wrapping around its end.
func (l *Loop) frame(i int64) ([]byte, error) {
	k := i % int64(l.Frames())
	buf := make([]byte, l.offsets[k+1]-l.offsets[k])
	if _, err := l.f.ReadAt(buf, l.offsets[k]); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}
