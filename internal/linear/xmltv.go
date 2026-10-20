package linear

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"time"
)

// xmltvTime is the timestamp layout of the sample guide this feed mirrors
// (Freebie TV / Amagi): ISO 8601 with milliseconds and a +0000 offset, not
// the bare "20261019090000 +0000" of the XMLTV DTD.
const xmltvTime = "2006-01-02T15:04:05.000-0700"

// XMLTV document, mapped to the elements the sample feed carries.
type xmltvTV struct {
	XMLName    xml.Name         `xml:"tv"`
	Date       string           `xml:"date,attr"`
	SourceInfo string           `xml:"source-info-name,attr"`
	Generator  string           `xml:"generator-info-name,attr"`
	Channel    xmltvChannel     `xml:"channel"`
	Programmes []xmltvProgramme `xml:"programme"`
}

type xmltvChannel struct {
	ID          string    `xml:"id,attr"`
	DisplayName xmltvText `xml:"display-name"`
}

type xmltvText struct {
	Lang string `xml:"lang,attr"`
	Text string `xml:",chardata"`
}

type xmltvProgramme struct {
	Start      string      `xml:"start,attr"`
	Stop       string      `xml:"stop,attr"`
	Channel    string      `xml:"channel,attr"`
	Title      xmltvText   `xml:"title"`
	Desc       xmltvText   `xml:"desc"`
	Categories []xmltvText `xml:"category"`
	Language   string      `xml:"language"`
	Length     xmltvLength `xml:"length"`
	EpisodeNum xmltvEpNum  `xml:"episode-num"`
	Video      xmltvVideo  `xml:"video"`
	Audio      xmltvAudio  `xml:"audio"`
}

type xmltvLength struct {
	Units string `xml:"units,attr"`
	Value string `xml:",chardata"`
}

type xmltvEpNum struct {
	System string `xml:"system,attr"`
	Value  string `xml:",chardata"`
}

type xmltvVideo struct {
	Present string `xml:"present"`
	Colour  string `xml:"colour"`
	Quality string `xml:"quality"`
}

type xmltvAudio struct {
	Present string `xml:"present"`
	Stereo  string `xml:"stereo"`
}

// renderXMLTV is the XMLTV guide of e: one programme per programBlock from
// the block on air at now up to until, with no gaps. A guide ingest wants a
// continuous schedule days ahead, while the lineup chain only reaches
// EPGHorizonHours and is extended lazily, so blocks e already covers carry
// its listing summary and the rest describe the channel in general terms.
// That is what makes a guide possible for a channel whose content changes by
// the minute: every block is the same show — the area's homes for sale — and
// only its details are decided close to air time. note is appended to every
// description ("" for none).
func renderXMLTV(e EPG, now, until time.Time, note string) []byte {
	now, until = now.UTC(), until.UTC()
	sc, _ := ParseKey(e.Channel.Scope)
	key := e.Channel.Key

	known := make(map[time.Time]Program, len(e.Programs))
	for _, p := range e.Programs {
		known[p.Start.UTC()] = p
	}

	doc := xmltvTV{
		Date:       now.Format("2006-01-02"),
		SourceInfo: "Dwellings",
		Generator:  "dwellings-backend",
		Channel:    xmltvChannel{ID: key, DisplayName: xmltvText{Lang: "en", Text: e.Channel.Name}},
	}
	minutes := strconv.Itoa(int(programBlock / time.Minute))
	for b := now.Truncate(programBlock).Add(-programBlock); b.Before(until); b = b.Add(programBlock) {
		desc := sc.guideDescription()
		if p, ok := known[b]; ok {
			desc = p.Description
		}
		doc.Programmes = append(doc.Programmes, xmltvProgramme{
			Start:   b.Format(xmltvTime),
			Stop:    b.Add(programBlock).Format(xmltvTime),
			Channel: key,
			Title:   xmltvText{Lang: "en", Text: e.Channel.Name},
			Desc:    xmltvText{Lang: "en", Text: desc + note},
			Categories: []xmltvText{
				{Lang: "en", Text: "Lifestyle"},
				{Lang: "en", Text: "Real Estate"},
			},
			Language:   "English",
			Length:     xmltvLength{Units: "minutes", Value: minutes},
			EpisodeNum: xmltvEpNum{System: "assetID", Value: key + "@" + b.Format("20060102T1504")},
			Video:      xmltvVideo{Present: "yes", Colour: "yes", Quality: "1920x1080"},
			Audio:      xmltvAudio{Present: "yes", Stereo: "stereo"},
		})
	}

	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<!DOCTYPE tv SYSTEM "xmltv.dtd">` + "\n")
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	// Encoding a struct of strings cannot fail; the error is still checked
	// so a future change to the types is not silently truncated.
	if err := enc.Encode(doc); err != nil {
		panic("xmltv: " + err.Error())
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// guideDescription is the standing description of a block whose lineup is
// not materialised yet.
func (s Scope) guideDescription() string {
	return "The newest homes for sale in " + s.areaName() + ": photos, price and details of every listing, around the clock."
}
