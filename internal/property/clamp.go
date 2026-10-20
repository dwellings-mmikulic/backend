package property

import "math"

// Column limits from schema.sql. Zillow sometimes returns sizes that are not
// merely wrong but unstorable (a condo with 5,864,221,440 sqft of lot, a house
// with 40,103 bathrooms); passing them through fails the upsert with an int4
// encode error or a numeric overflow, and the listing parks in the queue after
// three attempts.
const (
	maxInt4Column    = math.MaxInt32
	maxBathroomsCell = 999.9 // NUMERIC(4,1)
)

// ClampToSchema turns every size the properties table cannot hold into 0,
// which the rest of the code already treats as "unknown" (overlays and the
// API omit zero sizes). Negative values are unknown too. It does not try to
// judge plausibility below the column limits.
func (p *Property) ClampToSchema() {
	p.HomeSizeSqft = clampInt4(p.HomeSizeSqft)
	p.LotSizeSqft = clampInt4(p.LotSizeSqft)
	p.Bedrooms = clampInt4(p.Bedrooms)
	if p.Bathrooms < 0 || p.Bathrooms > maxBathroomsCell {
		p.Bathrooms = 0
	}
}

func clampInt4(v int) int {
	if v < 0 || v > maxInt4Column {
		return 0
	}
	return v
}
