// Package zillow is a client for the OpenWebNinja Real-Time Zillow Data API.
// Endpoint: GET https://api.openwebninja.com/realtime-zillow-data/search
// Auth: X-API-Key header. The struct tags below match the live response shape.
package zillow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dwellingtw/backend/internal/config"
	"github.com/dwellingtw/backend/internal/property"
)

// Client talks to the OpenWebNinja Real-Time Zillow Data API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New creates a Zillow API client.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
	}
}

// listing is one entry from the search response "data" array. Only the fields
// we persist are mapped.
type listing struct {
	ZPID          string      `json:"zpid"`
	Price         json.Number `json:"price"`
	DetailURL     string      `json:"detailUrl"`
	Address       string      `json:"address"`
	StreetAddress string      `json:"streetAddress"`
	City          string      `json:"city"`
	State         string      `json:"state"`
	Zipcode       string      `json:"zipcode"`
	LivingArea    json.Number `json:"livingArea"`
	LotAreaValue  json.Number `json:"lotAreaValue"`
	LotAreaUnit   string      `json:"lotAreaUnit"`
	Bedrooms      json.Number `json:"bedrooms"`
	Bathrooms     json.Number `json:"bathrooms"`
	ImgSrc        string      `json:"imgSrc"`
	Carousel      carousel    `json:"carouselPhotosComposable"`
}

// carousel holds the full photo set; URLs are built from baseUrl + photoKey.
type carousel struct {
	BaseURL   string `json:"baseUrl"`
	PhotoData []struct {
		PhotoKey string `json:"photoKey"`
	} `json:"photoData"`
}

// searchResponse is the OpenWebNinja envelope: {status, request_id, parameters, data:[...]}.
type searchResponse struct {
	Status string    `json:"status"`
	Data   []listing `json:"data"`
}

// Search returns properties matching the configured criteria, paging until
// MaxResults is reached or the API runs out of results. Price and bedroom
// criteria are applied client-side.
func (c *Client) Search(ctx context.Context, s config.SearchCriteria) ([]property.Property, error) {
	var out []property.Property
	for page := 1; ; page++ {
		raw, err := c.searchPage(ctx, s, page)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}
		for i := range raw {
			p := toProperty(&raw[i])
			if !matches(&p, s) {
				continue
			}
			out = append(out, p)
			if s.MaxResults > 0 && len(out) >= s.MaxResults {
				return out, nil
			}
		}
		if page >= 20 { // hard safety cap on pagination
			break
		}
	}
	return out, nil
}

func (c *Client) searchPage(ctx context.Context, s config.SearchCriteria, page int) ([]listing, error) {
	q := url.Values{}
	q.Set("location", s.Location)
	q.Set("page", strconv.Itoa(page))
	if s.HomeStatus != "" {
		q.Set("home_status", s.HomeStatus)
	}
	endpoint := fmt.Sprintf("%s/search?%s", c.baseURL, q.Encode())

	var resp searchResponse
	if err := c.getJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zillow request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("zillow API returned status %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode zillow response: %w", err)
	}
	return nil
}

// matches applies the config criteria that the API call itself doesn't enforce.
func matches(p *property.Property, s config.SearchCriteria) bool {
	if s.MinPrice > 0 && p.SalePrice < int64(s.MinPrice) {
		return false
	}
	if s.MaxPrice > 0 && p.SalePrice > int64(s.MaxPrice) {
		return false
	}
	if s.MinBedrooms > 0 && p.Bedrooms < s.MinBedrooms {
		return false
	}
	return true
}

// toProperty maps a raw listing to the domain model.
func toProperty(l *listing) property.Property {
	return property.Property{
		ZPID:         l.ZPID,
		SalePrice:    toInt64(l.Price),
		Address:      firstNonEmpty(l.StreetAddress, l.Address),
		DetailURL:    normalizeDetailURL(l.DetailURL),
		City:         l.City,
		State:        l.State,
		Zip:          l.Zipcode,
		HomeSizeSqft: toInt(l.LivingArea),
		LotSizeSqft:  lotToSqft(l.LotAreaValue, l.LotAreaUnit),
		Bedrooms:     toInt(l.Bedrooms),
		Bathrooms:    toFloat(l.Bathrooms),
		ImageURLs:    photoURLs(l),
	}
}

// photoURLs builds the full image URL set from the carousel template, falling
// back to the single imgSrc thumbnail. URLs are upgraded to a high-resolution
// size so the rendered video isn't blurry.
func photoURLs(l *listing) []string {
	if l.Carousel.BaseURL != "" && len(l.Carousel.PhotoData) > 0 {
		urls := make([]string, 0, len(l.Carousel.PhotoData))
		for _, pd := range l.Carousel.PhotoData {
			if pd.PhotoKey == "" {
				continue
			}
			urls = append(urls, hiResZillow(strings.ReplaceAll(l.Carousel.BaseURL, "{photoKey}", pd.PhotoKey)))
		}
		if len(urls) > 0 {
			return urls
		}
	}
	if l.ImgSrc != "" {
		return []string{hiResZillow(l.ImgSrc)}
	}
	return nil
}

// zillowSizeToken matches the trailing size token in a zillowstatic photo URL,
// e.g. the "-p_e" in ".../<key>-p_e.jpg".
var zillowSizeToken = regexp.MustCompile(`-[a-z0-9_]+\.jpg$`)

// hiResZillow upgrades a zillowstatic photo URL to the largest commonly-available
// size (cc_ft_1536 returns up to 1536px). The API's carousel/imgSrc default to a
// small "-p_e" (~600px) size which upscales poorly to 1080p.
func hiResZillow(u string) string {
	if !strings.Contains(u, "photos.zillowstatic.com") {
		return u
	}
	return zillowSizeToken.ReplaceAllString(u, "-cc_ft_1536.jpg")
}

// normalizeDetailURL makes the Zillow detail URL absolute. The API sometimes
// returns a relative path like "/homedetails/...".
func normalizeDetailURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "/") {
		return "https://www.zillow.com" + u
	}
	return u
}

func lotToSqft(v json.Number, unit string) int {
	f := toFloat(v)
	if strings.EqualFold(strings.TrimSpace(unit), "acres") {
		f *= 43560 // 1 acre = 43,560 sq ft
	}
	return int(f)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func toInt(n json.Number) int     { return int(toFloat(n)) }
func toInt64(n json.Number) int64 { return int64(toFloat(n)) }

func toFloat(n json.Number) float64 {
	if n == "" {
		return 0
	}
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return f
}
