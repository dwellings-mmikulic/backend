package scheduler

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

func fullListing() property.Property {
	return property.Property{
		ZPID: "2071234567", Address: "123 Retta Esplanade", City: "Punta Gorda", State: "FL", Zip: "33950",
		DetailURL: "https://www.zillow.com/homedetails/2071234567_zpid/",
		SalePrice: 1250000, HomeSizeSqft: 2400, LotSizeSqft: 9100, Bedrooms: 4, Bathrooms: 2.5,
		ImageURLs: []string{"https://photos.zillowstatic.com/a.jpg", "https://photos.zillowstatic.com/b.webp"},
	}
}

func TestEncodeListing_RoundTrips(t *testing.T) {
	for _, revisit := range []bool{false, true} {
		want := fullListing()
		payload, err := EncodeListing(&want, revisit)
		if err != nil {
			t.Fatal(err)
		}
		got, gotRevisit, err := DecodeListing(payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if gotRevisit != revisit {
			t.Errorf("revisit = %v, want %v", gotRevisit, revisit)
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("round trip changed the listing:\n got %+v\nwant %+v", *got, want)
		}
	}
}

// The payload is a stored format: rows enqueued by one build are decoded by
// the next, and by backfill-videos. Pin the keys.
func TestEncodeListing_UsesExplicitSnakeCaseKeys(t *testing.T) {
	p := fullListing()
	payload, err := EncodeListing(&p, true)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{
		"address", "bathrooms", "bedrooms", "city", "detail_url", "home_size_sqft",
		"image_urls", "lot_size_sqft", "revisit", "sale_price", "state", "zip", "zpid",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("payload keys = %v\nwant %v", keys, want)
	}

	payload, err = EncodeListing(&p, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "revisit") {
		t.Errorf("revisit must be omitted from a discovery payload: %s", payload)
	}
}

// PostgreSQL stores neither a NUL in text nor \u0000 in jsonb. One such byte
// in a provider string would make the row unstorable, so they are stripped.
func TestEncodeListing_StripsNULFromEveryString(t *testing.T) {
	p := property.Property{
		ZPID: "12\x0034", Address: "1 Main\x00 St", City: "Pun\x00ta", State: "F\x00L", Zip: "33\x00950",
		DetailURL: "https://z.example/\x00x",
		ImageURLs: []string{"https://img.example/\x00a.jpg", "\x00"},
	}
	payload, err := EncodeListing(&p, false)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(payload); strings.Contains(s, `\u0000`) || strings.Contains(s, "\x00") {
		t.Fatalf("payload still carries a NUL: %s", s)
	}
	got, _, err := DecodeListing(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := property.Property{
		ZPID: "1234", Address: "1 Main St", City: "Punta", State: "FL", Zip: "33950",
		DetailURL: "https://z.example/x",
		ImageURLs: []string{"https://img.example/a.jpg", ""},
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v\nwant %+v", *got, want)
	}
	if p.ZPID != "12\x0034" || p.ImageURLs[0] != "https://img.example/\x00a.jpg" {
		t.Error("EncodeListing must not modify the caller's property")
	}
}

func TestDecodeListing(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
		zpid    string
		revisit bool
		images  int
	}{
		{name: "as jsonb renders it: reordered keys, spaces",
			payload: `{"zpid": "7", "revisit": true, "image_urls": ["a", "b"], "bathrooms": 1.5}`,
			zpid:    "7", revisit: true, images: 2},
		{name: "unknown keys from a newer build are ignored",
			payload: `{"zpid":"7","image_urls":[],"some_future_field":1}`, zpid: "7"},
		{name: "null image list", payload: `{"zpid":"7","image_urls":null}`, zpid: "7"},
		{name: "not json", payload: `<html>`, wantErr: true},
		{name: "empty", payload: ``, wantErr: true},
		{name: "wrong shape", payload: `["7"]`, wantErr: true},
		{name: "wrong field type", payload: `{"zpid":7}`, wantErr: true},
		{name: "no zpid", payload: `{"address":"1 Main St"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, revisit, err := DecodeListing([]byte(tt.payload))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decoded %+v, want an error", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.ZPID != tt.zpid || revisit != tt.revisit || len(p.ImageURLs) != tt.images {
				t.Errorf("got zpid=%q revisit=%v images=%d, want %q %v %d",
					p.ZPID, revisit, len(p.ImageURLs), tt.zpid, tt.revisit, tt.images)
			}
		})
	}
}
