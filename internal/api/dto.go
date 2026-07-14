package api

import (
	"math"
	"time"

	"github.com/dwellingtw/backend/internal/property"
)

// listItem is one browse-screen card.
type listItem struct {
	ZPID         string  `json:"zpid"`
	Price        int64   `json:"price"`
	Address      string  `json:"address"`
	City         string  `json:"city"`
	State        string  `json:"state"`
	Zip          string  `json:"zip"`
	Bedrooms     int     `json:"bedrooms"`
	Bathrooms    float64 `json:"bathrooms"`
	HomeSizeSqft int     `json:"home_size_sqft"`
	PropertyType *string `json:"property_type"`
	ImageURL     *string `json:"image_url"`
}

type listResponse struct {
	Total      int        `json:"total"`
	Results    []listItem `json:"results"`
	NextCursor *string    `json:"next_cursor"`
}

type agentDTO struct {
	Name      *string `json:"name"`
	Phone     *string `json:"phone"`
	Brokerage *string `json:"brokerage"`
}

// detailResponse is the full detail-screen payload.
type detailResponse struct {
	ZPID          string    `json:"zpid"`
	Price         int64     `json:"price"`
	Address       string    `json:"address"`
	City          string    `json:"city"`
	State         string    `json:"state"`
	Zip           string    `json:"zip"`
	Bedrooms      int       `json:"bedrooms"`
	Bathrooms     float64   `json:"bathrooms"`
	HomeSizeSqft  int       `json:"home_size_sqft"`
	LotSizeSqft   int       `json:"lot_size_sqft"`
	LotSizeAcres  float64   `json:"lot_size_acres"`
	PropertyType  *string   `json:"property_type"`
	Description   *string   `json:"description"`
	YearBuilt     *int      `json:"year_built"`
	Heating       *string   `json:"heating"`
	Cooling       *string   `json:"cooling"`
	Garage        *string   `json:"garage"`
	HOAFeeMonthly *int      `json:"hoa_fee_monthly"`
	MLSNumber     *string   `json:"mls_number"`
	ListingStatus *string   `json:"listing_status"`
	Agent         *agentDTO `json:"agent"`
	Latitude      *float64  `json:"latitude"`
	Longitude     *float64  `json:"longitude"`
	ImageURLs     []string  `json:"image_urls"`
	VideoURL      *string   `json:"video_url"`
	DetailURL     string    `json:"detail_url"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toListItem(p *property.Property) listItem {
	it := listItem{
		ZPID: p.ZPID, Price: p.SalePrice,
		Address: p.Address, City: p.City, State: p.State, Zip: p.Zip,
		Bedrooms: p.Bedrooms, Bathrooms: p.Bathrooms, HomeSizeSqft: p.HomeSizeSqft,
		PropertyType: p.PropertyType,
	}
	if len(p.ImageURLs) > 0 {
		it.ImageURL = &p.ImageURLs[0]
	}
	return it
}

func toDetailResponse(p *property.Property) detailResponse {
	d := detailResponse{
		ZPID: p.ZPID, Price: p.SalePrice,
		Address: p.Address, City: p.City, State: p.State, Zip: p.Zip,
		Bedrooms: p.Bedrooms, Bathrooms: p.Bathrooms, HomeSizeSqft: p.HomeSizeSqft,
		LotSizeSqft:  p.LotSizeSqft,
		LotSizeAcres: math.Round(float64(p.LotSizeSqft)/43560*100) / 100,
		PropertyType: p.PropertyType, Description: p.Description, YearBuilt: p.YearBuilt,
		Heating: p.Heating, Cooling: p.Cooling, Garage: p.Garage,
		HOAFeeMonthly: p.HOAFeeMonthly, MLSNumber: p.MLSNumber, ListingStatus: p.ListingStatus,
		Latitude: p.Latitude, Longitude: p.Longitude,
		ImageURLs: p.ImageURLs, DetailURL: p.DetailURL,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if d.ImageURLs == nil {
		d.ImageURLs = []string{}
	}
	if p.VideoURL != "" {
		d.VideoURL = &p.VideoURL
	}
	if p.AgentName != nil || p.AgentPhone != nil || p.AgentBrokerage != nil {
		d.Agent = &agentDTO{Name: p.AgentName, Phone: p.AgentPhone, Brokerage: p.AgentBrokerage}
	}
	return d
}
