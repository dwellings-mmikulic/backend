package property

import "time"

// VideoStatus tracks the render state of a listing's video.
type VideoStatus string

const (
	VideoPending VideoStatus = "pending"
	VideoReady   VideoStatus = "ready"
	VideoFailed  VideoStatus = "failed"
)

// Details holds the enrichment fields fetched once per property from the
// Zillow property-details API. All fields are pointers: nil means unknown
// (not yet enriched, or absent from the API response).
type Details struct {
	PropertyType   *string
	Description    *string
	YearBuilt      *int
	Heating        *string
	Cooling        *string
	Garage         *string
	HOAFeeMonthly  *int
	MLSNumber      *string
	ListingStatus  *string
	AgentName      *string
	AgentPhone     *string
	AgentBrokerage *string
	Latitude       *float64
	Longitude      *float64
}

// Property is the domain model for a collected Zillow listing.
type Property struct {
	ID           int64
	ZPID         string // Zillow property id — natural key for upserts
	SalePrice    int64  // in whole dollars
	Address      string
	City         string
	State        string
	Zip          string
	HomeSizeSqft int
	LotSizeSqft  int
	Bedrooms     int
	Bathrooms    float64
	DetailURL    string   // Zillow listing URL (for QR code + Roku feed)
	ImageURLs    []string // Bunny CDN URLs

	Details
	DetailsFetchedAt *time.Time // nil = enrichment not yet attempted/succeeded

	// Video / Roku feed state.
	VideoURL          string
	VideoStatus       VideoStatus
	VideoContentHash  string
	VideoRenderedAt   *time.Time
	VideoDurationSecs int

	CreatedAt time.Time
	UpdatedAt time.Time
}
