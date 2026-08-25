package locationiq

import (
	"regexp"
	"strings"
)

// unitSuffix matches a trailing secondary-address designator: "#4E",
// "Apt 5", "Unit B", ", Ste 200", "Fl 3". LocationIQ's structured search
// cannot resolve a street carrying one and falls back to the city centroid
// (which Geocode then rejects), whereas the bare street resolves to the
// house. The value after the designator must be a unit-like token — a
// number with optional letters, or a single letter — so a road that
// happens to be called "Unit Road" is left alone.
var unitSuffix = regexp.MustCompile(`(?i)[\s,]+(?:#\s*\S+|(?:apt|apartment|unit|ste|suite|fl|floor|bldg|building|lot|spc|space|trlr|rm|room)\.?\s+(?:[a-z]?\d+[a-z]?|[a-z]))$`)

// stripUnit removes a trailing unit/apartment designator from a street
// address. Addresses without one are returned unchanged.
func stripUnit(street string) string {
	return strings.TrimSpace(unitSuffix.ReplaceAllString(strings.TrimSpace(street), ""))
}
