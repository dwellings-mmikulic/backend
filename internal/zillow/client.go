// Package zillow is a client for the OpenWebNinja Real-Time Zillow Data API.
// Endpoint: GET https://api.openwebninja.com/realtime-zillow-data/search
// Auth: X-API-Key header. The struct tags below match the live response shape.
package zillow

import (
	"context"
	"encoding/json"
	"errors"
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

	// wait sleeps between retry attempts and gives up early when ctx is
	// done. It is a field only so tests can assert the back-off without
	// sleeping through it.
	wait func(ctx context.Context, d time.Duration) error
}

// New creates a Zillow API client.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
		wait:    sleepCtx,
	}
}

// listing is one entry from the search response "data" array. Only the fields
// we persist are mapped.
type listing struct {
	ZPID          string   `json:"zpid"`
	Price         number   `json:"price"`
	DetailURL     string   `json:"detailUrl"`
	Address       string   `json:"address"`
	StreetAddress string   `json:"streetAddress"`
	City          string   `json:"city"`
	State         string   `json:"state"`
	Zipcode       string   `json:"zipcode"`
	LivingArea    number   `json:"livingArea"`
	LotAreaValue  number   `json:"lotAreaValue"`
	LotAreaUnit   string   `json:"lotAreaUnit"`
	Bedrooms      number   `json:"bedrooms"`
	Bathrooms     number   `json:"bathrooms"`
	ImgSrc        string   `json:"imgSrc"`
	Carousel      carousel `json:"carouselPhotosComposable"`
}

// carousel holds the full photo set; URLs are built from baseUrl + photoKey.
type carousel struct {
	BaseURL   string `json:"baseUrl"`
	PhotoData []struct {
		PhotoKey string `json:"photoKey"`
	} `json:"photoData"`
}

// maxSearchPages is the hard safety cap on pagination.
const maxSearchPages = 20

// SearchResult is what one SearchPages call fetched, complete or not.
type SearchResult struct {
	Properties []property.Property
	// Requests is the number of HTTP requests actually sent, retries
	// included. It is reported even alongside an error: a failed request
	// still counted against the provider's quota.
	Requests int
	// NextPage is 0 when the search ran to completion. Otherwise the search
	// stopped early and NextPage is the first page that was not fetched,
	// which is where a later call should resume.
	NextPage int
}

// SearchPages returns properties matching the configured criteria, paging
// from startPage (<= 0 means 1) until MaxResults is reached, the API runs out
// of results, or the page cap is hit; all three are a complete search
// (NextPage 0). s.MaxPages, when > 0, lowers the hard 20-page cap; both are
// absolute page numbers, so a resumed search stops where an uninterrupted one
// would have. Price and bedroom criteria are applied client-side.
//
// The permit is asked before every HTTP attempt. When it denies page p the
// budget has run out mid-ZIP, which is not a failure: the pages fetched so far
// come back with NextPage = p and a nil error. Any other failure on page p
// returns the pages fetched so far TOGETHER with the error and NextPage = p,
// so nothing already paid for is thrown away or bought again.
func (c *Client) SearchPages(ctx context.Context, s config.SearchCriteria, startPage int, permit Permit) (SearchResult, error) {
	lastPage := maxSearchPages
	if s.MaxPages > 0 && s.MaxPages < lastPage {
		lastPage = s.MaxPages
	}
	if startPage <= 0 {
		startPage = 1
	}

	var res SearchResult
	for page := startPage; page <= lastPage; page++ {
		raw, sent, err := c.searchPage(ctx, s, page, permit)
		res.Requests += sent
		if errors.Is(err, ErrBudgetExhausted) {
			res.NextPage = page
			return res, nil
		}
		if err != nil {
			res.NextPage = page
			return res, fmt.Errorf("search page %d: %w", page, err)
		}
		if len(raw) == 0 {
			break
		}
		for i := range raw {
			p := toProperty(&raw[i])
			if !matches(&p, s) {
				continue
			}
			res.Properties = append(res.Properties, p)
			if s.MaxResults > 0 && len(res.Properties) >= s.MaxResults {
				return res, nil
			}
		}
	}
	return res, nil
}

// Usage reports the account's current quota for this API, queried from the
// provider's /usage endpoint (which lives at the API host root, not under the
// per-API base path). The api_id is derived from the base URL's last path
// segment, e.g. ".../realtime-zillow-data" → "realtime_zillow_data".
type Usage struct {
	Plan struct {
		Nickname string `json:"nickname"`
		IsFree   bool   `json:"is_free"`
	} `json:"plan"`
	Status string        `json:"status"` // "ok" | "exceeded" | ...
	Quotas []QuotaMetric `json:"quotas"`
}

// QuotaMetric is one named quota (e.g. "Requests") within a Usage report.
type QuotaMetric struct {
	Name      string `json:"name"`
	Limit     int    `json:"limit"`
	Used      int    `json:"used"`
	Remaining int    `json:"remaining"`
	ResetAt   string `json:"reset_at"`
}

func (c *Client) Usage(ctx context.Context) (*Usage, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	apiID := strings.ReplaceAll(strings.Trim(base.Path, "/"), "-", "_")
	if apiID == "" {
		return nil, fmt.Errorf("cannot derive api_id from base URL %q", c.baseURL)
	}
	endpoint := fmt.Sprintf("%s://%s/usage?api_id=%s", base.Scheme, base.Host, url.QueryEscape(apiID))

	// Deliberately outside fetch: the quota probe is not charged to the budget
	// ledger, so it asks no permit, and the quota gate fails open on any
	// error, so a retry would buy nothing.
	req, err := c.newRequest(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data Usage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode zillow response: %w", err)
	}
	return &env.Data, nil
}

// searchPage fetches one page. The int is the number of requests sent for it.
func (c *Client) searchPage(ctx context.Context, s config.SearchCriteria, page int, permit Permit) ([]listing, int, error) {
	q := url.Values{}
	q.Set("location", s.Location)
	q.Set("page", strconv.Itoa(page))
	if s.HomeStatus != "" {
		q.Set("home_status", s.HomeStatus)
	}
	endpoint := fmt.Sprintf("%s/search?%s", c.baseURL, q.Encode())

	body, sent, err := c.fetch(ctx, endpoint, permit)
	if err != nil {
		return nil, sent, err
	}
	listings, err := decodeListings(body)
	return listings, sent, err
}

// decodeListings unwraps a search response, whose data is a flat array of
// listings. No listings (with an OK envelope) is the end of the results.
//
// Only what has always meant "no listings" does: an empty array, null, or no
// data at all. An empty object or string is a shape this client does not
// know and stays a decode error, because the caller marks the ZIP searched on
// a clean end of results: a provider that moved its listings elsewhere would
// otherwise empty every ZIP without a single error in the logs.
func decodeListings(body []byte) ([]listing, error) {
	env, err := decodeEnvelope(body)
	if err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return nil, nil // no "data" key; [] and null need no help from us
	}
	var listings []listing
	if err := json.Unmarshal(env.Data, &listings); err != nil {
		if env.failed() {
			// Not an array because it is the failure report, not a page.
			return nil, env.softError(body)
		}
		return nil, fmt.Errorf("decode zillow listings: %w", err)
	}
	return listings, nil
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

func lotToSqft(v number, unit string) int {
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
