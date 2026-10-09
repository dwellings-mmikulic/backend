package linear

import (
	"context"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestMemStore_Spans(t *testing.T) {
	ctx := context.Background()
	m := newMemStore()
	hh := viewer.ID{7}
	if spans, _ := m.Spans(ctx, hh); spans != nil {
		t.Fatalf("unknown household has spans: %+v", spans)
	}
	s0 := &Span{Household: hh, N: 0, Scope: "us", Source: SourceDefault, StartsAt: t0, CreatedAt: t0}
	if ok, err := m.InsertSpan(ctx, s0); err != nil || !ok {
		t.Fatalf("insert span 0: ok=%v err=%v", ok, err)
	}
	if ok, _ := m.InsertSpan(ctx, s0); ok {
		t.Error("duplicate (household, n) must not insert")
	}
	s1 := &Span{Household: hh, N: 1, Scope: "zip:77494", Requested: "zip:77494", Source: SourceChoice, StartsAt: t0.Add(time.Minute), SeqOffset: 5, ItemOffset: 2, CreatedAt: t0.Add(time.Minute)}
	if ok, _ := m.InsertSpan(ctx, s1); !ok {
		t.Fatal("insert span 1")
	}
	s1b := *s1
	s1b.Scope = "state:tx"
	if err := m.ReplaceSpan(ctx, &s1b); err != nil {
		t.Fatal(err)
	}
	if err := m.ReplaceSpan(ctx, &Span{Household: hh, N: 9}); err == nil {
		t.Error("replacing a missing span must fail")
	}
	spans, _ := m.Spans(ctx, hh)
	if len(spans) != 2 || spans[0].N != 0 || spans[1].N != 1 || spans[1].Scope != "state:tx" {
		t.Errorf("spans = %+v", spans)
	}
}

func TestMemStore_ListCities(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, austin, 101, 2)
	cs, err := m.ListCities(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0] != (City{City: "katy", State: "tx", Clips: 5}) {
		t.Errorf("cities = %+v", cs)
	}
}
