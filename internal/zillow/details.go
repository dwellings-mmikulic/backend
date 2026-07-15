package zillow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dwellingtw/backend/internal/property"
)

// ErrDetailsNotFound means the API has no details for this zpid. Callers
// should mark the row as fetched so it is not retried forever.
var ErrDetailsNotFound = errors.New("property details not found")

// detailsPath is the provider's property-details endpoint, relative to the
// API base URL. Kept as a const because the exact path is provider-defined
// (verified live during implementation — see the plan's Task 5 Step 7).
const detailsPath = "/property-details"

// detailsRecord maps the fields we persist from the details response "data"
// object. Unknown/absent fields simply stay zero and map to nil pointers.
type detailsRecord struct {
	ZPID          json.Number `json:"zpid"`
	HomeType      string      `json:"homeType"`
	HomeStatus    string      `json:"homeStatus"`
	Description   string      `json:"description"`
	YearBuilt     json.Number `json:"yearBuilt"`
	MonthlyHOAFee json.Number `json:"monthlyHoaFee"`
	Latitude      json.Number `json:"latitude"`
	Longitude     json.Number `json:"longitude"`
	ResoFacts     struct {
		Heating               []string    `json:"heating"`
		Cooling               []string    `json:"cooling"`
		GarageParkingCapacity json.Number `json:"garageParkingCapacity"`
	} `json:"resoFacts"`
	AttributionInfo struct {
		AgentName        string `json:"agentName"`
		AgentPhoneNumber string `json:"agentPhoneNumber"`
		BrokerName       string `json:"brokerName"`
		MLSID            string `json:"mlsId"`
	} `json:"attributionInfo"`
}

// PropertyDetails fetches the one-time enrichment record for a zpid. It
// returns the mapped details plus the raw response body (stored as
// details_raw so future fields never require re-fetching).
func (c *Client) PropertyDetails(ctx context.Context, zpid string) (*property.Details, []byte, error) {
	endpoint := fmt.Sprintf("%s%s?zpid=%s", c.baseURL, detailsPath, url.QueryEscape(zpid))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build details request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("details request zpid=%s: %w", zpid, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, nil, fmt.Errorf("details API returned status %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20)) // details payloads are large; 4 MiB is ample
	if err != nil {
		return nil, nil, fmt.Errorf("read details body: %w", err)
	}

	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, nil, fmt.Errorf("decode details envelope: %w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
	}

	var rec detailsRecord
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, nil, fmt.Errorf("decode details data: %w", err)
	}
	return toDetails(&rec), body, nil
}

// toDetails maps the raw record onto the domain model, turning empty values
// into nil pointers.
func toDetails(rec *detailsRecord) *property.Details {
	return &property.Details{
		PropertyType:   strPtr(rec.HomeType),
		Description:    strPtr(rec.Description),
		YearBuilt:      intPtr(rec.YearBuilt),
		Heating:        strPtr(strings.Join(rec.ResoFacts.Heating, ", ")),
		Cooling:        strPtr(strings.Join(rec.ResoFacts.Cooling, ", ")),
		Garage:         garagePtr(rec.ResoFacts.GarageParkingCapacity),
		HOAFeeMonthly:  intPtr(rec.MonthlyHOAFee),
		MLSNumber:      strPtr(rec.AttributionInfo.MLSID),
		ListingStatus:  strPtr(rec.HomeStatus),
		AgentName:      strPtr(rec.AttributionInfo.AgentName),
		AgentPhone:     strPtr(rec.AttributionInfo.AgentPhoneNumber),
		AgentBrokerage: strPtr(rec.AttributionInfo.BrokerName),
		Latitude:       floatPtr(rec.Latitude),
		Longitude:      floatPtr(rec.Longitude),
	}
}

func strPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(n json.Number) *int {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	v := int(f)
	return &v
}

func floatPtr(n json.Number) *float64 {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	return &f
}

func garagePtr(n json.Number) *string {
	cap := toFloat(n)
	if cap <= 0 {
		return nil
	}
	s := fmt.Sprintf("%d Car Garage", int(cap))
	return &s
}
