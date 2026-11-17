package quotaforecast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	log "github.com/sirupsen/logrus"
)

// HorizonHours is how far ahead each forecast reaches (the dashboard shows up
// to two weeks ahead and can page one week further).
const HorizonHours = 21 * 24

const (
	// MethodAverage repeats the trailing 24h spend rate.
	MethodAverage = "average"
	// MethodNeuralSpend runs the network on hourly quota spend.
	MethodNeuralSpend = "neural-spend"
	// MethodNeuralTokens runs the network on hourly weighted tokens and converts
	// them to quota units with the recent units-per-token ratio.
	MethodNeuralTokens = "neural-tokens"

	averageHours = 24
	// Backtest: up to this many daily cut points, each scoring the next 24h.
	backtestDays = 7
	// The fewest scored days before a neural method may replace the average.
	minBacktestDays = 3
	// Quota spend history needed before a cut point is scored.
	minSpendHours = 72
)

var methods = []string{MethodAverage, MethodNeuralSpend, MethodNeuralTokens}

// WeeklyWindow is the history series id of each pooled provider.
var WeeklyWindow = map[string]string{"claude": "seven-day", "codex": "weekly"}

// Forecast is one provider's latest prediction.
type Forecast struct {
	// StartMs is the start of the first predicted hour (the end of the last
	// whole recorded hour); Rates[i] covers [StartMs+i*StepMs, +StepMs).
	StartMs int64     `json:"startMs"`
	StepMs  int64     `json:"stepMs"`
	Rates   []float64 `json:"rates"`
	// Method is the one that served Rates: the lowest backtest error wins.
	Method string `json:"method"`
	// BacktestMAE is each method's mean absolute error of the next-24h spend
	// total (units) over BacktestDays recent days, refit at every cut point.
	BacktestMAE  map[string]float64 `json:"backtestMae"`
	BacktestDays int                `json:"backtestDays"`
	// Samples, Skill, Generation and Epochs describe the network behind a
	// neural method (Skill: holdout error reduction against a 24h average).
	Samples     int      `json:"samples,omitempty"`
	Skill       *float64 `json:"skill,omitempty"`
	Generation  int      `json:"generation,omitempty"`
	WarmStart   bool     `json:"warmStart,omitempty"`
	Epochs      int      `json:"epochs,omitempty"`
	TrainedAtMs int64    `json:"trainedAtMs"`
	// UnitsPerMToken is the conversion behind MethodNeuralTokens.
	UnitsPerMToken float64 `json:"unitsPerMToken,omitempty"`
}

// Source supplies history and plan units. Units map "provider:name" ids to
// base-plan units; ready reports whether plan units are known yet.
type Source interface {
	History(now time.Time) quotahistory.History
	Tokens(now time.Time) []quotahistory.TokenPoint
	Units() (units map[string]float64, ready bool)
}

type savedModel struct {
	Model      *Model `json:"model"`
	Generation int    `json:"generation"`
}

// Trainer retrains every provider's models each hour and keeps the forecasts.
type Trainer struct {
	source Source
	path   string
	loc    *time.Location
	now    func() time.Time

	mu        sync.RWMutex
	models    map[string]savedModel // key: provider + "/" + method
	forecasts map[string]*Forecast
}

// NewTrainer loads previously saved weights from path (if any).
func NewTrainer(source Source, path string) *Trainer {
	t := &Trainer{source: source, path: path, loc: time.Local, now: time.Now, models: map[string]savedModel{}, forecasts: map[string]*Forecast{}}
	data, err := os.ReadFile(path)
	if err == nil {
		saved := map[string]savedModel{}
		if errJSON := json.Unmarshal(data, &saved); errJSON != nil {
			log.WithError(errJSON).Warn("ignore unreadable quota forecast models")
		} else {
			t.models = saved
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.WithError(err).Warn("read quota forecast models")
	}
	return t
}

// Forecasts returns the latest forecast per provider.
func (t *Trainer) Forecasts() map[string]*Forecast {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]*Forecast, len(t.forecasts))
	for k, v := range t.forecasts {
		out[k] = v
	}
	return out
}

// Run trains once plan units are known (or after ten minutes), then shortly
// after the top of every hour, until ctx ends.
func (t *Trainer) Run(ctx context.Context) {
	startedAt := t.now()
	for {
		if _, ready := t.source.Units(); ready || t.now().Sub(startedAt) >= 10*time.Minute {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
	for {
		t.TrainOnce()
		now := t.now()
		wait := now.Truncate(time.Hour).Add(time.Hour + time.Minute).Sub(now)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// series is one provider's inputs, optionally cut short for a backtest.
type seriesInput struct {
	spend  *HourlySpend
	tokens *HourlySpend
}

func cut(data *HourlySpend, endMs int64) *HourlySpend {
	if data == nil || endMs <= data.StartMs {
		return nil
	}
	n := min(len(data.Rates), int((endMs-data.StartMs)/data.StepMs))
	return &HourlySpend{StartMs: data.StartMs, StepMs: data.StepMs, Rates: data.Rates[:n], Usable: data.Usable[:n]}
}

// predict returns steps hourly spend rates (units/h) after s.spend ends.
// prev seeds a warm start; the fitted model and ratio are returned for reporting.
func (t *Trainer) predict(method string, s seriesInput, prev *Model, steps int) (rates []float64, model *Model, ratio float64) {
	spend := s.spend
	if spend == nil || len(spend.Rates) == 0 {
		return nil, nil, 0
	}
	switch method {
	case MethodAverage:
		from := max(0, len(spend.Rates)-averageHours)
		sum := 0.0
		for _, r := range spend.Rates[from:] {
			sum += r
		}
		rate := sum / float64(len(spend.Rates)-from)
		rates = make([]float64, steps)
		for i := range rates {
			rates[i] = rate
		}
		return rates, nil, 0
	case MethodNeuralSpend:
		model = Train(*spend, prev, t.loc)
		if model == nil {
			return nil, nil, 0
		}
		return model.Forecast(*spend, steps, t.loc), model, 0
	case MethodNeuralTokens:
		// Tokens must end where spend ends so the forecasts line up.
		tokens := cut(s.tokens, spend.StartMs+int64(len(spend.Rates))*spend.StepMs)
		if tokens == nil || tokens.StartMs+int64(len(tokens.Rates))*tokens.StepMs != spend.StartMs+int64(len(spend.Rates))*spend.StepMs {
			return nil, nil, 0
		}
		ratio, _ = UnitsPerToken(tokens, spend, ratioWindowHours)
		if !(ratio > 0) {
			return nil, nil, 0
		}
		model = Train(*tokens, prev, t.loc)
		if model == nil {
			return nil, nil, 0
		}
		rates = model.Forecast(*tokens, steps, t.loc)
		for i := range rates {
			rates[i] *= ratio
		}
		return rates, model, ratio
	}
	return nil, nil, 0
}

// backtest refits every method at up to backtestDays daily cut points before
// the end of s and scores each next-24h spend total. Methods that could not
// predict at a cut fall back to the average there, so every method is scored
// on the same days.
func (t *Trainer) backtest(s seriesInput) (map[string]float64, int) {
	errs := map[string]float64{}
	days := 0
	n := len(s.spend.Rates)
	for k := backtestDays; k >= 1; k-- {
		at := n - k*24
		if at < minSpendHours {
			continue
		}
		endMs := s.spend.StartMs + int64(at)*s.spend.StepMs
		trial := seriesInput{spend: cut(s.spend, endMs), tokens: s.tokens}
		actual := 0.0
		for _, r := range s.spend.Rates[at : at+24] {
			actual += r
		}
		fallback, _, _ := t.predict(MethodAverage, trial, nil, 24)
		for _, method := range methods {
			rates, _, _ := t.predict(method, trial, nil, 24)
			if len(rates) != 24 {
				rates = fallback
			}
			total := 0.0
			for _, r := range rates {
				total += r
			}
			errs[method] += math.Abs(total - actual)
		}
		days++
	}
	for method := range errs {
		errs[method] /= float64(max(1, days))
	}
	return errs, days
}

// TrainOnce retrains every provider from the current history and saves weights.
func (t *Trainer) TrainOnce() {
	now := t.now()
	history := t.source.History(now)
	tokenPoints := t.source.Tokens(now)
	units, _ := t.source.Units()
	t.mu.RLock()
	previous := make(map[string]savedModel, len(t.models))
	for k, v := range t.models {
		previous[k] = v
	}
	t.mu.RUnlock()

	models := map[string]savedModel{}
	for k, v := range previous {
		models[k] = v
	}
	forecasts := map[string]*Forecast{}
	for provider, window := range WeeklyWindow {
		spend := PoolHourlySpend(history.Credentials, units, provider, window, now.UnixMilli())
		if spend == nil {
			continue
		}
		tokens := HourlyTokens(tokenPoints, provider, now.UnixMilli())
		MarkDryHours(tokens, spend)
		s := seriesInput{spend: spend, tokens: tokens}

		started := time.Now()
		scores, days := t.backtest(s)
		method := MethodAverage
		if days >= minBacktestDays {
			for _, candidate := range methods {
				if scores[candidate] < scores[method]-1e-9 {
					method = candidate
				}
			}
		}
		// Every method is refit on the full history (neural ones warm-started
		// from their saved weights), so a method not served today keeps
		// learning and can win a later backtest.
		type fitted struct {
			rates      []float64
			model      *Model
			ratio      float64
			generation int
		}
		fits := map[string]fitted{}
		for _, candidate := range methods {
			key := provider + "/" + candidate
			rates, model, ratio := t.predict(candidate, s, previous[key].Model, HorizonHours)
			f := fitted{rates: rates, model: model, ratio: ratio}
			if model != nil {
				f.generation = previous[key].Generation + 1
				models[key] = savedModel{Model: model, Generation: f.generation}
			}
			fits[candidate] = f
		}
		if len(fits[method].rates) != HorizonHours {
			method = MethodAverage
		}
		chosen := fits[method]
		rates, model, ratio := chosen.rates, chosen.model, chosen.ratio
		forecast := &Forecast{
			StartMs:        spend.StartMs + int64(len(spend.Rates))*spend.StepMs,
			StepMs:         spend.StepMs,
			Rates:          rates,
			Method:         method,
			BacktestMAE:    scores,
			BacktestDays:   days,
			TrainedAtMs:    now.UnixMilli(),
			UnitsPerMToken: ratio,
		}
		if model != nil {
			forecast.Samples, forecast.Skill, forecast.Generation = model.Samples, model.Skill, chosen.generation
			forecast.WarmStart, forecast.Epochs = model.WarmStart, model.Epochs
		}
		forecasts[provider] = forecast
		log.WithFields(log.Fields{"provider": provider, "method": method, "backtest_days": days, "backtest_mae": scores, "generation": forecast.Generation, "duration": time.Since(started).Round(time.Millisecond)}).Info("quota forecast trained")
	}

	t.mu.Lock()
	t.models = models
	t.forecasts = forecasts
	t.mu.Unlock()
	if err := t.save(models); err != nil {
		log.WithError(err).Warn("save quota forecast models")
	}
}

func (t *Trainer) save(models map[string]savedModel) error {
	if t.path == "" {
		return nil
	}
	data, err := json.Marshal(models)
	if err != nil {
		return err
	}
	if errDir := os.MkdirAll(filepath.Dir(t.path), 0700); errDir != nil {
		return errDir
	}
	tmp := t.path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0600); errWrite != nil {
		return errWrite
	}
	return os.Rename(tmp, t.path)
}
