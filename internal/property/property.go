package property

import "time"

// VideoStatus tracks the render state of a listing's video.
type VideoStatus string

const (
	VideoPending VideoStatus = "pending"
	VideoReady   VideoStatus = "ready"
	VideoFailed  VideoStatus = "failed"
)

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

	// Video / Roku feed state.
	VideoURL          string
	VideoStatus       VideoStatus
	VideoContentHash  string
	VideoRenderedAt   *time.Time
	VideoDurationSecs int

	CreatedAt time.Time
	UpdatedAt time.Time
}
