package linear

import (
	"errors"
	"strings"
	"testing"
)

// A missing clips entry (the zero-value ClipSegments, zero segments) must
// never be allowed to silently shift every later segment's media sequence
// number — expandItems must fail loudly instead, with an error the handler
// can turn into a logged 500 rather than a panic that kills the request.
func TestExpandItems_ErrorsOnMissingClip(t *testing.T) {
	v1, _, _ := fixture()
	_, err := expandItems(v1, map[int64]ClipSegments{}, 0, 0)
	if !errors.Is(err, ErrInconsistentLineup) {
		t.Fatalf("err = %v, want ErrInconsistentLineup", err)
	}
	var ile *InconsistentLineupError
	if !errors.As(err, &ile) {
		t.Fatalf("err %v is not an *InconsistentLineupError", err)
	}
	if ile.ClipID != 1 || ile.Item != 0 || ile.Channel != "us" || ile.Version != 1 || !ile.Missing {
		t.Errorf("error detail = %+v", ile)
	}
	if !strings.Contains(err.Error(), "clip 1") {
		t.Errorf("message = %q, want it to name clip 1", err)
	}
}

// A clip present in the map but whose actual segment count disagrees with
// the version's recorded ItemSegs must also fail loudly, not desync.
func TestExpandItems_ErrorsOnSegmentCountMismatch(t *testing.T) {
	v1, _, clips := fixture()
	bad := map[int64]ClipSegments{
		1: {ID: 1, BaseURL: clips[1].BaseURL, SegmentMS: []int{3000, 3000}}, // v1.ItemSegs[0] says 3
	}
	_, err := expandItems(v1, bad, 0, 0)
	if !errors.Is(err, ErrInconsistentLineup) {
		t.Fatalf("err = %v, want ErrInconsistentLineup", err)
	}
	var ile *InconsistentLineupError
	if errors.As(err, &ile); ile.Got != 2 || ile.Want != 3 || ile.Missing {
		t.Errorf("error detail = %+v", ile)
	}
}

// The media sequence number of an item must come from v.ItemSegs, not from
// counting an individual clip's actual segments, so that expanding a range
// that skips a prefix still lands on the correct starting sequence.
func TestExpandItems_SeqAdvancesByItemSegs(t *testing.T) {
	v1, _, clips := fixture()
	// item 0 (A) has 3 segments per v1.ItemSegs[0]; item 1 (B) must start at
	// StartSeq + 3 regardless of how expandItems iterates A's segments.
	segs, err := expandItems(v1, clips, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantSeq := v1.StartSeq + int64(v1.ItemSegs[0])
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Seq != wantSeq || segs[1].Seq != wantSeq+1 {
		t.Errorf("seqs = [%d %d], want [%d %d]", segs[0].Seq, segs[1].Seq, wantSeq, wantSeq+1)
	}
}

// An itemless version cannot be produced by newVersion (a lineup always has
// at least one clip), but a hand-written row must not index out of range.
func TestWindow_ItemlessVersionIsEmptyNotAPanic(t *testing.T) {
	v := &Version{Key: "us", Version: 1, StartsAt: t0, EndsAt: t0}
	if ids := windowClipIDs(v, nil, t0, liveWindow); len(ids) != 0 {
		t.Errorf("windowClipIDs = %v, want none", ids)
	}
	segs, err := window(v, nil, map[int64]ClipSegments{}, t0, liveWindow)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("window = %+v, want none", segs)
	}
}
