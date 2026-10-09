package linear

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

func TestEnsureHousehold_CreatesSpan0FromFirstCandidateWithContent(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	m.addZip("77777") // real ZIP, no listings: must be skipped
	s := testService(m, t0)
	hh := viewer.ID{1}

	spans, err := s.EnsureHousehold(context.Background(), hh, []Scope{{Zip: "77777"}, {Zip: "77494"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 || spans[0].N != 0 || spans[0].Scope != "zip:77494" || spans[0].Requested != "zip:77494" ||
		spans[0].Source != SourceGeo || !spans[0].StartsAt.Equal(t0) || spans[0].SeqOffset != 0 || spans[0].ItemOffset != 0 {
		t.Errorf("span 0 = %+v", spans)
	}
	again, err := s.EnsureHousehold(context.Background(), hh, nil, t0.Add(time.Hour))
	if err != nil || len(again) != 1 || !again[0].StartsAt.Equal(t0) {
		t.Errorf("second call must return the existing span: %+v, %v", again, err)
	}
}

func TestEnsureHousehold_DefaultsToNational(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	spans, err := s.EnsureHousehold(context.Background(), viewer.ID{2}, []Scope{{State: "fl"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if spans[0].Scope != "us" || spans[0].Source != SourceDefault || spans[0].Requested != "" {
		t.Errorf("span 0 = %+v", spans[0])
	}
}

func TestHousehold_UnknownIsAnError(t *testing.T) {
	s := testService(newMemStore(), t0)
	if _, err := s.Household(context.Background(), viewer.ID{3}); !errors.Is(err, ErrUnknownHousehold) {
		t.Errorf("err = %v", err)
	}
}

func TestCities_UsesMinScopeClipsAndCaches(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 5)
	addClips(m, austin, 101, 2) // below MinScopeClips (3 in testService)
	s := testService(m, t0)
	cs, err := s.Cities(context.Background())
	if err != nil || len(cs) != 1 || cs[0].City != "katy" {
		t.Fatalf("cities = %+v, %v", cs, err)
	}
	addClips(m, austin, 201, 5)
	cs, _ = s.Cities(context.Background())
	if len(cs) != 1 {
		t.Errorf("cached answer expected within 10 min, got %+v", cs)
	}
	s.now = func() time.Time { return t0.Add(11 * time.Minute) }
	cs, _ = s.Cities(context.Background())
	if len(cs) != 2 {
		t.Errorf("refreshed answer expected after 10 min, got %+v", cs)
	}
}
