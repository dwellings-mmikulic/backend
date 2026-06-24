package zillow

import "testing"

func TestHiResZillow(t *testing.T) {
	cases := map[string]string{
		"https://photos.zillowstatic.com/fp/abc-p_e.jpg":                          "https://photos.zillowstatic.com/fp/abc-cc_ft_1536.jpg",
		"https://photos.zillowstatic.com/fp/abc-uncropped_scaled_within_1024.jpg": "https://photos.zillowstatic.com/fp/abc-cc_ft_1536.jpg",
		"https://other.example.com/x-p_e.jpg":                                     "https://other.example.com/x-p_e.jpg", // non-zillow untouched
	}
	for in, want := range cases {
		if got := hiResZillow(in); got != want {
			t.Errorf("hiResZillow(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPhotoURLs_UpgradesResolution(t *testing.T) {
	l := listing{}
	l.Carousel.BaseURL = "https://photos.zillowstatic.com/fp/{photoKey}-p_e.jpg"
	l.Carousel.PhotoData = []struct {
		PhotoKey string `json:"photoKey"`
	}{{PhotoKey: "k1"}}
	got := photoURLs(&l)
	if len(got) != 1 || got[0] != "https://photos.zillowstatic.com/fp/k1-cc_ft_1536.jpg" {
		t.Errorf("photoURLs = %v", got)
	}
}
