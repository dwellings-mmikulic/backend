// Package musictest builds small ADTS loops and MPEG-TS segments for tests.
package musictest

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	VideoPID = 0x100
	AudioPID = 0x101
	pmtPID   = 0x1000
	// SampleRate of the loops WriteLoop builds.
	SampleRate = 44100
)

// Frame is ADTS frame i of a test loop: a valid header and a body that
// identifies the frame. Sizes vary so frames take one to three TS packets.
func Frame(i int) []byte {
	n := 7 + 120 + (i*97)%330
	f := make([]byte, n)
	f[0], f[1] = 0xFF, 0xF1
	f[2] = 1<<6 | 4<<2 // AAC-LC, 44.1 kHz
	f[3] = 2<<6 | byte(n>>11)
	f[4] = byte(n >> 3)
	f[5] = byte(n<<5) | 0x1F
	f[6] = 0xFC
	for k := 7; k < n; k++ {
		f[k] = byte(i + k)
	}
	return f
}

// WriteLoop writes a loop of n frames and returns its path.
func WriteLoop(t *testing.T, n int) string {
	t.Helper()
	var b []byte
	for i := range n {
		b = append(b, Frame(i)...)
	}
	path := filepath.Join(t.TempDir(), "loop.aac")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func packet(pid int, start bool, cc int, af, payload []byte) []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = byte(pid >> 8)
	if start {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	if fill := 184 - len(payload); fill > 0 || af != nil {
		p[3] = 0x30 | byte(cc&0x0F)
		p[4] = byte(fill - 1)
		copy(p[5:], af)
		for k := 5 + len(af); k < 4+fill; k++ {
			p[k] = 0xFF
		}
		if fill > 1 && af == nil {
			p[5] = 0
		}
		copy(p[4+fill:], payload)
	} else {
		p[3] = 0x10 | byte(cc&0x0F)
		copy(p[4:], payload)
	}
	return p
}

func pes(id byte, pts int64, data []byte) []byte {
	return append([]byte{0, 0, 1, id, 0, 0, 0x80, 0x80, 5,
		0x21 | byte(pts>>29)&0x0E, byte(pts >> 22), 0x01 | byte(pts>>14)&0xFE, byte(pts >> 7), 0x01 | byte(pts<<1)&0xFE}, data...)
}

// Segment builds a TS segment of n pictures at 30 fps, the first with
// timestamp firstPTS (90 kHz), each carrying a clock reference 0.7 s behind
// it. With audio it declares an AAC track and carries a packet of it after
// every third picture; without, the program has no sound.
func Segment(firstPTS int64, n int, audio bool) []byte {
	pat := []byte{0, 0x00, 0xB0, 0x0D, 0, 1, 0xC1, 0, 0, 0, 1, 0xE0 | pmtPID>>8, pmtPID & 0xFF, 0, 0, 0, 0}
	pmt := []byte{0, 0x02, 0xB0, 0x17, 0, 1, 0xC1, 0, 0, 0xE0 | VideoPID>>8, VideoPID & 0xFF, 0xF0, 0,
		0x1B, 0xE0 | VideoPID>>8, VideoPID & 0xFF, 0xF0, 0,
		0x0F, 0xE0 | AudioPID>>8, AudioPID & 0xFF, 0xF0, 0, 0, 0, 0, 0}
	if !audio {
		pmt = append(pmt[:18], 0, 0, 0, 0)
		pmt[3] = 0x12
	}
	out := append(packet(0, true, 0, nil, pat), packet(pmtPID, true, 0, nil, pmt)...)
	for k := range n {
		pts := firstPTS + int64(k)*3000
		pcr := pts - 63000
		af := []byte{0x10, byte(pcr >> 25), byte(pcr >> 17), byte(pcr >> 9), byte(pcr >> 1), byte(pcr<<7) | 0x7E, 0}
		picture := make([]byte, 100)
		for i := range picture {
			picture[i] = byte(k)
		}
		out = append(out, packet(VideoPID, true, k, af, pes(0xE0, pts, picture))...)
		if audio && k%3 == 2 {
			out = append(out, packet(AudioPID, true, k/3, nil, pes(0xC0, pts, []byte("the clip's own sound")))...)
		}
	}
	return out
}
