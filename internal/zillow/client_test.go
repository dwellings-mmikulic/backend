package zillow

import (
	"encoding/json"
	"testing"
)

func TestToProperty(t *testing.T) {
	l := listing{
		ZPID:          "43590635",
		Price:         "195000",
		StreetAddress: "622 Burning Tree Ln",
		City:          "Punta Gorda",
		State:         "FL",
		Zipcode:       "33982",
		LivingArea:    "1104",
		LotAreaValue:  "8249",
		LotAreaUnit:   "sqft",
		Bedrooms:      "3",
		Bathrooms:     "2",
		ImgSrc:        "https://photos.zillowstatic.com/fp/abc-p_e.jpg",
	}
	l.Carousel.BaseURL = "https://photos.zillowstatic.com/fp/{photoKey}-p_e.jpg"
	l.Carousel.PhotoData = []struct {
		PhotoKey string `json:"photoKey"`
	}{{PhotoKey: "key1"}, {PhotoKey: "key2"}}

	p := toProperty(&l)

	if p.ZPID != "43590635" || p.SalePrice != 195000 {
		t.Errorf("zpid/price wrong: %+v", p)
	}
	if p.Address != "622 Burning Tree Ln" || p.City != "Punta Gorda" || p.Zip != "33982" {
		t.Errorf("address wrong: %+v", p)
	}
	if p.HomeSizeSqft != 1104 || p.LotSizeSqft != 8249 || p.Bedrooms != 3 || p.Bathrooms != 2 {
		t.Errorf("metrics wrong: %+v", p)
	}
	if len(p.ImageURLs) != 2 || p.ImageURLs[0] != "https://photos.zillowstatic.com/fp/key1-cc_ft_1536.jpg" {
		t.Errorf("carousel photo build wrong: %v", p.ImageURLs)
	}
}

func TestPhotoURLs_FallbackToImgSrc(t *testing.T) {
	l := listing{ImgSrc: "https://x/y.jpg"}
	got := photoURLs(&l)
	if len(got) != 1 || got[0] != "https://x/y.jpg" {
		t.Errorf("fallback failed: %v", got)
	}
}

func TestLotToSqft_Acres(t *testing.T) {
	if got := lotToSqft(json.Number("0.5"), "acres"); got != 21780 {
		t.Errorf("acres conversion = %d, want 21780", got)
	}
}

// TestSearchResponseDecode verifies the envelope decodes a real-shaped payload.
func TestSearchResponseDecode(t *testing.T) {
	const body = `{"status":"OK","request_id":"x","parameters":{},"data":[{"zpid":"1","price":195000,"streetAddress":"1 Main","city":"Tampa","state":"FL","zipcode":"33601","livingArea":1000,"lotAreaValue":5000,"lotAreaUnit":"sqft","bedrooms":3,"bathrooms":2,"imgSrc":"https://x/t.jpg"}]}`
	var r searchResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != "OK" || len(r.Data) != 1 || r.Data[0].ZPID != "1" {
		t.Errorf("decoded wrong: %+v", r)
	}
}
