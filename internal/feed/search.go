package feed

import (
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// SearchFeed is a Roku Search feed document (spec version 1): the catalogue
// Roku ingests so listings appear in the home-screen search and deep link
// into the app. It is a different format from the Direct Publisher Feed the
// app itself reads; both are rendered from the same published listings.
// See https://developer.roku.com/docs/specs/search/search-feed.md.
type SearchFeed struct {
	Version                      string        `json:"version"`
	DefaultLanguage              string        `json:"defaultLanguage"`
	DefaultAvailabilityCountries []string      `json:"defaultAvailabilityCountries"`
	NextPageURL                  string        `json:"nextPageUrl,omitempty"`
	Assets                       []SearchAsset `json:"assets"`
}

// SearchAsset is one shortForm catalogue entry. ID and PlayID are both the
// zpid: Roku hands PlayID back to the app as the deep link contentId.
type SearchAsset struct {
	ID                string           `json:"id"`
	Type              string           `json:"type"`
	Titles            []LocalizedText  `json:"titles"`
	ShortDescriptions []LocalizedText  `json:"shortDescriptions"`
	ReleaseDate       string           `json:"releaseDate"`
	Genres            []string         `json:"genres"`
	Tags              []string         `json:"tags,omitempty"`
	AdvisoryRatings   []AdvisoryRating `json:"advisoryRatings"`
	Images            []SearchImage    `json:"images"`
	DurationInSeconds int              `json:"durationInSeconds"`
	Content           SearchContent    `json:"content"`
}

// LocalizedText is a title or description in one or more languages.
type LocalizedText struct {
	Value     string   `json:"value"`
	Languages []string `json:"languages"`
}

// AdvisoryRating is a parental rating from one rating authority.
type AdvisoryRating struct {
	Source string `json:"source"`
	Value  string `json:"value"`
}

// SearchImage is a poster; Roku requires one of type "main" (16:9 or 2:3).
type SearchImage struct {
	Type      string   `json:"type"`
	URL       string   `json:"url"`
	Languages []string `json:"languages,omitempty"`
}

// SearchContent holds the ways the item can be played.
type SearchContent struct {
	PlayOptions []PlayOption `json:"playOptions"`
}

// PlayOption is one way to play the item; PlayID is the deep link contentId.
type PlayOption struct {
	License string `json:"license"`
	Quality string `json:"quality"`
	PlayID  string `json:"playId"`
}

// Search feed constants: what every listing clip is, as far as Roku's fixed
// vocabularies allow. "house/garden" is the closest genre to real estate in
// Roku's list; NR under the US TV Parental Guidelines says "not rated" in the
// field Roku requires; the clips are free 1080p renders.
const (
	searchLanguage  = "en"
	searchCountry   = "us"
	searchGenre     = "house/garden"
	searchRatingSrc = "USA_PR"
	searchRating    = "NR"
	searchQuality   = "fhd"
	searchLicense   = "free"
	searchTag       = "real estate"
	maxTextLen      = 200 // Roku's cap on titles and short descriptions
)

// BuildSearch renders one page of the Roku Search feed from ready listings.
// Listings without a video or a photo are skipped: Roku requires a main
// image, and a search hit must be playable.
func BuildSearch(props []property.Property, now time.Time) SearchFeed {
	f := SearchFeed{
		Version:                      "1",
		DefaultLanguage:              searchLanguage,
		DefaultAvailabilityCountries: []string{searchCountry},
		Assets:                       make([]SearchAsset, 0, len(props)),
	}
	for i := range props {
		p := &props[i]
		if p.VideoURL == "" || len(p.ImageURLs) == 0 || p.ImageURLs[0] == "" {
			continue
		}
		released := now.UTC()
		if p.VideoRenderedAt != nil {
			released = p.VideoRenderedAt.UTC()
		}
		desc := shortDescription(p)
		if desc == "" {
			desc = "Home for sale"
		}
		f.Assets = append(f.Assets, SearchAsset{
			ID:                p.ZPID,
			Type:              "shortForm",
			Titles:            []LocalizedText{{Value: truncate(title(p), maxTextLen), Languages: []string{searchLanguage}}},
			ShortDescriptions: []LocalizedText{{Value: truncate(desc, maxTextLen), Languages: []string{searchLanguage}}},
			ReleaseDate:       released.Format("2006-01-02"),
			Genres:            []string{searchGenre},
			Tags:              []string{searchTag},
			AdvisoryRatings:   []AdvisoryRating{{Source: searchRatingSrc, Value: searchRating}},
			Images:            []SearchImage{{Type: "main", URL: p.ImageURLs[0], Languages: []string{searchLanguage}}},
			DurationInSeconds: p.VideoDurationSecs,
			Content: SearchContent{PlayOptions: []PlayOption{{
				License: searchLicense, Quality: searchQuality, PlayID: p.ZPID,
			}}},
		})
	}
	return f
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
