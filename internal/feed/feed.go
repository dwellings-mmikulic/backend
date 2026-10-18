// Package feed renders the published listings for Roku: the Roku Search feed
// (search.go) and the page cursor it is walked with (cursor.go). The Roku app
// itself reads the listings API, not a feed.
package feed

import (
	"strconv"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// title is the listing's headline: price and street address.
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

// shortDescription is the listing's facts line: beds, baths, size, place.
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
