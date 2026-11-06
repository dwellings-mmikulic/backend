package property

import (
	"fmt"
	"strings"
	"time"
)

// Sort orders for public listing queries.
type Sort string

const (
	SortNewest    Sort = "newest"
	SortPriceAsc  Sort = "price_asc"
	SortPriceDesc Sort = "price_desc"
)

// orderBy is the only source of ORDER BY clauses — user input never reaches
// the SQL directly. The secondary id column makes every ordering total, which
// keyset pagination requires.
var orderBy = map[Sort]string{
	SortNewest:    "created_at DESC, id DESC",
	SortPriceAsc:  "sale_price ASC, id ASC",
	SortPriceDesc: "sale_price DESC, id DESC",
}

// PageKey is the keyset position of the last row of the previous page.
type PageKey struct {
	CreatedAt time.Time // used by SortNewest
	Price     int64     // used by the price sorts
	ID        int64     // tiebreaker, always set
}

// Filter selects and orders properties for the public listing API. Zero
// values mean "no constraint".
type Filter struct {
	Zip          string
	City         string
	State        string
	MinPrice     int64
	MaxPrice     int64
	PropertyType string
	MinBeds      int
	MinBaths     float64
	MinSqft      int
	MaxSqft      int
	Sort         Sort
	Limit        int
	After        *PageKey
}

// publishedPredicate is what makes a stored listing public: its video has
// been rendered and uploaded. A listing is upserted before its video exists
// (video_status 'pending' while the media worker renders it, 'failed' when
// the render failed), and until then it belongs to no viewer-facing surface:
// the browse list, the detail endpoint and the Roku feed all share this one
// predicate, so they can never disagree about what is public.
const publishedPredicate = "video_status = 'ready' AND video_url IS NOT NULL"

// listColumns are the columns the browse endpoint needs; id and created_at
// feed the next-page cursor. COALESCE guards pre-enrichment NULLs on columns
// that scan into non-pointer Go fields.
const listColumns = `id, zpid, COALESCE(sale_price,0), address, COALESCE(city,''),
       COALESCE(state,''), COALESCE(zip,''), COALESCE(bedrooms,0),
       COALESCE(bathrooms,0), COALESCE(home_size_sqft,0), property_type,
       image_urls, COALESCE(video_url,''), created_at`

// buildWhere renders the published predicate, the filter and optionally the
// keyset predicate as a WHERE clause with 1-based positional args.
func buildWhere(f Filter, includeKeyset bool) (string, []any) {
	conds := []string{publishedPredicate}
	var args []any
	add := func(format string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}

	if f.Zip != "" {
		add("zip = $%d", f.Zip)
	}
	if f.City != "" {
		add("lower(city) = lower($%d)", f.City)
	}
	if f.State != "" {
		add("lower(state) = lower($%d)", f.State)
	}
	if f.MinPrice > 0 {
		add("sale_price >= $%d", f.MinPrice)
	}
	if f.MaxPrice > 0 {
		add("sale_price <= $%d", f.MaxPrice)
	}
	if f.PropertyType != "" {
		add("property_type = $%d", f.PropertyType)
	}
	if f.MinBeds > 0 {
		add("bedrooms >= $%d", f.MinBeds)
	}
	if f.MinBaths > 0 {
		add("bathrooms >= $%d", f.MinBaths)
	}
	if f.MinSqft > 0 {
		add("home_size_sqft >= $%d", f.MinSqft)
	}
	if f.MaxSqft > 0 {
		add("home_size_sqft <= $%d", f.MaxSqft)
	}

	if includeKeyset && f.After != nil {
		k := f.After
		switch f.Sort {
		case SortPriceAsc:
			args = append(args, k.Price, k.ID)
			conds = append(conds, fmt.Sprintf("(sale_price, id) > ($%d, $%d)", len(args)-1, len(args)))
		case SortPriceDesc:
			args = append(args, k.Price, k.ID)
			conds = append(conds, fmt.Sprintf("(sale_price, id) < ($%d, $%d)", len(args)-1, len(args)))
		default: // SortNewest
			args = append(args, k.CreatedAt, k.ID)
			conds = append(conds, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
		}
	}

	return " WHERE " + strings.Join(conds, " AND "), args
}

// buildListQuery renders the page SELECT. It asks for Limit+1 rows so the
// caller can detect whether a next page exists without a second query.
func buildListQuery(f Filter) (string, []any) {
	sort := f.Sort
	if _, ok := orderBy[sort]; !ok {
		sort = SortNewest
	}
	where, args := buildWhere(f, true)
	q := "SELECT " + listColumns + " FROM properties" + where +
		" ORDER BY " + orderBy[sort] +
		fmt.Sprintf(" LIMIT %d", f.Limit+1)
	return q, args
}

// buildCountQuery renders the total count for the same filter, without the
// keyset predicate (the total is page-independent).
func buildCountQuery(f Filter) (string, []any) {
	where, args := buildWhere(f, false)
	return "SELECT COUNT(*) FROM properties" + where, args
}

// SearchFeedSize is how many listings the Roku Search feed carries. Roku
// wants one page of a few thousand items, so the feed is a fixed set rather
// than the whole catalogue.
const SearchFeedSize = 3000

// feedColumns are what the Roku Search feed needs per listing.
const feedColumns = `p.id, p.zpid, p.sale_price, p.address, p.city, p.state, p.zip,
       p.home_size_sqft, p.lot_size_sqft, p.bedrooms, p.bathrooms, p.detail_url,
       p.image_urls, COALESCE(p.video_url,''), COALESCE(p.video_duration_secs,0),
       p.video_rendered_at`

// pinSearchFeedQuery fills search_feed_pins once: the SearchFeedSize newest
// published renders at that moment. The NOT EXISTS guard makes it a no-op
// for good after the first run, so the feed's list never changes on its own;
// truncating the table picks a fresh set on the next request.
var pinSearchFeedQuery = fmt.Sprintf(`
INSERT INTO search_feed_pins (property_id)
SELECT id FROM properties
WHERE %s AND NOT EXISTS (SELECT 1 FROM search_feed_pins)
ORDER BY video_rendered_at DESC, id DESC
LIMIT %d`, publishedPredicate, SearchFeedSize)

// searchFeedQuery reads the pinned listings that are still published, in a
// fixed order. A pinned listing whose video is re-rendering drops out until
// it is ready again; nothing takes its place.
var searchFeedQuery = "SELECT " + feedColumns +
	" FROM search_feed_pins s JOIN properties p ON p.id = s.property_id WHERE " +
	"p.video_status = 'ready' AND p.video_url IS NOT NULL ORDER BY p.id"
