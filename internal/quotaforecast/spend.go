package quotaforecast

import (
	"math"
	"sort"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

const (
	hourMs = int64(3600 * 1000)
	// Readings further apart than this are a gap (server down): their spend is
	// still counted, but the hours they cover are not training targets.
	maxReadingGapMs = 2 * hourMs
	// Reset instants this close are the same window.
	resetGroupMs = int64(60 * 1000)
	// A pool below this share of its observed capacity is dry: spend was capped
	// by quota, not chosen.
	dryShare = 0.01
)

type sample struct {
	at    int64
	value float64
}

// samples converts weekly readings to remaining base-plan units. A window that
// rolled between two readings becomes a vertical refill at the old reset
// instant (same rule as the dashboard's historySamples).
func samples(readings []quotahistory.Reading, units float64) []sample {
	out := make([]sample, 0, len(readings))
	toUnits := func(used float64) float64 {
		return (100 - math.Min(100, math.Max(0, used))) / 100 * units
	}
	for i, r := range readings {
		at, used, reset := int64(r[0]), r[1], int64(r[2])
		if i > 0 {
			prevAt, prevUsed, prevReset := int64(readings[i-1][0]), readings[i-1][1], int64(readings[i-1][2])
			if prevReset > 0 && reset > 0 && reset-prevReset > 2*resetGroupMs && prevReset > prevAt && prevReset < at {
				out = append(out, sample{prevReset, toUnits(prevUsed)}, sample{prevReset, units})
			}
		}
		out = append(out, sample{at, toUnits(used)})
	}
	return out
}

// PoolHourlySpend builds hourly pool spend from every credential's weekly
// readings. units maps a history credential id ("provider:name") to its plan
// units; unknown ids count as 1. Spend between two readings is spread evenly
// over the time between them. A credential contributes nothing before its first
// reading. Only whole hours before endMs are included.
func PoolHourlySpend(credentials map[string]*quotahistory.Credential, units map[string]float64, provider, window string, endMs int64) *HourlySpend {
	type line struct {
		samples []sample
		units   float64
	}
	lines := []line{}
	first, last := int64(math.MaxInt64), int64(0)
	for id, c := range credentials {
		if c == nil || c.Provider != provider || c.Series[window] == nil {
			continue
		}
		readings := append([]quotahistory.Reading(nil), c.Series[window].Readings...)
		sort.Slice(readings, func(i, j int) bool { return readings[i][0] < readings[j][0] })
		u := units[id]
		if !(u > 0) {
			u = 1
		}
		s := samples(readings, u)
		if len(s) < 2 {
			continue
		}
		lines = append(lines, line{s, u})
		first = min(first, s[0].at)
		last = max(last, s[len(s)-1].at)
	}
	if len(lines) == 0 {
		return nil
	}
	start := (first + hourMs - 1) / hourMs * hourMs
	end := min(last, endMs) / hourMs * hourMs
	if end <= start {
		return nil
	}
	buckets := int((end - start) / hourMs)
	rates := make([]float64, buckets)
	gap := make([]bool, buckets)
	remaining := make([]float64, buckets+1)
	capacity := make([]float64, buckets+1)

	for _, l := range lines {
		s := l.samples
		for i := 1; i < len(s); i++ {
			a, b := s[i-1], s[i]
			if b.at <= a.at {
				continue
			}
			spent := math.Max(0, a.value-b.value)
			from, to := max(a.at, start), min(b.at, end)
			if to <= from {
				continue
			}
			perMs := spent / float64(b.at-a.at)
			long := b.at-a.at > maxReadingGapMs
			for k := int((from - start) / hourMs); k < buckets; k++ {
				bucketFrom := start + int64(k)*hourMs
				bucketTo := bucketFrom + hourMs
				if bucketFrom >= to {
					break
				}
				overlap := min(bucketTo, to) - max(bucketFrom, from)
				if overlap <= 0 {
					continue
				}
				rates[k] += perMs * float64(overlap)
				if long {
					gap[k] = true
				}
			}
		}
		// Remaining units at each bucket boundary, for credentials observed by then.
		for k := 0; k <= buckets; k++ {
			at := start + int64(k)*hourMs
			if at < s[0].at {
				continue
			}
			remaining[k] += valueAt(s, at)
			capacity[k] += l.units
		}
	}
	usable := make([]bool, buckets)
	for k := range usable {
		dry := math.Min(remaining[k], remaining[k+1])
		usable[k] = !gap[k] && capacity[k] > 0 && dry > dryShare*capacity[k]
	}
	return &HourlySpend{StartMs: start, StepMs: hourMs, Rates: rates, Usable: usable}
}

// valueAt interpolates a sample line at t, taking the value after a vertical
// jump and holding the last value past the end.
func valueAt(s []sample, t int64) float64 {
	idx := sort.Search(len(s), func(i int) bool { return s[i].at > t })
	if idx == 0 {
		return s[0].value
	}
	if idx == len(s) {
		return s[len(s)-1].value
	}
	a, b := s[idx-1], s[idx]
	if b.at == a.at {
		return b.value
	}
	return a.value + (b.value-a.value)*float64(t-a.at)/float64(b.at-a.at)
}
