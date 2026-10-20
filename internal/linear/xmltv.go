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
	SubTitle   xmltvText   `xml:"sub-title"`
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

// renderXMLTV is the XMLTV guide of e's channel: one programme per
// programBlock from the block on air at now up to until, with no gaps. A
// guide ingest wants a continuous schedule days ahead, while the lineup only
// reaches EPGHorizonHours and what airs in a slot varies per viewer
// (personal feeds) and changes by the minute — so no block is described by
// its listings. Every block is the same show, the area's homes for sale, and
// carries the same copy. That is what makes a guide possible for a channel
// whose content changes constantly.
func renderXMLTV(e EPG, now, until time.Time) []byte {
	now, until = now.UTC(), until.UTC()
	sc, _ := ParseKey(e.Channel.Scope)
	key := e.Channel.Key
	short, long := sc.guideCopy()

	doc := xmltvTV{
		Date:       now.Format("2006-01-02"),
		SourceInfo: "Dwellings",
		Generator:  "dwellings-backend",
		Channel:    xmltvChannel{ID: key, DisplayName: xmltvText{Lang: "en", Text: e.Channel.Name}},
	}
	minutes := strconv.Itoa(int(programBlock / time.Minute))
	for b := now.Truncate(programBlock).Add(-programBlock); b.Before(until); b = b.Add(programBlock) {
		doc.Programmes = append(doc.Programmes, xmltvProgramme{
			Start:    b.Format(xmltvTime),
			Stop:     b.Add(programBlock).Format(xmltvTime),
			Channel:  key,
			Title:    xmltvText{Lang: "en", Text: e.Channel.Name},
			SubTitle: xmltvText{Lang: "en", Text: short},
			Desc:     xmltvText{Lang: "en", Text: long},
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

// guideCopy is the channel copy every guide block carries (Daniel,
// 2026-10-20): the short form, for the sub-title, and the full text, for
// the description. Platforms differ in how much of a description they
// show, so both are sent. On an area channel the area replaces "from across
// the United States".
func (s Scope) guideCopy() (short, long string) {
	where := "from across the United States"
	if s.Key() != "us" {
		where = "in " + s.areaName()
	}
	short = "Explore real estate listings " + where + " with Dwellings.TV"
	long = short + ", featuring an engaging video slideshow of properties nationwide. " +
		"Scan the QR code in the bottom-right corner to access additional details about any listing. " +
		"Customize your viewing experience in real time using your mobile device by scanning the periodic on-screen QR code or visiting http://dwellings.tv/tv to get started immediately. " +
		"Your mobile device must be connected to the same Wi-Fi network as your TV to customize the channel."
	return short, long
}
