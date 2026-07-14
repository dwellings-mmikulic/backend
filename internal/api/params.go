package api

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/dwellingtw/backend/internal/property"
)

const (
	defaultLimit = 24
	maxLimit     = 100
)

// parseListParams validates browse query parameters into a property.Filter.
// The returned error text is the public 400 message and names the offending
// parameter.
func parseListParams(q url.Values) (property.Filter, error) {
	f := property.Filter{
		Sort:  property.SortNewest,
		Limit: defaultLimit,
		Zip:   q.Get("zip"), City: q.Get("city"), State: q.Get("state"),
		PropertyType: q.Get("property_type"),
	}

	var err error
	if f.MinPrice, err = int64Param(q, "min_price"); err != nil {
		return f, err
	}
	if f.MaxPrice, err = int64Param(q, "max_price"); err != nil {
		return f, err
	}
	if f.MinBeds, err = intParam(q, "min_beds"); err != nil {
		return f, err
	}
	if f.MinBaths, err = floatParam(q, "min_baths"); err != nil {
		return f, err
	}
	if f.MinSqft, err = intParam(q, "min_sqft"); err != nil {
		return f, err
	}
	if f.MaxSqft, err = intParam(q, "max_sqft"); err != nil {
		return f, err
	}

	switch s := q.Get("sort"); s {
	case "", string(property.SortNewest):
		// default already set
	case string(property.SortPriceAsc):
		f.Sort = property.SortPriceAsc
	case string(property.SortPriceDesc):
		f.Sort = property.SortPriceDesc
	default:
		return f, fmt.Errorf("invalid sort %q (newest, price_asc, price_desc)", s)
	}

	if s := q.Get("limit"); s != "" {
		n, convErr := strconv.Atoi(s)
		if convErr != nil || n < 1 || n > maxLimit {
			return f, fmt.Errorf("invalid limit (must be 1-%d)", maxLimit)
		}
		f.Limit = n
	}

	if tok := q.Get("cursor"); tok != "" {
		c, curErr := decodeCursor(tok, string(f.Sort))
		if curErr != nil {
			return f, curErr
		}
		key := &property.PageKey{ID: c.ID, Price: c.Price}
		if c.CreatedAt != nil {
			key.CreatedAt = *c.CreatedAt
		}
		f.After = key
	}
	return f, nil
}

func int64Param(q url.Values, name string) (int64, error) {
	s := q.Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return n, nil
}

func intParam(q url.Values, name string) (int, error) {
	n, err := int64Param(q, name)
	return int(n), err
}

func floatParam(q url.Values, name string) (float64, error) {
	s := q.Get(name)
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return f, nil
}
