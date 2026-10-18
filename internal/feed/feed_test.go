package feed

import (
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

func TestTitle(t *testing.T) {
	tests := []struct {
		name string
		p    property.Property
		want string
	}{
		{"price and address", property.Property{SalePrice: 310000, Address: "23265 McBurney Ave"}, "$310,000 · 23265 McBurney Ave"},
		{"no price", property.Property{Address: "1 Main St"}, "Contact for price · 1 Main St"},
		{"no address", property.Property{SalePrice: 1250000}, "$1,250,000"},
	}
	for _, tt := range tests {
		if got := title(&tt.p); got != tt.want {
			t.Errorf("%s: title = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestShortDescription(t *testing.T) {
	tests := []struct {
		name string
		p    property.Property
		want string
	}{
		{"all facts", property.Property{Bedrooms: 3, Bathrooms: 2.5, HomeSizeSqft: 1779, City: "Punta Gorda", State: "FL"}, "3 bd · 2.5 ba · 1,779 sqft — Punta Gorda, FL"},
		{"whole baths", property.Property{Bedrooms: 2, Bathrooms: 2}, "2 bd · 2 ba"},
		{"place only", property.Property{City: "Austin", State: "TX"}, "Austin, TX"},
		{"state only", property.Property{State: "TX"}, "TX"},
		{"nothing", property.Property{}, ""},
	}
	for _, tt := range tests {
		if got := shortDescription(&tt.p); got != tt.want {
			t.Errorf("%s: shortDescription = %q, want %q", tt.name, got, tt.want)
		}
	}
}
