package zillow

import (
	"encoding/json"
	"strconv"
	"strings"
)

// number is a tolerant numeric field. The provider is inconsistent: a value
// may arrive as a JSON number, as a quoted number, as "" or as null. All of
// the non-numeric forms decode to the empty string, which toFloat reads as 0,
// instead of failing the entire response.
type number string

func (n *number) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		*n = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var q string
		if err := json.Unmarshal(b, &q); err != nil {
			return err
		}
		s = strings.TrimSpace(q)
	}
	if s == "" {
		*n = ""
		return nil
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		// Unparseable content is treated as absent rather than fatal.
		*n = ""
		return nil
	}
	*n = number(s)
	return nil
}

func toInt(n number) int     { return int(toFloat(n)) }
func toInt64(n number) int64 { return int64(toFloat(n)) }

func toFloat(n number) float64 {
	if n == "" {
		return 0
	}
	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return 0
	}
	return f
}
