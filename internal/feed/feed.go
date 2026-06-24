// Package feed builds a Roku Direct Publisher JSON feed from listing videos.
// See: https://developer.roku.com/docs/specs/direct-publisher-feeds.md
package feed

import (
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// Feed is the top-level Roku Direct Publisher document.
type Feed struct {
	ProviderName    string           `json:"providerName"`
	Language        string           `json:"language"`
	LastUpdated     string           `json:"lastUpdated"`
	ShortFormVideos []ShortFormVideo `json:"shortFormVideos"`
}

// ShortFormVideo is one playable listing entry.
type ShortFormVideo struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	ShortDescription string   `json:"shortDescription"`
	Thumbnail        string   `json:"thumbnail"`
	ReleaseDate      string   `json:"releaseDate"`
	Content          Content  `json:"content"`
	Tags             []string `json:"tags,omitempty"`
}

// Content holds the playable video info.
type Content struct {
	DateAdded string  `json:"dateAdded"`
	Duration  int     `json:"duration"`
	Videos    []Video `json:"videos"`
}

// Video is a single encoded rendition.
type Video struct {
	URL       string `json:"url"`
	Quality   string `json:"quality"`
	VideoType string `json:"videoType"`
}

// Build constructs the feed from ready listings. now is used for lastUpdated and
// as a fallback date when a listing has no render timestamp.
func Build(providerName string, props []property.Property, now time.Time) Feed {
	f := Feed{
		ProviderName:    providerName,
		Language:        "en",
		LastUpdated:     now.UTC().Format(time.RFC3339),
		ShortFormVideos: make([]ShortFormVideo, 0, len(props)),
	}

	for i := range props {
		p := &props[i]
		if p.VideoURL == "" {
			continue
		}
		added := now.UTC()
		if p.VideoRenderedAt != nil {
			added = p.VideoRenderedAt.UTC()
		}
		thumb := ""
		if len(p.ImageURLs) > 0 {
			thumb = p.ImageURLs[0]
		}

		f.ShortFormVideos = append(f.ShortFormVideos, ShortFormVideo{
			ID:               p.ZPID,
			Title:            title(p),
			ShortDescription: shortDescription(p),
			Thumbnail:        thumb,
			ReleaseDate:      added.Format("2006-01-02"),
			Content: Content{
				DateAdded: added.Format(time.RFC3339),
				Duration:  p.VideoDurationSecs,
				Videos: []Video{{
					URL:       p.VideoURL,
					Quality:   "HD",
					VideoType: "MP4",
				}},
			},
			Tags: []string{"real estate"},
		})
	}
	return f
}

func title(p *property.Property) string {
	price := "Contact for price"
	if p.SalePrice > 0 {
		price = "$" + humanInt(int64(p.SalePrice))
	}
	if p.Address == "" {
		return price
	}
	return price + " · " + p.Address
}

func shortDescription(p *property.Property) string {
	var parts []string
	if p.Bedrooms > 0 {
		parts = append(parts, strconv.Itoa(p.Bedrooms)+" bd")
	}
	if p.Bathrooms > 0 {
		parts = append(parts, trimFloat(p.Bathrooms)+" ba")
	}
	if p.HomeSizeSqft > 0 {
		parts = append(parts, humanInt(int64(p.HomeSizeSqft))+" sqft")
	}
	loc := strings.TrimSpace(strings.Trim(strings.Join([]string{p.City, p.State}, ", "), ", "))
	facts := strings.Join(parts, " · ")
	switch {
	case facts != "" && loc != "":
		return facts + " — " + loc
	case facts != "":
		return facts
	default:
		return loc
	}
}

func humanInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func trimFloat(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}
