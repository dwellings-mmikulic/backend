package linear

import (
	"context"
	"time"

	"github.com/dwellingtw/backend/internal/viewer"
)

// ClipRef is what lineup building needs to know about a clip.
type ClipRef struct {
	ID       int64
	TotalMS  int
	Segments int
}

// ClipSegments is what playlist generation needs about a clip.
type ClipSegments struct {
	ID        int64
	BaseURL   string
	SegmentMS []int
}

// Listing is what the EPG needs about the property behind a clip.
type Listing struct {
	ClipID int64
	ZPID   string
	Price  int64
	City   string
	State  string
}

// Span is one stretch of a household's personal feed (see
// docs/superpowers/specs/2026-10-09-personal-feeds-qr-design.md): from
// StartsAt the household airs channel Scope. Spans chain the way versions
// do: the offsets relabel the channel's segment and item counters so the
// personal playlist's MEDIA-SEQUENCE and DISCONTINUITY-SEQUENCE keep
// climbing across a switch.
type Span struct {
	Household  viewer.ID
	N          int       // 0, 1, 2… in order; the latest is the current choice
	Scope      string    // effective channel key after fallback
	Requested  string    // the key the viewer asked for; "" when none
	Source     string    // SourceChoice, SourceGeo or SourceDefault
	StartsAt   time.Time // when this span starts airing
	SeqOffset  int64     // personal seq = channel seq + SeqOffset
	ItemOffset int64     // personal item = channel item + ItemOffset
	CreatedAt  time.Time
}

// Span sources.
const (
	SourceChoice  = "choice"  // the viewer picked it on the mobile page
	SourceGeo     = "geo"     // the IP's location
	SourceDefault = "default" // national, nothing better known
)

// City is an area the mobile page can offer.
type City struct {
	City  string // lowercase, as in properties.city
	State string // lowercase 2-letter code
	Clips int    // current clips
}

// Version is one materialised lineup of a channel. Versions form a chain:
// version N starts at N-1's EndsAt and continues its segment and item
// counters, so playlist sequence numbers stay monotonic across the chain.
type Version struct {
	Key       string
	Version   int
	Scope     string // effective scope key after fallback
	StartsAt  time.Time
	EndsAt    time.Time
	StartSeq  int64   // media sequence number of the first segment
	StartItem int64   // global index of item 0 (0 only for version 1)
	ItemIDs   []int64 // video_hls ids in air order
	ItemMS    []int
	ItemSegs  []int
}

// TotalMS is the version's air time.
func (v *Version) TotalMS() int {
	t := 0
	for _, ms := range v.ItemMS {
		t += ms
	}
	return t
}

// Segments is the number of segments across all items.
func (v *Version) Segments() int64 {
	var n int64
	for _, s := range v.ItemSegs {
		n += int64(s)
	}
	return n
}

// Store is the persistence the channel service needs.
type Store interface {
	// ListClips returns the current clips in scope, ordered by id.
	ListClips(ctx context.Context, s Scope) ([]ClipRef, error)
	// CityOfZip returns the lowercase city and state most listings in zip
	// belong to; both empty when the ZIP is unknown.
	CityOfZip(ctx context.Context, zip string) (city, state string, err error)
	// AreaExists reports whether s names a real place: a ZIP in zip_codes, a
	// city+state some property is in. The national scope always exists, and
	// so does a state that passed the state-code check — both are bounded
	// sets, unlike the ZIP and city spaces a caller can invent.
	AreaExists(ctx context.Context, s Scope) (bool, error)
	// LatestVersions returns up to n versions of key, newest first.
	LatestVersions(ctx context.Context, key string, n int) ([]Version, error)
	// VersionsAt returns the newest version of key that started at or before
	// t and its predecessor, newest first (empty when every version of key
	// starts after t). It is how "the version covering t" is found: the chain
	// tip can sit far in the future once the EPG has extended it, so the tip
	// says nothing about which version is on air at t.
	VersionsAt(ctx context.Context, key string, t time.Time) ([]Version, error)
	// ListVersionsBetween returns the versions of key that overlap
	// [from, to), oldest first. The EPG reports a bounded window, so it must
	// never have to load a channel's whole version history.
	ListVersionsBetween(ctx context.Context, key string, from, to time.Time) ([]Version, error)
	// InsertVersion stores v unless (key, version) already exists; ok reports
	// whether v was stored.
	InsertVersion(ctx context.Context, v *Version) (ok bool, err error)
	// ClipsByID returns the segment layout of each id.
	ClipsByID(ctx context.Context, ids []int64) (map[int64]ClipSegments, error)
	// ListingsByClipID returns the listing behind each clip id.
	ListingsByClipID(ctx context.Context, ids []int64) ([]Listing, error)
	// CountCurrentClips counts the current clips across the whole library.
	CountCurrentClips(ctx context.Context) (int, error)
	// Spans returns a household's spans oldest first; nil for an unknown
	// household.
	Spans(ctx context.Context, h viewer.ID) ([]Span, error)
	// InsertSpan stores sp unless (household, n) exists; ok reports whether
	// it was stored.
	InsertSpan(ctx context.Context, sp *Span) (ok bool, err error)
	// ReplaceSpan overwrites the stored (household, n) with sp. It is only
	// ever used on a span that has not started airing.
	ReplaceSpan(ctx context.Context, sp *Span) error
	// ListCities returns every city with at least min current clips, ordered
	// by state then city.
	ListCities(ctx context.Context, min int) ([]City, error)
}
