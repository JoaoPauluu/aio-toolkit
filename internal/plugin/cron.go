package plugin

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronSpec is a parsed five-field cron expression:
// minute hour day-of-month month day-of-week (0 or 7 = Sunday).
// Each field accepts *, numbers, ranges (1-5), lists (1,3,5) and steps (*/15, 0-30/5).
// The @hourly, @daily, @weekly and @monthly shortcuts are accepted too.
type CronSpec struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
}

var cronAliases = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
	"@yearly":  "0 0 1 1 *",
}

func ParseCron(expr string) (*CronSpec, error) {
	expr = strings.TrimSpace(expr)
	if a, ok := cronAliases[expr]; ok {
		expr = a
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron %q: want 5 fields (minute hour day month weekday)", expr)
	}
	var s CronSpec
	var err error
	if s.minute, err = cronField(f[0], 0, 59); err != nil {
		return nil, err
	}
	if s.hour, err = cronField(f[1], 0, 23); err != nil {
		return nil, err
	}
	if s.dom, err = cronField(f[2], 1, 31); err != nil {
		return nil, err
	}
	if s.month, err = cronField(f[3], 1, 12); err != nil {
		return nil, err
	}
	if s.dow, err = cronField(f[4], 0, 7); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1 // 7 is Sunday too
	}
	s.domStar, s.dowStar = f[2] == "*", f[4] == "*"
	return &s, nil
}

func cronField(f string, lo, hi int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("cron: bad step %q", part)
			}
			step = n
		}
		a, b := lo, hi
		if rng != "*" {
			x, y, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = strconv.Atoi(x); err != nil {
				return 0, fmt.Errorf("cron: bad value %q", part)
			}
			b = a
			if isRange {
				if b, err = strconv.Atoi(y); err != nil {
					return 0, fmt.Errorf("cron: bad range %q", part)
				}
			} else if hasStep {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("cron: %q out of range %d-%d", part, lo, hi)
		}
		for i := a; i <= b; i += step {
			bits |= 1 << uint(i)
		}
	}
	return bits, nil
}

func (s *CronSpec) matches(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 || s.hour&(1<<uint(t.Hour())) == 0 || s.month&(1<<uint(t.Month())) == 0 {
		return false
	}
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	// Classic cron: when both day fields are restricted, either may match.
	if !s.domStar && !s.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Next returns the first minute strictly after t that matches, or the zero
// time if none is found within ~5 years (e.g. "0 0 31 2 *").
func (s *CronSpec) Next(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, t.Location())
	for limit := t.AddDate(5, 0, 0); t.Before(limit); {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if s.matches(t) {
			return t
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
