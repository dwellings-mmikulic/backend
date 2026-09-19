package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// QueuedListing is the listing_queue payload: what a search result knows about
// a listing, plus Revisit for items enqueued by cmd/backfill-videos.
//
// It is a stored format. Rows written by one build are read by the next, by
// other boxes of the fleet mid-rollout and by the backfill tool, so every key
// is spelled out: renaming a Go field must not silently change it.
type QueuedListing struct {
	ZPID         string   `json:"zpid"`
	Address      string   `json:"address"`
	City         string   `json:"city"`
	State        string   `json:"state"`
	Zip          string   `json:"zip"`
	DetailURL    string   `json:"detail_url"`
	SalePrice    int64    `json:"sale_price"`
	HomeSizeSqft int      `json:"home_size_sqft"`
	LotSizeSqft  int      `json:"lot_size_sqft"`
	Bedrooms     int      `json:"bedrooms"`
	Bathrooms    float64  `json:"bathrooms"`
	ImageURLs    []string `json:"image_urls"`
	// Revisit marks a listing that is already stored and only needs its video
	// rendered: no photo upload, no upsert, whatever SKIP_EXISTING says.
	Revisit bool `json:"revisit,omitempty"`
}

// EncodeListing builds the queue payload for p. It is exported for
// cmd/backfill-videos, which enqueues stored listings with revisit set.
//
// NUL bytes are stripped from every string: PostgreSQL stores neither a NUL in
// text nor \u0000 in jsonb, so a single one in a provider string would make
// the item unstorable (and property.Upsert would refuse the row later anyway).
// p is left untouched.
func EncodeListing(p *property.Property, revisit bool) ([]byte, error) {
	q := QueuedListing{
		ZPID:         stripNUL(p.ZPID),
		Address:      stripNUL(p.Address),
		City:         stripNUL(p.City),
		State:        stripNUL(p.State),
		Zip:          stripNUL(p.Zip),
		DetailURL:    stripNUL(p.DetailURL),
		SalePrice:    p.SalePrice,
		HomeSizeSqft: p.HomeSizeSqft,
		LotSizeSqft:  p.LotSizeSqft,
		Bedrooms:     p.Bedrooms,
		Bathrooms:    p.Bathrooms,
		Revisit:      revisit,
	}
	if p.ImageURLs != nil {
		q.ImageURLs = make([]string, len(p.ImageURLs))
		for i, u := range p.ImageURLs {
			q.ImageURLs[i] = stripNUL(u)
		}
	}
	payload, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("encode listing %s: %w", p.ZPID, err)
	}
	return payload, nil
}

// DecodeListing is the inverse of EncodeListing: the listing and its revisit
// flag. Unknown keys are ignored so an older worker can read a newer build's
// rows during a rollout. A payload without a zpid is an error: there is
// nothing such an item could be stored or rendered as.
func DecodeListing(payload []byte) (*property.Property, bool, error) {
	var q QueuedListing
	if err := json.Unmarshal(payload, &q); err != nil {
		return nil, false, fmt.Errorf("decode listing payload: %w", err)
	}
	if q.ZPID == "" {
		return nil, false, errors.New("decode listing payload: no zpid")
	}
	return &property.Property{
		ZPID:         q.ZPID,
		Address:      q.Address,
		City:         q.City,
		State:        q.State,
		Zip:          q.Zip,
		DetailURL:    q.DetailURL,
		SalePrice:    q.SalePrice,
		HomeSizeSqft: q.HomeSizeSqft,
		LotSizeSqft:  q.LotSizeSqft,
		Bedrooms:     q.Bedrooms,
		Bathrooms:    q.Bathrooms,
		ImageURLs:    q.ImageURLs,
	}, q.Revisit, nil
}

func stripNUL(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}
