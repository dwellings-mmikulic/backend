package linear

import (
	"context"
	"testing"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestMemStore_Anchors(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	hh := viewer.ID{3}
	if a, err := m.Anchor(ctx, hh, "us"); err != nil || a != nil {
		t.Fatalf("missing anchor = %+v err=%v", a, err)
	}
	a := &Anchor{Household: hh, Channel: "us", StartsAt: t0, SeqOffset: 10, ItemOffset: 2, CreatedAt: t0}
	if ok, err := m.InsertAnchor(ctx, a); err != nil || !ok {
		t.Fatalf("insert: ok=%v err=%v", ok, err)
	}
	dup := *a
	dup.SeqOffset = 99
	if ok, _ := m.InsertAnchor(ctx, &dup); ok {
		t.Error("duplicate (household, channel) must not insert")
	}
	got, _ := m.Anchor(ctx, hh, "us")
	if got == nil || got.SeqOffset != 10 {
		t.Errorf("anchor = %+v", got)
	}
	if other, _ := m.Anchor(ctx, hh, "state:tx"); other != nil {
		t.Errorf("anchor for another channel = %+v", other)
	}
}
