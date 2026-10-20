package property

import "testing"

// Zillow occasionally returns absurd sizes (a condo with 5,864,221,440 sqft of
// lot, a house with 40,103 bathrooms). Those rows failed the upsert for weeks
// (int4 encode error / numeric field overflow) and parked in listing_queue.
func TestClampToSchemaDropsValuesTheColumnsCannotHold(t *testing.T) {
	p := Property{
		ZPID:         "108254647",
		HomeSizeSqft: 1050,
		LotSizeSqft:  5864221440, // > int4
		Bedrooms:     2,
		Bathrooms:    40103, // > NUMERIC(4,1)
	}
	p.ClampToSchema()
	if p.LotSizeSqft != 0 {
		t.Errorf("LotSizeSqft = %d, want 0 (unknown)", p.LotSizeSqft)
	}
	if p.Bathrooms != 0 {
		t.Errorf("Bathrooms = %v, want 0 (unknown)", p.Bathrooms)
	}
	if p.HomeSizeSqft != 1050 || p.Bedrooms != 2 {
		t.Errorf("sane fields changed: home=%d beds=%d", p.HomeSizeSqft, p.Bedrooms)
	}
}

func TestClampToSchemaKeepsEverythingThatFits(t *testing.T) {
	cases := []struct {
		name string
		in   Property
	}{
		{"typical", Property{HomeSizeSqft: 2400, LotSizeSqft: 8249, Bedrooms: 3, Bathrooms: 2.5}},
		{"largest storable", Property{HomeSizeSqft: 2147483647, LotSizeSqft: 2147483647, Bedrooms: 2147483647, Bathrooms: 999.9}},
		{"unknown zeros", Property{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in
			got.ClampToSchema()
			if got.HomeSizeSqft != c.in.HomeSizeSqft || got.LotSizeSqft != c.in.LotSizeSqft ||
				got.Bedrooms != c.in.Bedrooms || got.Bathrooms != c.in.Bathrooms {
				t.Errorf("changed: got %+v, want %+v", got, c.in)
			}
		})
	}
}

func TestClampToSchemaTreatsNegativeAsUnknown(t *testing.T) {
	p := Property{HomeSizeSqft: -1, LotSizeSqft: -5, Bedrooms: -2, Bathrooms: -1.5}
	p.ClampToSchema()
	if p.HomeSizeSqft != 0 || p.LotSizeSqft != 0 || p.Bedrooms != 0 || p.Bathrooms != 0 {
		t.Errorf("negatives kept: %+v", p)
	}
}
