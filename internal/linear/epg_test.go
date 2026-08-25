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
	// Coverage extends at least to now + horizon (24 h) minus one block.
	last := e.Programs[len(e.Programs)-1]
	if last.End.Before(t0.Add(7*time.Minute + 23*time.Hour)) {
		t.Errorf("EPG ends at %s, want ≥ now + 23 h", last.End)
	}
	for i := 1; i < len(e.Programs); i++ {
		if e.Programs[i].Start.Before(e.Programs[i-1].End) {
			t.Errorf("programs overlap at %d", i)
		}
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
