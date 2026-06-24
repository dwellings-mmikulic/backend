package video

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// priceText formats the sale price like "$310,000". Returns "Contact for price"
// when unknown.
func priceText(p *property.Property) string {
	if p.SalePrice <= 0 {
		return "Contact for price"
	}
	return "$" + humanInt(p.SalePrice)
}

// addressText combines the street address with city, state and zip.
func addressText(p *property.Property) string {
	parts := []string{}
	if p.Address != "" {
		parts = append(parts, p.Address)
	}
	cityLine := strings.TrimSpace(strings.Join(nonEmpty(p.City, p.State), ", "))
	if p.Zip != "" {
		cityLine = strings.TrimSpace(cityLine + " " + p.Zip)
	}
	if cityLine != "" {
		parts = append(parts, cityLine)
	}
	return strings.Join(parts, ", ")
}

// factsText builds the facts row, e.g. "3 bd · 2 ba · 1,779 sqft · lot 9,999 sqft".
func factsText(p *property.Property) string {
	var parts []string
	if p.Bedrooms > 0 {
		parts = append(parts, fmt.Sprintf("%d bd", p.Bedrooms))
	}
	if p.Bathrooms > 0 {
		parts = append(parts, fmt.Sprintf("%s ba", trimFloat(p.Bathrooms)))
	}
	if p.HomeSizeSqft > 0 {
		parts = append(parts, fmt.Sprintf("%s sqft", humanInt(int64(p.HomeSizeSqft))))
	}
	if p.LotSizeSqft > 0 {
		parts = append(parts, fmt.Sprintf("lot %s sqft", humanInt(int64(p.LotSizeSqft))))
	}
	return strings.Join(parts, " · ")
}

// humanInt formats an integer with thousands separators, e.g. 1779 -> "1,779".
func humanInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// trimFloat renders a float without a trailing ".0" (3.0 -> "3", 2.5 -> "2.5").
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
