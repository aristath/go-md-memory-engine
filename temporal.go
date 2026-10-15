package memory

import (
	"errors"
	"fmt"
	"time"
)

// Temporal preserves supplied calendar precision. Date is a single year, month,
// or day. Start/End are inclusive range bounds; an omitted bound is open.
// Approximate/Unknown/Description are never replaced with invented dates.
type Temporal struct {
	Date        string `json:"date,omitempty"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Approximate bool   `json:"approximate,omitempty"`
	Unknown     bool   `json:"unknown,omitempty"`
	Description string `json:"description,omitempty"`
}

func dateBounds(s string) (int64, int64, error) {
	layout := "2006-01-02"
	switch len(s) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
	default:
		return 0, 0, fmt.Errorf("invalid calendar date %q (use YYYY, YYYY-MM or YYYY-MM-DD)", s)
	}
	t, e := time.Parse(layout, s)
	if e != nil || t.Year() < 1 || t.Year() > 9999 {
		return 0, 0, fmt.Errorf("invalid calendar date %q", s)
	}
	end := t.AddDate(0, 0, 1)
	if len(s) == 4 {
		end = t.AddDate(1, 0, 0)
	} else if len(s) == 7 {
		end = t.AddDate(0, 1, 0)
	}
	return t.Unix(), end.Unix() - 1, nil
}
func (t Temporal) bounds() (int64, int64, error) {
	if t.Date != "" {
		if t.Start != "" || t.End != "" || t.Unknown {
			return 0, 0, errors.New("date cannot coexist with range bounds or unknown")
		}
		return dateBounds(t.Date)
	}
	if t.Unknown {
		if t.Start != "" || t.End != "" {
			return 0, 0, errors.New("unknown cannot have date bounds")
		}
		return 0, 0, nil
	}
	if t.Start == "" && t.End == "" {
		return 0, 0, errors.New("time requires date, at least one range bound, or unknown:true")
	}
	lo, hi := int64(-62135596800), int64(253402300799)
	var e error
	if t.Start != "" {
		lo, _, e = dateBounds(t.Start)
		if e != nil {
			return 0, 0, e
		}
	}
	if t.End != "" {
		_, hi, e = dateBounds(t.End)
		if e != nil {
			return 0, 0, e
		}
	}
	if lo > hi {
		return 0, 0, errors.New("time range is reversed")
	}
	return lo, hi, nil
}

// compareTime treats calendar precision/ranges as possible event times. A year
// overlaps a particular month only possibly; it is certainly within that year.
// Approximate bounds establish possible matches, never certain exclusions.
func compareTime(a, b Temporal, op string) string {
	if a.Unknown || b.Unknown {
		return "unknown"
	}
	al, ah, _ := a.bounds()
	bl, bh, _ := b.bounds()
	result := "false"
	switch op {
	case "overlaps":
		if al >= bl && ah <= bh {
			result = "true"
		} else if al <= bh && ah >= bl {
			result = "possible"
		}
	case "before":
		if ah < bl {
			result = "true"
		} else if al < bh {
			result = "possible"
		}
	case "after":
		if al > bh {
			result = "true"
		} else if ah > bl {
			result = "possible"
		}
	}
	if a.Approximate || b.Approximate {
		if result != "false" {
			return "possible"
		}
		return "unknown"
	}
	return result
}
