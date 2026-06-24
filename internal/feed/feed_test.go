package feed

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

func TestBuild_GoldenShape(t *testing.T) {
	rendered := time.Date(2026, 6, 18, 10, 0, 0, 0, time.UTC)
	props := []property.Property{
		{
			ZPID: "43590635", SalePrice: 310000, Address: "23265 McBurney Ave",
			City: "Punta Gorda", State: "FL", Bedrooms: 3, Bathrooms: 3, HomeSizeSqft: 1779,
			ImageURLs:         []string{"https://cdn/properties/43590635/0.jpg"},
			VideoURL:          "https://cdn/videos/43590635.mp4",
			VideoDurationSecs: 120,
			VideoRenderedAt:   &rendered,
		},
	}
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	f := Build("DwellingTV", props, now)

	if f.ProviderName != "DwellingTV" || f.Language != "en" {
		t.Errorf("header wrong: %+v", f)
	}
	if f.LastUpdated != "2026-06-20T12:00:00Z" {
		t.Errorf("lastUpdated = %s", f.LastUpdated)
	}
	if len(f.ShortFormVideos) != 1 {
		t.Fatalf("want 1 video, got %d", len(f.ShortFormVideos))
	}
	v := f.ShortFormVideos[0]
	if v.ID != "43590635" {
		t.Errorf("id = %s", v.ID)
	}
	if v.Title != "$310,000 · 23265 McBurney Ave" {
		t.Errorf("title = %q", v.Title)
	}
	if v.ShortDescription != "3 bd · 3 ba · 1,779 sqft — Punta Gorda, FL" {
		t.Errorf("shortDescription = %q", v.ShortDescription)
	}
	if v.Thumbnail != "https://cdn/properties/43590635/0.jpg" {
		t.Errorf("thumbnail = %q", v.Thumbnail)
	}
	if v.ReleaseDate != "2026-06-18" {
		t.Errorf("releaseDate = %q", v.ReleaseDate)
	}
	if v.Content.Duration != 120 || len(v.Content.Videos) != 1 {
		t.Errorf("content wrong: %+v", v.Content)
	}
	if v.Content.Videos[0].URL != "https://cdn/videos/43590635.mp4" ||
		v.Content.Videos[0].Quality != "HD" || v.Content.Videos[0].VideoType != "MP4" {
		t.Errorf("video rendition wrong: %+v", v.Content.Videos[0])
	}
}

func TestBuild_SkipsMissingVideoURL(t *testing.T) {
	props := []property.Property{
		{ZPID: "1", VideoURL: ""},
		{ZPID: "2", VideoURL: "https://cdn/videos/2.mp4", VideoDurationSecs: 30},
	}
	f := Build("DwellingTV", props, time.Now())
	if len(f.ShortFormVideos) != 1 || f.ShortFormVideos[0].ID != "2" {
		t.Errorf("expected only id=2, got %+v", f.ShortFormVideos)
	}
}

func TestBuild_ValidJSON(t *testing.T) {
	f := Build("DwellingTV", []property.Property{
		{ZPID: "2", Address: "X", SalePrice: 100000, VideoURL: "https://cdn/v.mp4", VideoDurationSecs: 30},
	}, time.Now())
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) {
		t.Error("invalid json")
	}
}
