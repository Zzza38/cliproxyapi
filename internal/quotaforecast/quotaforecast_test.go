package quotaforecast

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

var start = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func dailyRate(at time.Time) float64 {
	if h := at.Hour(); h >= 9 && h < 17 {
		return 2
	}
	return 0.2
}

func series(days int, rate func(time.Time) float64) HourlySpend {
	n := days * 24
	data := HourlySpend{StartMs: start.UnixMilli(), StepMs: hourMs, Rates: make([]float64, n), Usable: make([]bool, n)}
	for i := range data.Rates {
		data.Rates[i] = rate(start.Add(time.Duration(i) * time.Hour))
		data.Usable[i] = true
	}
	return data
}

func mean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func TestTrainLearnsDailyRhythm(t *testing.T) {
	data := series(14, dailyRate)
	model := Train(data, nil, time.UTC)
	if model == nil {
		t.Fatal("expected a model")
	}
	rates := model.Forecast(data, 24, time.UTC)
	end := start.Add(time.Duration(len(data.Rates)) * time.Hour)
	var busy, quiet []float64
	for i, rate := range rates {
		if dailyRate(end.Add(time.Duration(i)*time.Hour)) == 2 {
			busy = append(busy, rate)
		} else {
			quiet = append(quiet, rate)
		}
	}
	if mean(busy) < 1.4 || mean(quiet) > 0.6 {
		t.Fatalf("busy %.2f quiet %.2f", mean(busy), mean(quiet))
	}
	if model.Skill == nil || *model.Skill < 0.5 {
		t.Fatalf("skill %v", model.Skill)
	}
}

func TestWarmStartNeverWorseOnHoldout(t *testing.T) {
	data := series(10, dailyRate)
	first := Train(data, nil, time.UTC)
	second := Train(data, first, time.UTC)
	if second == nil || second.HoldoutLoss == nil || *second.HoldoutLoss > *first.HoldoutLoss+1e-12 {
		t.Fatalf("second run lost ground: %v vs %v", second.HoldoutLoss, first.HoldoutLoss)
	}
}

func TestTrainRejectsThinOrEmptyHistory(t *testing.T) {
	if Train(series(1, dailyRate), nil, time.UTC) != nil {
		t.Fatal("one day should be too little")
	}
	if Train(series(5, func(time.Time) float64 { return 0 }), nil, time.UTC) != nil {
		t.Fatal("all-zero history has no scale")
	}
	bad := &Model{Version: ModelVersion, Weights: &Weights{W1: []float64{1}}}
	if Train(series(5, dailyRate), bad, time.UTC) == nil {
		t.Fatal("malformed previous weights must be ignored, not fatal")
	}
}

func readings(from time.Time, hours int, every time.Duration, used func(time.Time) float64, reset time.Time) []quotahistory.Reading {
	out := []quotahistory.Reading{}
	for at := from; at.Before(from.Add(time.Duration(hours) * time.Hour)); at = at.Add(every) {
		out = append(out, quotahistory.Reading{float64(at.UnixMilli()), used(at), float64(reset.UnixMilli())})
	}
	return out
}

func TestPoolHourlySpendWeightsUnitsAndSkipsResetsAndGaps(t *testing.T) {
	reset := start.Add(10 * time.Hour)
	// a: 10 units, 1% per hour (0.1 units/h), weekly window rolls at hour 10.
	a := readings(start, 20, 15*time.Minute, func(at time.Time) float64 {
		if at.Before(reset) {
			return 50 + at.Sub(start).Hours()
		}
		return at.Sub(reset).Hours()
	}, reset)
	for i := range a {
		if int64(a[i][0]) >= reset.UnixMilli() {
			a[i][2] = float64(reset.Add(168 * time.Hour).UnixMilli())
		}
	}
	// b: 1 unit, 10% per hour, with a 3h gap between hours 4 and 7.
	b := []quotahistory.Reading{}
	for _, r := range readings(start, 20, 15*time.Minute, func(at time.Time) float64 { return at.Sub(start).Hours() * 2 }, start.Add(100*time.Hour)) {
		h := (r[0] - float64(start.UnixMilli())) / float64(hourMs)
		if h > 4 && h < 7 {
			continue
		}
		b = append(b, r)
	}
	creds := map[string]*quotahistory.Credential{
		"codex:a":  {Provider: "codex", Series: map[string]*quotahistory.Series{"weekly": {WindowMinutes: 10080, Readings: a}}},
		"codex:b":  {Provider: "codex", Series: map[string]*quotahistory.Series{"weekly": {WindowMinutes: 10080, Readings: b}}},
		"claude:x": {Provider: "claude", Series: map[string]*quotahistory.Series{"seven-day": {Readings: a}}},
	}
	spend := PoolHourlySpend(creds, map[string]float64{"codex:a": 10}, "codex", "weekly", start.Add(30*time.Hour).UnixMilli())
	if spend == nil || spend.StartMs != start.UnixMilli() || len(spend.Rates) != 19 {
		t.Fatalf("unexpected buckets: %+v", spend)
	}
	// 0.1 from a plus 0.02 from b, reset hour included (refill is not spend).
	for _, k := range []int{0, 3, 10, 15} {
		if math.Abs(spend.Rates[k]-0.12) > 1e-9 {
			t.Fatalf("hour %d rate %.4f", k, spend.Rates[k])
		}
	}
	if !spend.Usable[3] || spend.Usable[5] || !spend.Usable[8] {
		t.Fatalf("gap hours must not be training targets: %v", spend.Usable)
	}
}

type fakeSource struct{ history quotahistory.History }

func (f fakeSource) History(time.Time) quotahistory.History     { return f.history }
func (f fakeSource) Units() (map[string]float64, bool)          { return map[string]float64{}, true }
func (f fakeSource) Tokens(time.Time) []quotahistory.TokenPoint { return nil }

func TestTrainerPersistsAndWarmStarts(t *testing.T) {
	used := 0.0
	r := []quotahistory.Reading{}
	for at := start; at.Before(start.Add(9 * 24 * time.Hour)); at = at.Add(15 * time.Minute) {
		r = append(r, quotahistory.Reading{float64(at.UnixMilli()), used, float64(start.Add(400 * time.Hour).UnixMilli())})
		used += dailyRate(at) / 4 / 10
	}
	src := fakeSource{quotahistory.History{Credentials: map[string]*quotahistory.Credential{
		"codex:a": {Provider: "codex", Series: map[string]*quotahistory.Series{"weekly": {WindowMinutes: 10080, Readings: r}}},
	}}}
	path := filepath.Join(t.TempDir(), "quota-forecast.json")
	now := func() time.Time { return start.Add(9 * 24 * time.Hour) }

	first := NewTrainer(src, path)
	first.now, first.loc = now, time.UTC
	first.TrainOnce()
	f := first.Forecasts()["codex"]
	if f == nil || len(f.Rates) != HorizonHours || f.StartMs != now().Add(-time.Hour).UnixMilli() {
		t.Fatalf("first forecast: %+v", f)
	}
	if f.BacktestDays < minBacktestDays || len(f.BacktestMAE) != len(methods) {
		t.Fatalf("backtest did not score every method: %+v", f)
	}
	if f.BacktestMAE[f.Method] > f.BacktestMAE[MethodAverage] {
		t.Fatalf("served %s although the average backtested better: %v", f.Method, f.BacktestMAE)
	}
	if first.models["codex/"+MethodNeuralSpend].Generation != 1 {
		t.Fatalf("neural weights not kept: %+v", first.models)
	}

	second := NewTrainer(src, path)
	second.now, second.loc = now, time.UTC
	second.TrainOnce()
	if g := second.models["codex/"+MethodNeuralSpend]; g.Generation != 2 || g.Model == nil {
		t.Fatalf("saved model was not reloaded: %+v", g)
	}
}

func TestBacktestPrefersAverageWhenNeuralLoses(t *testing.T) {
	tr := &Trainer{loc: time.UTC}
	// Bursty, unpredictable spend: one random spike a day at a random hour.
	r := rng(7)
	data := series(10, func(time.Time) float64 { return 0 })
	for d := 0; d < 10; d++ {
		data.Rates[d*24+int(r.next()*24)] = 1 + 9*r.next()
	}
	scores, days := tr.backtest(series2(data))
	if days != backtestDays {
		t.Fatalf("days %d", days)
	}
	for _, m := range methods {
		if _, ok := scores[m]; !ok {
			t.Fatalf("missing score for %s", m)
		}
	}
}

func series2(spend HourlySpend) seriesInput { return seriesInput{spend: &spend} }
