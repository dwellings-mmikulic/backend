package feed

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

func TestBuildSearch_Shape(t *testing.T) {
	rendered := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	props := []property.Property{
		{
			ZPID: "43590635", SalePrice: 310000, Address: "23265 McBurney Ave",
			City: "Punta Gorda", State: "FL", Bedrooms: 3, Bathrooms: 2.5, HomeSizeSqft: 1779,
			ImageURLs:         []string{"https://cdn/properties/43590635/0.jpg"},
			VideoURL:          "https://cdn/videos/43590635.mp4",
			VideoDurationSecs: 120,
			VideoRenderedAt:   &rendered,
		},
		// No photo: Roku requires a main image, so this one is left out.
		{ZPID: "2", VideoURL: "https://cdn/videos/2.mp4", VideoRenderedAt: &rendered},
		// No video: nothing to deep link into.
		{ZPID: "3", ImageURLs: []string{"https://cdn/3.jpg"}, VideoRenderedAt: &rendered},
	}
	f := BuildSearch(props, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))

	if f.Version != "1" || f.DefaultLanguage != "en" || len(f.DefaultAvailabilityCountries) != 1 || f.DefaultAvailabilityCountries[0] != "us" {
		t.Errorf("header wrong: %+v", f)
	}
	if f.NextPageURL != "" {
		t.Errorf("builder must leave nextPageUrl to the handler, got %q", f.NextPageURL)
	}
	if len(f.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 (no photo and no video are skipped)", len(f.Assets))
	}
	a := f.Assets[0]
	if a.ID != "43590635" || a.Type != "shortForm" {
		t.Errorf("id/type = %s/%s", a.ID, a.Type)
	}
	if a.Content.PlayOptions[0].PlayID != a.ID {
		t.Errorf("playId %q must equal the asset id %q: it is the deep link contentId", a.Content.PlayOptions[0].PlayID, a.ID)
	}
	if a.Titles[0].Value != "$310,000 · 23265 McBurney Ave" || a.Titles[0].Languages[0] != "en" {
		t.Errorf("title = %+v", a.Titles)
	}
	if a.ShortDescriptions[0].Value != "3 bd · 2.5 ba · 1,779 sqft — Punta Gorda, FL" {
		t.Errorf("shortDescription = %q", a.ShortDescriptions[0].Value)
	}
	if a.ReleaseDate != "2026-10-18" {
		t.Errorf("releaseDate = %s, want the render date", a.ReleaseDate)
	}
	if a.Genres[0] != "house/garden" || a.AdvisoryRatings[0].Source != "USA_PR" || a.AdvisoryRatings[0].Value != "NR" {
		t.Errorf("genre/rating = %v %v", a.Genres, a.AdvisoryRatings)
	}
	if a.Images[0].Type != "main" || a.Images[0].URL != "https://cdn/properties/43590635/0.jpg" {
		t.Errorf("image = %+v", a.Images)
	}
	if a.DurationInSeconds != 120 {
		t.Errorf("duration = %d", a.DurationInSeconds)
	}
	po := a.Content.PlayOptions[0]
	if po.License != "free" || po.Quality != "fhd" {
		t.Errorf("playOption = %+v", po)
	}
}

func TestBuildSearch_FallbacksAndCaps(t *testing.T) {
	long := strings.Repeat("x", 300)
	props := []property.Property{{
		ZPID: "9", Address: long, ImageURLs: []string{"https://cdn/9.jpg"}, VideoURL: "https://cdn/videos/9.mp4",
	}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	f := BuildSearch(props, now)
	a := f.Assets[0]
	if len([]rune(a.Titles[0].Value)) != 200 {
		t.Errorf("title must be capped at 200 chars, got %d", len([]rune(a.Titles[0].Value)))
	}
	if a.ShortDescriptions[0].Value != "Home for sale" {
		t.Errorf("a listing with no facts still needs a non-empty description, got %q", a.ShortDescriptions[0].Value)
	}
	if a.ReleaseDate != "2026-10-18" {
		t.Errorf("releaseDate without a render time must fall back to now, got %s", a.ReleaseDate)
	}
}

// The encoded document is what Roku validates; SEARCH_FEED_DUMP=<path>
// writes a page so it can be checked against Roku's JSON schema.
func TestBuildSearch_EncodesAndDumps(t *testing.T) {
	rendered := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	props := []property.Property{{
		ZPID: "43590635", SalePrice: 310000, Address: "23265 McBurney Ave", City: "Punta Gorda", State: "FL",
		Bedrooms: 3, Bathrooms: 2, HomeSizeSqft: 1779, ImageURLs: []string{"https://cdn/p/0.jpg"},
		VideoURL: "https://cdn/videos/43590635.mp4", VideoDurationSecs: 120, VideoRenderedAt: &rendered,
	}}
	f := BuildSearch(props, rendered)
	f.NextPageURL = "https://api.example.com/roku/search-feed.json?cursor=abc&limit=500"
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version":"1"`, `"nextPageUrl"`, `"type":"shortForm"`, `"playId":"43590635"`, `"languages":["en"]`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("encoded feed lacks %s:\n%s", key, b)
		}
	}
	if path := os.Getenv("SEARCH_FEED_DUMP"); path != "" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
