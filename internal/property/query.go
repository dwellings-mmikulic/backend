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

// listColumns are the columns the browse endpoint needs; id and created_at
// feed the next-page cursor. COALESCE guards pre-enrichment NULLs on columns
// that scan into non-pointer Go fields.
const listColumns = `id, zpid, COALESCE(sale_price,0), address, COALESCE(city,''),
       COALESCE(state,''), COALESCE(zip,''), COALESCE(bedrooms,0),
       COALESCE(bathrooms,0), COALESCE(home_size_sqft,0), property_type,
       image_urls, created_at`

// buildWhere renders the filter (and optionally the keyset predicate) as a
// WHERE clause with 1-based positional args.
func buildWhere(f Filter, includeKeyset bool) (string, []any) {
	var conds []string
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

	if len(conds) == 0 {
		return "", args
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
