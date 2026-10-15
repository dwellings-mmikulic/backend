package linear

import (
	"context"
	"testing"
)

func TestBumperItemIDs(t *testing.T) {
	item := bumperItem(7)
	if item != -7 || !isBumper(item) || bumperID(item) != 7 || isBumper(7) {
		t.Errorf("bumperItem(7)=%d isBumper=%v bumperID=%d", item, isBumper(item), bumperID(item))
	}
	b := Bumper{ID: 7, SegmentMS: []int{3000, 3000, 3000, 3000}, TotalMS: 12000}
	if r := b.ref(); r != (ClipRef{ID: -7, TotalMS: 12000, Segments: 4}) {
		t.Errorf("ref = %+v", r)
	}
}

func TestMemStore_Bumpers(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	if b, err := m.CurrentBumper(ctx); err != nil || b != nil {
		t.Fatalf("empty store: bumper=%+v err=%v", b, err)
	}
	m.addClip(1, katy, 1, 3000, 3000)
	m.addBumper(1, 3000, 3000, 3000, 3000)
	m.addBumper(2, 4000, 4000, 4000)
	b, err := m.CurrentBumper(ctx)
	if err != nil || b == nil || b.ID != 2 || b.TotalMS != 12000 {
		t.Fatalf("current bumper = %+v err=%v (want the newest, id 2)", b, err)
	}
	clips, err := m.ClipsByID(ctx, []int64{1, bumperItem(1), bumperItem(9)})
	if err != nil {
		t.Fatal(err)
	}
	if len(clips) != 2 || clips[1].BaseURL == "" || len(clips[bumperItem(1)].SegmentMS) != 4 {
		t.Errorf("clips = %+v (want clip 1 and bumper -1, not the missing -9)", clips)
	}
	if clips[bumperItem(1)].ID != bumperItem(1) || clips[bumperItem(1)].BaseURL != "https://cdn/hls/bumpers/1/x" {
		t.Errorf("bumper entry = %+v", clips[bumperItem(1)])
	}
}
