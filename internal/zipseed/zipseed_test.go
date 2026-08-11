package zipseed

import "testing"

func TestParseRows(t *testing.T) {
	rows, err := parseRows()
	if err != nil {
		t.Fatalf("parseRows: %v", err)
	}
	if len(rows) != 29670 {
		t.Fatalf("got %d rows, want 29670", len(rows))
	}
	first := rows[0]
	if len(first.Zip) != 5 || first.Zip[0] != '0' {
		t.Errorf("first zip %q: want 5 chars with leading zero (CSV is sorted)", first.Zip)
	}
	states := map[string]bool{}
	for _, r := range rows {
		if len(r.Zip) != 5 {
			t.Fatalf("zip %q is not 5 chars", r.Zip)
		}
		if r.Population < 0 {
			t.Fatalf("zip %s has negative population %d", r.Zip, r.Population)
		}
		states[r.State] = true
	}
	if len(states) != 51 {
		t.Errorf("got %d states, want 51 (50 + DC)", len(states))
	}
}
