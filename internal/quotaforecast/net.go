// Package quotaforecast trains a small neural network on the pooled weekly
// quota history and predicts each provider's hourly spend.
//
// The model is a multilayer perceptron with 12 inputs, two tanh hidden layers
// (12 and 6 units) and a softplus output, so predictions are never negative.
// Rates are divided by the mean observed rate before training. Inputs for the
// hour being predicted:
//   - hour of day and day of week as sin/cos pairs (server local time)
//   - log1p of the spend 1, 2 and 3 hours earlier, the trailing 6h and 24h
//     means, the same hour yesterday and the same hour last week (yesterday
//     stands in, flagged, until a week of history exists)
//
// Training is full-batch Adam on mean squared error with light L2 and early
// stopping on the latest 20% of hours. Each run trains twice, once from the
// previous run's weights and once from a seeded initialisation, and keeps the
// one with the lower holdout error, so the model improves as history grows
// without getting stuck in an old optimum.
package quotaforecast

import (
	"math"
	"time"
)

const (
	// MinTrainSamples is the fewest usable hours a model trains on.
	MinTrainSamples = 24
	maxShortLag     = 24
	weekHours       = 168
	inputs          = 12
	hidden1         = 12
	hidden2         = 6
	maxEpochs       = 400
	patience        = 60
	learningRate    = 0.01
	l2              = 1e-4
	minHoldout      = 6
	// A prediction never exceeds this multiple of the busiest observed hour.
	maxRateMultiple = 3
	// Day-of-week inputs and the weekly lag need this much history; with less,
	// one unusual weekday would be learned as a weekly pattern.
	weeklyMinHours = 21 * 24
	// ModelVersion changes whenever the feature layout or architecture does;
	// saved weights from another version are ignored.
	ModelVersion = 2
)

// HourlySpend is pool spend per hour bucket in base-plan units per hour.
type HourlySpend struct {
	StartMs int64
	StepMs  int64
	Rates   []float64
	// Usable is false where a bucket is not a fair demand sample (the pool ran
	// dry or readings had a long gap). Such buckets still serve as lags.
	Usable []bool
}

// Weights holds the network parameters.
type Weights struct {
	W1 []float64 `json:"w1"` // hidden1 x inputs
	B1 []float64 `json:"b1"`
	W2 []float64 `json:"w2"` // hidden2 x hidden1
	B2 []float64 `json:"b2"`
	W3 []float64 `json:"w3"` // hidden2
	B3 []float64 `json:"b3"` // 1
}

func (w *Weights) params() [][]float64 { return [][]float64{w.W1, w.B1, w.W2, w.B2, w.W3, w.B3} }

func (w *Weights) valid() bool {
	if w == nil {
		return false
	}
	sizes := []int{hidden1 * inputs, hidden1, hidden2 * hidden1, hidden2, hidden2, 1}
	for i, p := range w.params() {
		if len(p) != sizes[i] {
			return false
		}
		for _, v := range p {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return false
			}
		}
	}
	return true
}

func (w *Weights) clone() *Weights {
	c := func(s []float64) []float64 { return append([]float64(nil), s...) }
	return &Weights{W1: c(w.W1), B1: c(w.B1), W2: c(w.W2), B2: c(w.B2), W3: c(w.W3), B3: c(w.B3)}
}

func zerosLike(w *Weights) *Weights {
	z := func(s []float64) []float64 { return make([]float64, len(s)) }
	return &Weights{W1: z(w.W1), B1: z(w.B1), W2: z(w.W2), B2: z(w.B2), W3: z(w.W3), B3: z(w.B3)}
}

// rng is mulberry32, a small deterministic PRNG.
type rng uint32

func (r *rng) next() float64 {
	*r += 0x6d2b79f5
	t := uint32(*r)
	t = (t ^ (t >> 15)) * (t | 1)
	t ^= t + (t^(t>>7))*(t|61)
	return float64(t^(t>>14)) / 4294967296
}

func initWeights(seed uint32) *Weights {
	r := rng(seed)
	layer := func(rows, cols int) []float64 {
		limit := math.Sqrt(6 / float64(rows+cols))
		out := make([]float64, rows*cols)
		for i := range out {
			out[i] = (r.next()*2 - 1) * limit
		}
		return out
	}
	return &Weights{
		W1: layer(hidden1, inputs), B1: make([]float64, hidden1),
		W2: layer(hidden2, hidden1), B2: make([]float64, hidden2),
		W3: layer(1, hidden2),
		// softplus(0.55) is about 1: start near the mean rate.
		B3: []float64{0.55},
	}
}

func softplus(x float64) float64 {
	if x > 30 {
		return x
	}
	return math.Log1p(math.Exp(x))
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

type activations struct {
	h1 [hidden1]float64
	h2 [hidden2]float64
	z3 float64
}

func forward(w *Weights, x []float64, act *activations) float64 {
	for j := 0; j < hidden1; j++ {
		sum := w.B1[j]
		row := w.W1[j*inputs : (j+1)*inputs]
		for k, v := range row {
			sum += v * x[k]
		}
		act.h1[j] = math.Tanh(sum)
	}
	for j := 0; j < hidden2; j++ {
		sum := w.B2[j]
		row := w.W2[j*hidden1 : (j+1)*hidden1]
		for k, v := range row {
			sum += v * act.h1[k]
		}
		act.h2[j] = math.Tanh(sum)
	}
	z := w.B3[0]
	for k := 0; k < hidden2; k++ {
		z += w.W3[k] * act.h2[k]
	}
	act.z3 = z
	return softplus(z)
}

// writeFeatures fills out with the inputs for hour i of rates (already scaled).
func writeFeatures(rates []float64, i int, bucketStart time.Time, weekly bool, out []float64) {
	hour := (float64(bucketStart.Hour()) + float64(bucketStart.Minute())/60) / 24
	day := (float64(bucketStart.Weekday()) + hour) / 7
	mean := func(from, count int) float64 {
		sum := 0.0
		for k := from; k < from+count; k++ {
			sum += rates[k]
		}
		return sum / float64(count)
	}
	hasWeek := weekly && i >= weekHours
	out[0] = math.Sin(2 * math.Pi * hour)
	out[1] = math.Cos(2 * math.Pi * hour)
	out[2], out[3] = 0, 0
	if weekly {
		out[2] = math.Sin(2 * math.Pi * day)
		out[3] = math.Cos(2 * math.Pi * day)
	}
	out[4] = math.Log1p(rates[i-1])
	out[5] = math.Log1p(rates[i-2])
	out[6] = math.Log1p(rates[i-3])
	out[7] = math.Log1p(mean(i-6, 6))
	out[8] = math.Log1p(mean(i-24, 24))
	out[9] = math.Log1p(rates[i-24])
	if hasWeek {
		out[10] = math.Log1p(rates[i-weekHours])
		out[11] = 1
	} else {
		out[10] = math.Log1p(rates[i-24])
		out[11] = 0
	}
}

// Model is a trained network plus what is needed to use and judge it.
type Model struct {
	Version int      `json:"version"`
	Weights *Weights `json:"weights"`
	// Scale is the mean observed rate; inputs and outputs are multiples of it.
	Scale   float64 `json:"scale"`
	MaxRate float64 `json:"maxRate"`
	Samples int     `json:"samples"`
	// HoldoutLoss is the scaled mean squared error on the latest 20% of hours.
	HoldoutLoss *float64 `json:"holdoutLoss,omitempty"`
	// Skill is the holdout error reduction against a trailing-24h-average
	// predictor (0.25 means 25% lower squared error).
	Skill *float64 `json:"skill,omitempty"`
	// WarmStart is true when this run started from the previous weights.
	WarmStart bool `json:"warmStart"`
	Epochs    int  `json:"epochs"`
	// Weekly is true once day-of-week inputs are in use.
	Weekly bool `json:"weekly"`
}

type dataset struct {
	x          []float64
	y          []float64
	trainCount int
	holdout    int
}

func (d *dataset) row(s int) []float64 { return d.x[s*inputs : (s+1)*inputs] }

// Train fits a model to data. previous, when compatible, seeds a warm-started
// candidate. Returns nil when there are too few usable hours.
func Train(data HourlySpend, previous *Model, loc *time.Location) *Model {
	if loc == nil {
		loc = time.Local
	}
	n := len(data.Rates)
	if n != len(data.Usable) || data.StepMs <= 0 {
		return nil
	}
	sum, count, maxRate := 0.0, 0, 0.0
	for i, rate := range data.Rates {
		if !data.Usable[i] {
			continue
		}
		sum += rate
		count++
		maxRate = math.Max(maxRate, rate)
	}
	if count == 0 {
		return nil
	}
	scale := sum / float64(count)
	if !(scale > 1e-9) {
		return nil
	}
	scaled := make([]float64, n)
	for i, rate := range data.Rates {
		scaled[i] = math.Max(0, rate) / scale
	}
	indices := []int{}
	for i := maxShortLag; i < n; i++ {
		if data.Usable[i] {
			indices = append(indices, i)
		}
	}
	if len(indices) < MinTrainSamples {
		return nil
	}
	weekly := n >= weeklyMinHours
	d := &dataset{x: make([]float64, len(indices)*inputs), y: make([]float64, len(indices))}
	for s, i := range indices {
		writeFeatures(scaled, i, time.UnixMilli(data.StartMs+int64(i)*data.StepMs).In(loc), weekly, d.row(s))
		d.y[s] = scaled[i]
	}
	d.holdout = len(indices) / 5
	if d.holdout < minHoldout {
		d.holdout = 0
	}
	d.trainCount = len(indices) - d.holdout

	candidates := []*Model{fit(d, initWeights(0x5eed+uint32(n)), false)}
	if previous != nil && previous.Version == ModelVersion && previous.Weekly == weekly && previous.Weights.valid() {
		candidates = append(candidates, fit(d, previous.Weights.clone(), true))
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.HoldoutLoss != nil && (best.HoldoutLoss == nil || *c.HoldoutLoss < *best.HoldoutLoss) {
			best = c
		}
	}
	best.Version = ModelVersion
	best.Weekly = weekly
	best.Scale = scale
	best.MaxRate = maxRate
	best.Samples = len(indices)
	if best.HoldoutLoss != nil {
		baseline := 0.0
		for s := d.trainCount; s < len(d.y); s++ {
			diff := math.Expm1(d.row(s)[8]) - d.y[s]
			baseline += diff * diff
		}
		baseline /= float64(d.holdout)
		if baseline > 1e-12 {
			skill := 1 - *best.HoldoutLoss/baseline
			best.Skill = &skill
		}
	}
	return best
}

func lossOf(w *Weights, d *dataset, from, to int) float64 {
	var act activations
	sum := 0.0
	for s := from; s < to; s++ {
		diff := forward(w, d.row(s), &act) - d.y[s]
		sum += diff * diff
	}
	return sum / math.Max(1, float64(to-from))
}

func fit(d *dataset, w *Weights, warm bool) *Model {
	grads, m, v := zerosLike(w), zerosLike(w), zerosLike(w)
	var act activations
	var d1 [hidden1]float64
	var d2 [hidden2]float64
	const beta1, beta2 = 0.9, 0.999
	best := w.clone()
	bestLoss := math.Inf(1)
	sinceBest := 0
	epochs := 0
	useHoldout := d.holdout > 0
	if useHoldout {
		bestLoss = lossOf(w, d, d.trainCount, len(d.y))
	}
	for epoch := 1; epoch <= maxEpochs; epoch++ {
		epochs = epoch
		for _, g := range grads.params() {
			clear(g)
		}
		for s := 0; s < d.trainCount; s++ {
			x := d.row(s)
			out := forward(w, x, &act)
			dz3 := (2 * (out - d.y[s]) / float64(d.trainCount)) * sigmoid(act.z3)
			grads.B3[0] += dz3
			for k := 0; k < hidden2; k++ {
				grads.W3[k] += dz3 * act.h2[k]
				d2[k] = dz3 * w.W3[k] * (1 - act.h2[k]*act.h2[k])
			}
			d1 = [hidden1]float64{}
			for j := 0; j < hidden2; j++ {
				grads.B2[j] += d2[j]
				for k := 0; k < hidden1; k++ {
					grads.W2[j*hidden1+k] += d2[j] * act.h1[k]
					d1[k] += d2[j] * w.W2[j*hidden1+k]
				}
			}
			for j := 0; j < hidden1; j++ {
				dz := d1[j] * (1 - act.h1[j]*act.h1[j])
				grads.B1[j] += dz
				for k := 0; k < inputs; k++ {
					grads.W1[j*inputs+k] += dz * x[k]
				}
			}
		}
		c1 := 1 - math.Pow(beta1, float64(epoch))
		c2 := 1 - math.Pow(beta2, float64(epoch))
		params, gs, ms, vs := w.params(), grads.params(), m.params(), v.params()
		for p := range params {
			decay := 0.0
			if p%2 == 0 { // W1, W2, W3
				decay = l2
			}
			for i := range params[p] {
				g := gs[p][i] + decay*params[p][i]
				ms[p][i] = beta1*ms[p][i] + (1-beta1)*g
				vs[p][i] = beta2*vs[p][i] + (1-beta2)*g*g
				params[p][i] -= learningRate * (ms[p][i] / c1) / (math.Sqrt(vs[p][i]/c2) + 1e-8)
			}
		}
		if !useHoldout {
			continue
		}
		loss := lossOf(w, d, d.trainCount, len(d.y))
		if loss < bestLoss-1e-9 {
			bestLoss, best, sinceBest = loss, w.clone(), 0
		} else if sinceBest++; sinceBest >= patience {
			break
		}
	}
	model := &Model{WarmStart: warm, Epochs: epochs}
	if useHoldout {
		model.Weights = best
		model.HoldoutLoss = &bestLoss
	} else {
		model.Weights = w
	}
	return model
}

// Forecast predicts steps hourly rates after data ends, feeding each prediction
// into the lags of the next.
func (m *Model) Forecast(data HourlySpend, steps int, loc *time.Location) []float64 {
	if m == nil || !m.Weights.valid() || !(m.Scale > 0) || data.StepMs <= 0 {
		return nil
	}
	if loc == nil {
		loc = time.Local
	}
	series := make([]float64, len(data.Rates), len(data.Rates)+steps)
	for i, rate := range data.Rates {
		series[i] = math.Max(0, rate) / m.Scale
	}
	if len(series) < maxShortLag {
		return nil
	}
	// Cap by the busiest recent hour, not the busiest ever: a quiet week after
	// a heavy one must not be forecast back up to the heavy week's peaks.
	recentMax := 0.0
	for _, v := range series[max(0, len(series)-weekHours):] {
		recentMax = math.Max(recentMax, v)
	}
	limit := recentMax * maxRateMultiple
	features := make([]float64, inputs)
	var act activations
	out := make([]float64, 0, steps)
	for step := 0; step < steps; step++ {
		i := len(series)
		writeFeatures(series, i, time.UnixMilli(data.StartMs+int64(i)*data.StepMs).In(loc), m.Weekly, features)
		predicted := math.Min(limit, forward(m.Weights, features, &act))
		series = append(series, predicted)
		out = append(out, predicted*m.Scale)
	}
	return out
}
