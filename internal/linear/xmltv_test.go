package linear

import (
	"context"
	"encoding/xml"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// xmltvDoc is the subset of XMLTV the tests read back.
type xmltvDoc struct {
	XMLName  xml.Name `xml:"tv"`
	Date     string   `xml:"date,attr"`
	Channels []struct {
		ID   string `xml:"id,attr"`
		Name string `xml:"display-name"`
	} `xml:"channel"`
	Programmes []struct {
		Start    string   `xml:"start,attr"`
		Stop     string   `xml:"stop,attr"`
		Channel  string   `xml:"channel,attr"`
		Title    string   `xml:"title"`
		Desc     string   `xml:"desc"`
		Category []string `xml:"category"`
		Length   string   `xml:"length"`
		Asset    string   `xml:"episode-num"`
	} `xml:"programme"`
}

// The sample feed (Freebie TV / Amagi) stamps times as
// 2026-10-19T09:00:00.000+0000; ingest expects exactly that shape.
var xmltvStamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.000\+0000$`)

func parseXMLTV(t *testing.T, body []byte) xmltvDoc {
	t.Helper()
	var doc xmltvDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, body)
	}
	return doc
}

func TestXMLTV_ContinuousBlocksToTheGuideHorizon(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40) // 40 min of content per version: far short of 7 days
	now := t0.Add(7 * time.Minute)
	s := testService(m, now)
	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, now)
	if err != nil {
		t.Fatal(err)
	}
	until := now.Add(7 * 24 * time.Hour)

	body := renderXMLTV(e, now, until, "")
	doc := parseXMLTV(t, body)

	if doc.Date != "2026-08-25" {
		t.Errorf("tv date = %q", doc.Date)
	}
	if len(doc.Channels) != 1 || doc.Channels[0].ID != "zip:77494" || doc.Channels[0].Name != "Homes for sale in 77494" {
		t.Fatalf("channels = %+v", doc.Channels)
	}
	// Every 30-minute block from the one on air to the horizon, no gaps,
	// no overlaps: a guide ingest wants a continuous schedule, not just the
	// blocks whose lineup already exists.
	// The horizon falls inside a block; that block is still listed.
	start := now.Truncate(programBlock).Add(-programBlock)
	want := int((until.Sub(start) + programBlock - 1) / programBlock)
	if len(doc.Programmes) != want {
		t.Fatalf("programmes = %d, want %d", len(doc.Programmes), want)
	}
	if doc.Programmes[0].Start != "2026-08-25T11:30:00.000+0000" {
		t.Errorf("first start = %q", doc.Programmes[0].Start)
	}
	for i, p := range doc.Programmes {
		if !xmltvStamp.MatchString(p.Start) || !xmltvStamp.MatchString(p.Stop) {
			t.Fatalf("programme %d stamps %q–%q", i, p.Start, p.Stop)
		}
		if i > 0 && p.Start != doc.Programmes[i-1].Stop {
			t.Fatalf("programme %d starts at %s, previous stopped at %s", i, p.Start, doc.Programmes[i-1].Stop)
		}
		if p.Channel != "zip:77494" || p.Title != "Homes for sale in 77494" || p.Length != "30" {
			t.Errorf("programme %d = %+v", i, p)
		}
		if p.Desc == "" || p.Asset == "" {
			t.Errorf("programme %d has empty desc or asset id: %+v", i, p)
		}
		if len(p.Category) == 0 {
			t.Errorf("programme %d has no category", i)
		}
	}
	// Every block carries the same static description: what airs in a slot
	// varies per viewer (personal feeds) and changes by the minute, so no
	// block is described by its listings.
	for i, p := range doc.Programmes {
		if p.Desc != "The newest homes for sale in 77494: photos, price and details of every listing, around the clock" {
			t.Fatalf("programme %d desc = %q", i, p.Desc)
		}
	}
	// Asset ids are unique per block.
	seen := map[string]bool{}
	for _, p := range doc.Programmes {
		if seen[p.Asset] {
			t.Fatalf("duplicate asset id %q", p.Asset)
		}
		seen[p.Asset] = true
	}
}

func TestXMLTV_NoteReachesEveryBlockAndIsEscaped(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	s := testService(m, t0)
	e, err := s.EPG(context.Background(), Scope{Zip: "77494"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	body := renderXMLTV(e, t0, t0.Add(2*time.Hour), " · Customize at tv.example.test/tv?a=1&b=2")
	doc := parseXMLTV(t, body)
	for i, p := range doc.Programmes {
		if !strings.HasSuffix(p.Desc, "tv.example.test/tv?a=1&b=2") {
			t.Errorf("programme %d desc = %q", i, p.Desc)
		}
	}
	if strings.Contains(string(body), "a=1&b=2") {
		t.Error("raw ampersand in XML body")
	}
}

func TestHandler_EPGXML(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	rec := serve(t, m, t0, "/channels/epg.xml?zip=77494")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/xml; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("cache control = %q", cc)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("missing CORS header")
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<!DOCTYPE tv SYSTEM "xmltv.dtd">`+"\n") {
		t.Errorf("body prologue:\n%.200s", body)
	}
	doc := parseXMLTV(t, rec.Body.Bytes())
	// Default guide: 7 days ahead, regardless of how far the lineup reaches.
	want := int((7*24*time.Hour + programBlock) / programBlock)
	if len(doc.Programmes) != want {
		t.Errorf("programmes = %d, want %d", len(doc.Programmes), want)
	}
	if strings.Contains(body, "Customize") {
		t.Error("note must not appear without feeds enabled")
	}

	rec = serve(t, m, t0, "/channels/epg.xml?state=zz")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown area status = %d", rec.Code)
	}
}

func TestHandler_EPGXMLCarriesTheCustomizeURL(t *testing.T) {
	m := newMemStore()
	addClips(m, katy, 1, 40)
	_, mux := feedMux(m, t0)
	rec := do(mux, http.MethodGet, "/channels/epg.xml?zip=77494", "203.0.113.9", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	doc := parseXMLTV(t, rec.Body.Bytes())
	for i, p := range doc.Programmes {
		if !strings.HasSuffix(p.Desc, " · Customize this channel at tv.example.test/tv") {
			t.Fatalf("programme %d desc = %q", i, p.Desc)
		}
	}
}
