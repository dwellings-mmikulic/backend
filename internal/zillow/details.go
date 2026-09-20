package zillow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ZPID          number `json:"zpid"`
	HomeType      string `json:"homeType"`
	HomeStatus    string `json:"homeStatus"`
	Description   string `json:"description"`
	YearBuilt     number `json:"yearBuilt"`
	MonthlyHOAFee number `json:"monthlyHoaFee"`
	Latitude      number `json:"latitude"`
	Longitude     number `json:"longitude"`
	ResoFacts     struct {
		Heating               []string `json:"heating"`
		Cooling               []string `json:"cooling"`
		GarageParkingCapacity number   `json:"garageParkingCapacity"`
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
//
// The permit is asked before every HTTP attempt; a denial is
// ErrBudgetExhausted with nothing sent. A 404, or an OK envelope without
// data, is ErrDetailsNotFound and final. An envelope that reports a failure
// and holds no record for a zpid is a transient error, never a record and
// never not-found. Use IsTransient on anything else to tell a provider outage
// (release the row, count no attempt) from a failure specific to this zpid.
func (c *Client) PropertyDetails(ctx context.Context, zpid string, permit Permit) (*property.Details, []byte, error) {
	endpoint := fmt.Sprintf("%s%s?zpid=%s", c.baseURL, detailsPath, url.QueryEscape(zpid))

	body, _, err := c.fetch(ctx, endpoint, permit)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
		}
		return nil, nil, fmt.Errorf("details zpid=%s: %w", zpid, err)
	}

	env, err := decodeEnvelope(body)
	if err != nil {
		return nil, nil, fmt.Errorf("details zpid=%s: %w", zpid, err)
	}
	// Only absent/null means not-found here, as it always has: an empty
	// object under an OK status still maps to a record with no fields.
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil, fmt.Errorf("zpid=%s: %w", zpid, ErrDetailsNotFound)
	}

	var rec detailsRecord
	decodeErr := json.Unmarshal(env.Data, &rec)
	// A failed envelope may carry its failure report in data, and
	// {"message": "upstream timeout"} decodes without complaint into a record
	// with every field empty: the caller would store it and stamp the row
	// fetched, losing the listing's details for good. So under a failed
	// status data only counts when it is a details record, which always names
	// its zpid; anything else (a string or a list never yields one either) is
	// the provider failing, not this row.
	if env.failed() && rec.ZPID == "" {
		return nil, nil, fmt.Errorf("details zpid=%s: %w", zpid, env.softError(body))
	}
	if decodeErr != nil {
		return nil, nil, fmt.Errorf("decode details data zpid=%s: %w", zpid, decodeErr)
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

func intPtr(n number) *int {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	v := int(f)
	return &v
}

func floatPtr(n number) *float64 {
	f := toFloat(n)
	if f == 0 {
		return nil
	}
	return &f
}

func garagePtr(n number) *string {
	cap := toFloat(n)
	if cap <= 0 {
		return nil
	}
	s := fmt.Sprintf("%d Car Garage", int(cap))
	return &s
}
