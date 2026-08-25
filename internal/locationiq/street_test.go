package locationiq

import "testing"

func TestStripUnit(t *testing.T) {
	cases := map[string]string{
		"3559 W Montrose Ave #4E":    "3559 W Montrose Ave",
		"3559 W Montrose Ave # 4E":   "3559 W Montrose Ave",
		"123 Main St Apt 5":          "123 Main St",
		"123 Main St, Apt. 5B":       "123 Main St",
		"123 Main St Unit B":         "123 Main St",
		"100 Oak Ave, Ste 200":       "100 Oak Ave",
		"100 Oak Ave Suite 200":      "100 Oak Ave",
		"9 Park Pl Fl 3":             "9 Park Pl",
		"1234 Hilltop Drive":         "1234 Hilltop Drive",
		"5 Unit Rd":                  "5 Unit Rd",
		"77 Space Center Blvd":       "77 Space Center Blvd",
		"The Harris Plan, Paramount": "The Harris Plan, Paramount",
		"  12 Elm St #2  ":           "12 Elm St",
		"":                           "",
	}
	for in, want := range cases {
		if got := stripUnit(in); got != want {
			t.Errorf("stripUnit(%q) = %q, want %q", in, got, want)
		}
	}
}
