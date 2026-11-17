package quotaforecast

import "github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"

const (
	// Cost-like token weights relative to uncached input, close to list-price
	// ratios for both providers. Only their relative size matters: the
	// units-per-token ratio absorbs the absolute scale.
	cacheReadWeight  = 0.1
	cacheWriteWeight = 1.25
	outputWeight     = 5
	// ratioWindowHours is how much recent overlap measures units per token.
	ratioWindowHours = 7 * 24
	// minRatioHours is the least overlap a ratio is trusted from.
	minRatioHours = 24
)

// WeightedTokens is one request's token demand in uncached-input equivalents.
func WeightedTokens(t quotahistory.TokenPoint) float64 {
	uncached := t.Input - t.CacheRead - t.CacheWrite
	if uncached < 0 {
		uncached = 0
	}
	w := float64(uncached) + cacheWriteWeight*float64(t.CacheWrite) + cacheReadWeight*float64(t.CacheRead) + outputWeight*float64(t.Output)
	if w < 0 {
		return 0
	}
	return w
}

// HourlyTokens buckets one provider's weighted tokens (millions per hour) by
// whole clock hours from its first record up to endMs.
func HourlyTokens(points []quotahistory.TokenPoint, provider string, endMs int64) *HourlySpend {
	first := int64(-1)
	for _, p := range points {
		if p.Provider == provider && (first < 0 || p.At < first) {
			first = p.At
		}
	}
	if first < 0 {
		return nil
	}
	start := first / hourMs * hourMs
	end := endMs / hourMs * hourMs
	if end <= start {
		return nil
	}
	n := int((end - start) / hourMs)
	data := &HourlySpend{StartMs: start, StepMs: hourMs, Rates: make([]float64, n), Usable: make([]bool, n)}
	for _, p := range points {
		if p.Provider != provider || p.At < start || p.At >= end {
			continue
		}
		data.Rates[(p.At-start)/hourMs] += WeightedTokens(p) / 1e6
	}
	for i := range data.Usable {
		data.Usable[i] = true
	}
	return data
}

// UnitsPerToken measures quota units spent per weighted token over the latest
// overlap of the two series (usable spend hours only). Zero when unknown.
func UnitsPerToken(tokens, spend *HourlySpend, hours int) (ratio float64, overlap int) {
	if tokens == nil || spend == nil || tokens.StepMs != spend.StepMs {
		return 0, 0
	}
	sumSpend, sumTokens := 0.0, 0.0
	for k := len(spend.Rates) - 1; k >= 0 && overlap < hours; k-- {
		if !spend.Usable[k] {
			continue
		}
		at := spend.StartMs + int64(k)*spend.StepMs
		i := int((at - tokens.StartMs) / tokens.StepMs)
		if at < tokens.StartMs || i >= len(tokens.Rates) {
			continue
		}
		sumSpend += spend.Rates[k]
		sumTokens += tokens.Rates[i]
		overlap++
	}
	if overlap < minRatioHours || !(sumTokens > 0) {
		return 0, overlap
	}
	return sumSpend / sumTokens, overlap
}

// MarkDryHours flags token hours where the quota pool ran dry as unusable:
// demand was capped there, so token counts understate it.
func MarkDryHours(tokens, spend *HourlySpend) {
	if tokens == nil || spend == nil {
		return
	}
	for k, ok := range spend.Usable {
		if ok {
			continue
		}
		at := spend.StartMs + int64(k)*spend.StepMs
		if i := int((at - tokens.StartMs) / tokens.StepMs); at >= tokens.StartMs && i < len(tokens.Usable) {
			tokens.Usable[i] = false
		}
	}
}
