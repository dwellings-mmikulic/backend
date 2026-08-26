// Package geo maps a client IP to a US location using a MaxMind GeoLite2
// City database. The lookup is offline and best-effort.
package geo

import (
	"fmt"
	"net"

	"github.com/oschwald/geoip2-golang"
)

// Location is what the channel resolver needs. Fields may be empty.
type Location struct {
	Zip   string
	City  string
	State string // 2-letter code, lowercase
}

// Lookup locates IPs.
type Lookup interface {
	// Locate returns the location of ip and whether anything useful was
	// found (a US ZIP, city or state).
	Locate(ip net.IP) (Location, bool)
}

// DB is a Lookup over an .mmdb file.
type DB struct {
	reader *geoip2.Reader
}

// Open loads the database at path.
func Open(path string) (*DB, error) {
	r, err := geoip2.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open geoip db %s: %w", path, err)
	}
	return &DB{reader: r}, nil
}

// Close releases the file.
func (d *DB) Close() error { return d.reader.Close() }

// Locate implements Lookup. Only US results are reported: the channels are
// US-only, so a foreign viewer gets the national default.
func (d *DB) Locate(ip net.IP) (Location, bool) {
	if ip == nil {
		return Location{}, false
	}
	rec, err := d.reader.City(ip)
	if err != nil || rec == nil || rec.Country.IsoCode != "US" {
		return Location{}, false
	}
	loc := Location{Zip: rec.Postal.Code, City: rec.City.Names["en"]}
	if len(rec.Subdivisions) > 0 {
		loc.State = lower(rec.Subdivisions[0].IsoCode)
	}
	return loc, loc.Zip != "" || (loc.City != "" && loc.State != "") || loc.State != ""
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
