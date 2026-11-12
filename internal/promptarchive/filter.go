package promptarchive

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ParseTime accepts a relative age ("90m", "2h", "3d", "1w"), a date
// ("2026-11-01"), a local datetime ("2026-11-01 14:30") or RFC 3339.
func ParseTime(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if n := len(raw); n > 1 {
		if value, errParse := strconv.ParseFloat(raw[:n-1], 64); errParse == nil && value >= 0 {
			unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[raw[n-1]]
			if unit > 0 {
				return now.Add(-time.Duration(value * float64(unit))), nil
			}
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, errParse := time.ParseInLocation(layout, raw, now.Location()); errParse == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use 2h, 3d, 2026-11-01 or RFC 3339)", raw)
}

// ParseFilter reads a Filter from query parameters.
func ParseFilter(q url.Values, now time.Time) (Filter, error) {
	f := Filter{
		Model:    q.Get("model"),
		Key:      q.Get("key"),
		Auth:     q.Get("auth"),
		Protocol: q.Get("protocol"),
		Session:  q.Get("session"),
		Endpoint: q.Get("endpoint"),
		Client:   q.Get("client"),
		Status:   q.Get("status"),
		Query:    q.Get("q"),
		Before:   q.Get("before"),
	}
	f.Deep, _ = strconv.ParseBool(q.Get("deep"))
	f.Internal, _ = strconv.ParseBool(q.Get("internal"))
	if raw := q.Get("limit"); raw != "" {
		limit, errLimit := strconv.Atoi(raw)
		if errLimit != nil {
			return f, fmt.Errorf("invalid limit %q", raw)
		}
		f.Limit = limit
	}
	var errTime error
	if f.Since, errTime = ParseTime(q.Get("since"), now); errTime != nil {
		return f, errTime
	}
	if f.Until, errTime = ParseTime(q.Get("until"), now); errTime != nil {
		return f, errTime
	}
	return f, nil
}

// Values encodes f as query parameters understood by ParseFilter.
func (f Filter) Values() url.Values {
	q := url.Values{}
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	set("model", f.Model)
	set("key", f.Key)
	set("auth", f.Auth)
	set("protocol", f.Protocol)
	set("session", f.Session)
	set("endpoint", f.Endpoint)
	set("client", f.Client)
	set("status", f.Status)
	set("q", f.Query)
	set("before", f.Before)
	if f.Deep {
		q.Set("deep", "true")
	}
	if f.Internal {
		q.Set("internal", "true")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339Nano))
	}
	if !f.Until.IsZero() {
		q.Set("until", f.Until.Format(time.RFC3339Nano))
	}
	return q
}
