package linear

import (
	"strings"
	"testing"
)

// A missing clips entry (the zero-value ClipSegments, zero segments) must
// never be allowed to silently shift every later segment's media sequence
// number — expandItems must fail loudly instead.
func TestExpandItems_PanicsOnMissingClip(t *testing.T) {
	v1, _, _ := fixture()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for missing clip, got none")
		}
		if !strings.Contains(r.(string), "clip 1") {
			t.Errorf("panic message = %q, want it to name clip 1", r)
		}
	}()
	expandItems(v1, map[int64]ClipSegments{}, 0, 0)
}

// A clip present in the map but whose actual segment count disagrees with
// the version's recorded ItemSegs must also fail loudly, not desync.
func TestExpandItems_PanicsOnSegmentCountMismatch(t *testing.T) {
	v1, _, clips := fixture()
	bad := map[int64]ClipSegments{
		1: {ID: 1, BaseURL: clips[1].BaseURL, SegmentMS: []int{3000, 3000}}, // v1.ItemSegs[0] says 3
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for segment count mismatch, got none")
		}
	}()
	expandItems(v1, bad, 0, 0)
}

// The media sequence number of an item must come from v.ItemSegs, not from
// counting an individual clip's actual segments, so that expanding a range
// that skips a prefix still lands on the correct starting sequence.
func TestExpandItems_SeqAdvancesByItemSegs(t *testing.T) {
	v1, _, clips := fixture()
	// item 0 (A) has 3 segments per v1.ItemSegs[0]; item 1 (B) must start at
	// StartSeq + 3 regardless of how expandItems iterates A's segments.
	segs := expandItems(v1, clips, 1, 1)
	wantSeq := v1.StartSeq + int64(v1.ItemSegs[0])
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Seq != wantSeq || segs[1].Seq != wantSeq+1 {
		t.Errorf("seqs = [%d %d], want [%d %d]", segs[0].Seq, segs[1].Seq, wantSeq, wantSeq+1)
	}
}
