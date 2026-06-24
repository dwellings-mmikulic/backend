package video

import (
	"slices"
	"strings"
	"testing"

	"github.com/dwellingtw/backend/internal/property"
)

func sampleProperty() *property.Property {
	return &property.Property{
		ZPID:         "43590635",
		SalePrice:    310000,
		Address:      "23265 McBurney Ave",
		City:         "Punta Gorda",
		State:        "FL",
		Zip:          "33980",
		HomeSizeSqft: 1779,
		LotSizeSqft:  9999,
		Bedrooms:     3,
		Bathrooms:    3,
		DetailURL:    "https://www.zillow.com/homedetails/43590635_zpid/",
		ImageURLs:    []string{"https://cdn/a.jpg", "https://cdn/b.jpg"},
	}
}

func TestTextFormatting(t *testing.T) {
	p := sampleProperty()
	if got := priceText(p); got != "$310,000" {
		t.Errorf("priceText = %q", got)
	}
	if got := addressText(p); got != "23265 McBurney Ave, Punta Gorda, FL 33980" {
		t.Errorf("addressText = %q", got)
	}
	if got := factsText(p); got != "3 bd · 3 ba · 1,779 sqft · lot 9,999 sqft" {
		t.Errorf("factsText = %q", got)
	}
}

func TestTrimFloatAndHumanInt(t *testing.T) {
	if trimFloat(3.0) != "3" || trimFloat(2.5) != "2.5" {
		t.Errorf("trimFloat wrong")
	}
	if humanInt(1779) != "1,779" || humanInt(310000) != "310,000" || humanInt(999) != "999" {
		t.Errorf("humanInt wrong")
	}
}

func TestPriceTextUnknown(t *testing.T) {
	p := &property.Property{}
	if got := priceText(p); got != "Contact for price" {
		t.Errorf("priceText empty = %q", got)
	}
}

func TestSelectMusicDeterministic(t *testing.T) {
	r := &Renderer{musicTracks: []string{"/m/a.mp3", "/m/b.mp3", "/m/c.mp3"}}
	first := r.selectMusic("43590635")
	if first == "" || !slices.Contains(r.musicTracks, first) {
		t.Fatalf("selectMusic returned %q", first)
	}
	// Stable across calls (so re-renders keep the same track).
	if again := r.selectMusic("43590635"); again != first {
		t.Errorf("selectMusic not deterministic: %q vs %q", first, again)
	}
	// No tracks → empty.
	empty := &Renderer{}
	if empty.selectMusic("x") != "" {
		t.Errorf("expected empty music when no tracks")
	}
}

func TestContentHashChanges(t *testing.T) {
	p := sampleProperty()
	base := ContentHash(p, 4)
	if base == "" {
		t.Fatal("empty hash")
	}
	if ContentHash(p, 4) != base {
		t.Error("hash not stable for same input")
	}
	// Price change → different hash.
	p2 := sampleProperty()
	p2.SalePrice = 999999
	if ContentHash(p2, 4) == base {
		t.Error("hash unchanged after price change")
	}
	// Photo set change → different hash.
	p3 := sampleProperty()
	p3.ImageURLs = append(p3.ImageURLs, "https://cdn/c.jpg")
	if ContentHash(p3, 4) == base {
		t.Error("hash unchanged after photo change")
	}
	// secondsPerPhoto change → different hash.
	if ContentHash(p, 6) == base {
		t.Error("hash unchanged after seconds change")
	}
}

func TestBuildFFmpegArgs_Full(t *testing.T) {
	s := renderSpec{
		imagePaths:      []string{"/w/0.jpg", "/w/1.jpg"},
		secondsPerPhoto: 4,
		fontPath:        "/font.ttf",
		priceFile:       "/w/price.txt",
		addressFile:     "/w/address.txt",
		factsFile:       "/w/facts.txt",
		caption:         "Scan for details",
		qrPath:          "/w/qr.png",
		musicPath:       "/m/track.mp3",
		outPath:         "/w/out.mp4",
	}
	args := buildFFmpegArgs(s)
	joined := strings.Join(args, " ")

	// Two image inputs, each looped for 4s.
	if strings.Count(joined, "-loop 1 -t 4 -i") != 2 {
		t.Errorf("expected 2 looped image inputs, args: %s", joined)
	}
	// Music looped, audio mapped + trimmed.
	mustContain(t, args, "-stream_loop", "-1")
	mustContain(t, args, "-shortest")
	if !slices.Contains(args, "2:a") {
		t.Errorf("expected music audio map 2:a (idx after 2 images), args: %v", args)
	}
	// QR is the last input (index 3) and composited; final map is [outv].
	mustContain(t, args, "-map", "[outv]")
	mustContain(t, args, "-movflags", "+faststart")

	fc := filterComplexArg(t, args)
	if !strings.Contains(fc, "concat=n=2:v=1:a=0[slide]") {
		t.Errorf("missing concat in filter: %s", fc)
	}
	if !strings.Contains(fc, "[3:v]scale=200:200,pad=224:264:12:12:color=white") {
		t.Errorf("QR should be input index 3 on a white card: %s", fc)
	}
	if !strings.Contains(fc, "Scan for details") {
		t.Errorf("caption should be drawn on the QR card: %s", fc)
	}
	if !strings.Contains(fc, "crop=1920:1080") {
		t.Errorf("missing scale-to-cover crop: %s", fc)
	}
}

func TestBuildFFmpegArgs_NoMusicNoQR(t *testing.T) {
	s := renderSpec{
		imagePaths:      []string{"/w/0.jpg"},
		secondsPerPhoto: 3,
		fontPath:        "/font.ttf",
		priceFile:       "/w/price.txt",
		outPath:         "/w/out.mp4",
	}
	args := buildFFmpegArgs(s)
	if slices.Contains(args, "-shortest") {
		t.Error("no -shortest expected without music")
	}
	// No QR → final video label is [ov].
	mustContain(t, args, "-map", "[ov]")
	fc := filterComplexArg(t, args)
	if strings.Contains(fc, "[qr]") {
		t.Errorf("unexpected QR in filter: %s", fc)
	}
}

func mustContain(t *testing.T, args []string, sub ...string) {
	t.Helper()
	for i := 0; i+len(sub) <= len(args); i++ {
		if slices.Equal(args[i:i+len(sub)], sub) {
			return
		}
	}
	t.Errorf("args missing sequence %v in %v", sub, args)
}

func filterComplexArg(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-filter_complex" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatal("no -filter_complex arg")
	return ""
}
