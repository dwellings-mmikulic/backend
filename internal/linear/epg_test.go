package linear

import (
	"context"
	"testing"
	"time"
)

func TestEPG_ThirtyMinuteBlocksWithListingSummary(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 40 one-minute clips, prices 300,000 … 339,000
	s := testService(m, t0.Add(7*time.Minute))

	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e.Channel.Key != "zip:77494" || e.Channel.Scope != "zip:77494" || e.Channel.Name != "Homes for sale in 77494" {
		t.Errorf("channel = %+v", e.Channel)
	}
	if len(e.Programs) == 0 {
		t.Fatal("no programs")
	}
	first := e.Programs[0]
	if first.Start.Minute()%30 != 0 || first.Start.Second() != 0 {
		t.Errorf("first program start %s is not on a half hour", first.Start)
	}
	if !first.End.Equal(first.Start.Add(30 * time.Minute)) {
		t.Errorf("program length = %s", first.End.Sub(first.Start))
	}
	if first.Title != "Homes for sale in 77494" {
		t.Errorf("title = %q", first.Title)
	}
	if first.Listings < 1 || first.Listings > 30 {
		t.Errorf("listings = %d", first.Listings)
	}
	if first.Description == "" || first.Description[len(first.Description)-1] == ' ' {
		t.Errorf("description = %q", first.Description)
	}
	for i := 1; i < len(e.Programs); i++ {
		if e.Programs[i].Start.Before(e.Programs[i-1].End) {
			t.Errorf("programs overlap at %d", i)
		}
	}
}

// EPG advances the version chain by at most epgChainBudget versions per
// call (advanceChain), so a channel whose versions are short relative to
// EPGHorizonHours only reaches the full horizon after several polls. Each
// poll resumes the persisted chain where the last one left off, so coverage
// grows monotonically until it reaches now + EPGHorizonHours.
func TestEPG_CoverageGrowsAcrossPolls(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 40 min of content per version
	s := testService(m, t0)

	var lastEnd time.Time
	for poll := 0; poll < 6; poll++ {
		e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0)
		if err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if len(e.Programs) == 0 {
			t.Fatalf("poll %d: no programs", poll)
		}
		end := e.Programs[len(e.Programs)-1].End
		if end.Before(lastEnd) {
			t.Fatalf("poll %d: coverage shrank from %s to %s", poll, lastEnd, end)
		}
		lastEnd = end
	}
	if want := t0.Add(23 * time.Hour); lastEnd.Before(want) {
		t.Errorf("after repeated polls EPG ends at %s, want ≥ %s (now + 23 h)", lastEnd, want)
	}
}

// A scope sitting at the MinScopeClips floor produces very short versions
// (buildItems never repeats clips), so reaching a 24 h horizon can need far
// more versions than a single request should build synchronously — and more
// than maxChain allows at all. EPG must still succeed: it materialises at
// most maxChain versions to catch the chain up to now (the same bound
// Playlist relies on) plus epgChainBudget more toward the horizon, rather
// than erroring or chaining without limit toward a 24 h-away horizon.
func TestEPG_ThinScopeBoundsChainWorkPerCall(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 3) // MinScopeClips floor (test config): ~3 min per version
	s := testService(m, t0)

	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatalf("EPG errored on a thin scope instead of returning partial coverage: %v", err)
	}
	if len(e.Programs) == 0 {
		t.Fatal("no programs")
	}
	all, _ := m.ListVersions(context.Background(), "zip:77494")
	if want := maxChain + epgChainBudget; len(all) > want {
		t.Errorf("materialised %d versions in one EPG call, want ≤ %d (maxChain catch-up + epgChainBudget)", len(all), want)
	}

	// A second poll (chain already caught up to now) only spends the
	// horizon-extension budget.
	before := len(all)
	if _, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0); err != nil {
		t.Fatal(err)
	}
	all2, _ := m.ListVersions(context.Background(), "zip:77494")
	if got := len(all2) - before; got > epgChainBudget {
		t.Errorf("steady-state poll materialised %d versions, want ≤ %d (epgChainBudget)", got, epgChainBudget)
	}
}

// The response window starts around now, not at the channel's very first
// version: an old channel can carry hundreds of past versions, and walking
// all of them into the response would grow it without bound purely with
// channel age.
func TestEPG_WindowStartsNearNowNotChannelGenesis(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 200) // more than one version's worth of content

	genesis := t0.Add(-30 * 24 * time.Hour)
	old := testService(m, genesis)
	v1, err := old.newVersion(context.Background(), "zip:77494", 1, genesis, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.InsertVersion(context.Background(), v1); err != nil {
		t.Fatal(err)
	}

	s := testService(m, t0) // 30 days after the channel's first version
	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Programs) == 0 {
		t.Fatal("no programs")
	}
	if e.Programs[0].Start.Before(t0.Add(-programBlock)) {
		t.Errorf("first program starts at %s (%s before now); window should be clamped near now, not the channel's genesis %s",
			e.Programs[0].Start, t0.Sub(e.Programs[0].Start), genesis)
	}
}

func TestEPG_DescriptionFormat(t *testing.T) {
	if got := describe(3, 285000, 1150000); got != "3 listings · $285,000–$1,150,000" {
		t.Errorf("describe = %q", got)
	}
	if got := describe(1, 500000, 500000); got != "1 listing · $500,000" {
		t.Errorf("describe = %q", got)
	}
	if got := describe(2, 0, 0); got != "2 listings" {
		t.Errorf("describe = %q", got)
	}
}
