package music

import (
	"errors"
	"fmt"
)

const (
	tsPacket = 188
	// audioLead is how long before its playing time a music frame is placed
	// in the segment, measured against the segment's clock reference. The
	// listing encodes place their own audio 0.35–0.6 s ahead.
	audioLead = 54000 // 0.6 s in 90 kHz ticks
	// frameGrid is one video frame at 30 fps in 90 kHz ticks. Every clip's
	// first picture sits on this grid.
	frameGrid = 3000
	// maxSegmentMS bounds a segment request; the channel's are under 10 s.
	maxSegmentMS = 60_000
)

// ErrNotRemuxable reports a video segment the music cannot be put into. The
// caller should serve the segment as it is.
var ErrNotRemuxable = errors.New("segment cannot take the music track")

// layout is what Remux needs to know about a video segment.
type layout struct {
	audioPID, videoPID, pcrPID int
	firstVideoPTS              int64
}

func pidOf(p []byte) int { return int(p[1]&0x1F)<<8 | int(p[2]) }

// payload returns the payload of TS packet p, nil when it has none.
func payload(p []byte) []byte {
	switch (p[3] >> 4) & 3 {
	case 1:
		return p[4:]
	case 3:
		if n := 5 + int(p[4]); n < tsPacket {
			return p[n:]
		}
	}
	return nil
}

// pcrOf returns the clock reference carried by p, in 90 kHz ticks.
func pcrOf(p []byte) (int64, bool) {
	if p[3]&0x20 == 0 || p[4] < 7 || p[5]&0x10 == 0 {
		return 0, false
	}
	return int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10])>>7, true
}

// pesPTS returns the timestamp of the PES packet starting in b.
func pesPTS(b []byte) (int64, bool) {
	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 || b[7]&0x80 == 0 {
		return 0, false
	}
	q := b[9:14]
	return int64(q[0]>>1&7)<<30 | int64(q[1])<<22 | int64(q[2]>>1)<<15 | int64(q[3])<<7 | int64(q[4]>>1), true
}

// section returns the table section that starts in packet p.
func section(p []byte) []byte {
	b := payload(p)
	if p[1]&0x40 == 0 || len(b) < 1 || 1+int(b[0])+3 > len(b) {
		return nil
	}
	b = b[1+int(b[0]):]
	if n := 3 + (int(b[1]&0x0F)<<8 | int(b[2])); n <= len(b) {
		return b[:n]
	}
	return nil
}

// scan reads the segment's program tables and its first picture's timestamp.
func scan(seg []byte) (layout, error) {
	lay := layout{audioPID: -1, videoPID: -1, pcrPID: -1, firstVideoPTS: -1}
	pmtPID := -1
	for off := 0; off < len(seg); off += tsPacket {
		p := seg[off : off+tsPacket]
		if p[0] != 0x47 {
			return lay, fmt.Errorf("%w: no sync byte at %d", ErrNotRemuxable, off)
		}
		pid := pidOf(p)
		switch {
		case pid == 0 && pmtPID < 0:
			s := section(p)
			for i := 8; s != nil && i+4 <= len(s)-4; i += 4 {
				if s[i] != 0 || s[i+1] != 0 { // program 0 is the network table
					pmtPID = int(s[i+2]&0x1F)<<8 | int(s[i+3])
					break
				}
			}
		case pid == pmtPID && lay.pcrPID < 0:
			s := section(p)
			if len(s) < 16 || s[0] != 0x02 {
				continue
			}
			lay.pcrPID = int(s[8]&0x1F)<<8 | int(s[9])
			for i := 12 + (int(s[10]&0x0F)<<8 | int(s[11])); i+5 <= len(s)-4; {
				es := int(s[i+1]&0x1F)<<8 | int(s[i+2])
				switch s[i] {
				case 0x0F: // AAC in ADTS, which is what the loop is
					if lay.audioPID < 0 {
						lay.audioPID = es
					}
				case 0x1B, 0x24:
					if lay.videoPID < 0 {
						lay.videoPID = es
					}
				}
				i += 5 + (int(s[i+3]&0x0F)<<8 | int(s[i+4]))
			}
		case pid == lay.videoPID && lay.firstVideoPTS < 0 && p[1]&0x40 != 0:
			if pts, ok := pesPTS(payload(p)); ok {
				lay.firstVideoPTS = pts
			}
		}
		if lay.firstVideoPTS >= 0 {
			break
		}
	}
	if lay.audioPID < 0 || lay.videoPID < 0 || lay.pcrPID == lay.audioPID || lay.firstVideoPTS < 0 {
		return lay, fmt.Errorf("%w: audio pid %d, video pid %d, clock pid %d, first picture %d",
			ErrNotRemuxable, lay.audioPID, lay.videoPID, lay.pcrPID, lay.firstVideoPTS)
	}
	return lay, nil
}

// framePackets is how many TS packets writeFrame uses for a frame of n bytes.
func framePackets(n int) int { return (pesHeader + n + tsPacket - 4 - 1) / (tsPacket - 4) }

const pesHeader = 14

// appendFrame appends one ADTS frame as one PES packet on pid with timestamp
// pts, continuing the continuity counter cc, and returns the next counter.
func appendFrame(dst []byte, frame []byte, pid int, pts int64, cc uint8) ([]byte, uint8) {
	pesLen := 3 + 5 + len(frame)
	pes := make([]byte, 0, pesHeader+len(frame))
	pes = append(pes, 0x00, 0x00, 0x01, 0xC0, byte(pesLen>>8), byte(pesLen), 0x80, 0x80, 0x05,
		0x21|byte(pts>>29)&0x0E, byte(pts>>22), 0x01|byte(pts>>14)&0xFE, byte(pts>>7), 0x01|byte(pts<<1)&0xFE)
	pes = append(pes, frame...)

	start := true
	for len(pes) > 0 {
		var p [tsPacket]byte
		p[0] = 0x47
		p[1] = byte(pid >> 8)
		if start {
			p[1] |= 0x40
		}
		p[2] = byte(pid)
		i := 4
		if room := tsPacket - 4; len(pes) < room {
			// The last packet: an adaptation field of stuffing fills the gap.
			fill := room - len(pes) // 1..183 bytes, counting its length byte
			p[3] = 0x30 | cc
			p[4] = byte(fill - 1)
			if fill > 1 {
				p[5] = 0x00
				for k := 6; k < 4+fill; k++ {
					p[k] = 0xFF
				}
			}
			i = 4 + fill
		} else {
			p[3] = 0x10 | cc
		}
		pes = pes[copy(p[i:], pes):]
		dst = append(dst, p[:]...)
		cc = (cc + 1) & 0x0F
		start = false
	}
	return dst, cc
}

// Remux returns video, an MPEG-TS segment with picture and sound, with its
// sound replaced by the music on air while it plays.
//
// startMS is the wall-clock time the segment airs at, durMS its length and
// elapsedMS how far into its clip it starts. The frames are picked by the
// wall clock alone, so consecutive segments carry consecutive frames of the
// loop whatever clips they belong to. Their timestamps continue the clip's
// own timeline: the clip's first picture is found from this segment's first
// picture, and the frame on air then is given that timestamp. The continuity
// counter is the one a muxer writing the clip in one go would have reached.
//
// ErrNotRemuxable when the segment has no AAC track to replace.
func (l *Loop) Remux(video []byte, startMS int64, durMS, elapsedMS int) ([]byte, error) {
	if durMS <= 0 || durMS > maxSegmentMS || elapsedMS < 0 || startMS < int64(elapsedMS) {
		return nil, fmt.Errorf("segment timing out of range: start %d, duration %d, elapsed %d", startMS, durMS, elapsedMS)
	}
	if len(video) == 0 || len(video)%tsPacket != 0 {
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of packets", ErrNotRemuxable, len(video))
	}
	lay, err := scan(video)
	if err != nil {
		return nil, err
	}

	clipStartMS := startMS - int64(elapsedMS)
	// The clip's first picture, from this segment's: the playlist's
	// millisecond durations are a few hundred microseconds off per segment,
	// the frame grid takes that back out.
	clipPTS := lay.firstVideoPTS - int64(elapsedMS)*90
	clipPTS = (clipPTS + frameGrid/2) / frameGrid * frameGrid
	ptsOf := func(i int64) int64 { return clipPTS + l.frameTicks(i) - clipStartMS*90 }

	first, end := l.frameAt(startMS), l.frameAt(startMS+int64(durMS))
	var cc uint8
	for i := l.frameAt(clipStartMS); i < first; i++ {
		cc += uint8(framePackets(l.frameSize(i)))
	}
	cc &= 0x0F

	out := make([]byte, 0, len(video)+int(end-first)*2*tsPacket)
	next := first
	var clock int64
	haveClock := false
	emit := func(upTo int64) error {
		for ; next < end && ptsOf(next) <= upTo; next++ {
			if ptsOf(next) < 0 {
				continue
			}
			f, err := l.frame(next)
			if err != nil {
				return err
			}
			out, cc = appendFrame(out, f, lay.audioPID, ptsOf(next), cc)
		}
		return nil
	}
	for off := 0; off < len(video); off += tsPacket {
		p := video[off : off+tsPacket]
		if p[0] != 0x47 {
			return nil, fmt.Errorf("%w: no sync byte at %d", ErrNotRemuxable, off)
		}
		pid := pidOf(p)
		if pid == lay.audioPID {
			continue
		}
		if pid == lay.pcrPID {
			if pcr, ok := pcrOf(p); ok {
				clock, haveClock = pcr, true
			}
		}
		if haveClock {
			if err := emit(clock + audioLead); err != nil {
				return nil, err
			}
		}
		out = append(out, p...)
	}
	if err := emit(1 << 62); err != nil {
		return nil, err
	}
	return out, nil
}
