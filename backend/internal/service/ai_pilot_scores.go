package service

import (
	"math"
	"strings"
)

// Default overallWeights: stability dominates — speed/cheapness do not matter if it fails.
// Overridable via AIAutopilotSettings score_weight_* (percentages).
const (
	defaultScoreWStability  = 0.4
	defaultScoreWLatency    = 0.3
	defaultScoreWThroughput = 0.2
	defaultScoreWCost       = 0.1
	// overallDriftTolerance: if model overall drifts this far from weighted dims, recompute.
	overallDriftTolerance = 25
)

// ScoreWeights are fractions that sum to 1 for overall = Σ dim * weight.
type ScoreWeights struct {
	Stability  float64
	Latency    float64
	Throughput float64
	Cost       float64
}

// DefaultScoreWeights returns UpstreamRouter original 40/30/20/10.
func DefaultScoreWeights() ScoreWeights {
	return ScoreWeights{
		Stability:  defaultScoreWStability,
		Latency:    defaultScoreWLatency,
		Throughput: defaultScoreWThroughput,
		Cost:       defaultScoreWCost,
	}
}

// Valid reports whether weights are usable (positive sum, each non-negative).
func (w ScoreWeights) Valid() bool {
	if w.Stability < 0 || w.Latency < 0 || w.Throughput < 0 || w.Cost < 0 {
		return false
	}
	sum := w.Stability + w.Latency + w.Throughput + w.Cost
	return sum > 0.01 && !math.IsNaN(sum) && !math.IsInf(sum, 0)
}

// Normalized returns fractions summing to 1 (or defaults if invalid).
func (w ScoreWeights) Normalized() ScoreWeights {
	if !w.Valid() {
		return DefaultScoreWeights()
	}
	sum := w.Stability + w.Latency + w.Throughput + w.Cost
	return ScoreWeights{
		Stability:  w.Stability / sum,
		Latency:    w.Latency / sum,
		Throughput: w.Throughput / sum,
		Cost:       w.Cost / sum,
	}
}

// Percents returns integer percents that sum to 100 (best-effort rounding).
func (w ScoreWeights) Percents() (stability, latency, throughput, cost int) {
	n := w.Normalized()
	stability = int(math.Round(n.Stability * 100))
	latency = int(math.Round(n.Latency * 100))
	throughput = int(math.Round(n.Throughput * 100))
	cost = 100 - stability - latency - throughput
	if cost < 0 {
		cost = 0
	}
	return
}

// ScoreWeightsFromPercents builds weights from 0–100 integer percents.
// Zero-all → defaults. Non-zero sum is renormalized even if ≠100.
func ScoreWeightsFromPercents(stab, lat, thr, cost int) ScoreWeights {
	if stab < 0 {
		stab = 0
	}
	if lat < 0 {
		lat = 0
	}
	if thr < 0 {
		thr = 0
	}
	if cost < 0 {
		cost = 0
	}
	if stab == 0 && lat == 0 && thr == 0 && cost == 0 {
		return DefaultScoreWeights()
	}
	return ScoreWeights{
		Stability:  float64(stab),
		Latency:    float64(lat),
		Throughput: float64(thr),
		Cost:       float64(cost),
	}.Normalized()
}

// ScoreWeights returns configured dimension weights (normalized fractions).
func (s AIAutopilotSettings) ScoreWeights() ScoreWeights {
	return ScoreWeightsFromPercents(
		s.ScoreWeightStability,
		s.ScoreWeightLatency,
		s.ScoreWeightThroughput,
		s.ScoreWeightCost,
	)
}

func clampScore(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Max(0, math.Min(100, v))
}

// NormalizeAccountScore clamps dims and recomputes overall with default weights.
func NormalizeAccountScore(s AIAccountScore) AIAccountScore {
	return NormalizeAccountScoreWith(s, DefaultScoreWeights())
}

// NormalizeAccountScoreWith clamps dims and recomputes overall when inconsistent
// using the given score weights (settings-driven).
func NormalizeAccountScoreWith(s AIAccountScore, w ScoreWeights) AIAccountScore {
	w = w.Normalized()
	s.Stability = clampScore(s.Stability)
	s.Latency = clampScore(s.Latency)
	s.Throughput = clampScore(s.Throughput)
	s.Cost = clampScore(s.Cost)
	s.Confidence = clampScore(s.Confidence)
	// Model confidence is 0–1 in actions; for scores UpstreamRouter uses 0–100 on confidence field sometimes.
	// Keep 0–100 after clamp; if model sent 0–1, scale up.
	if s.Confidence > 0 && s.Confidence <= 1 {
		s.Confidence = clampScore(s.Confidence * 100)
	}

	want := w.Stability*s.Stability + w.Latency*s.Latency +
		w.Throughput*s.Throughput + w.Cost*s.Cost
	s.Overall = clampScore(s.Overall)
	// Always recompute overall from dims when cost was backend-derived, or when drifted.
	if s.Overall <= 0 || math.Abs(s.Overall-want) > overallDriftTolerance {
		s.Overall = clampScore(want)
		note := strings.TrimSpace(s.Note)
		if !strings.Contains(note, "overall 由后端") {
			if note != "" {
				note += " "
			}
			note += "[overall 由后端按分项重算]"
		}
		s.Note = note
	}
	if len(s.Note) > 300 {
		s.Note = s.Note[:300]
	}
	return s
}

// cost score defaults when rate is unknown / only one peer.
const (
	// Unknown-rate accounts must NOT inherit peer cheap composites (LLM used to invent 0.06).
	// Slightly pessimistic so high 性价比 weight does not promote mystery rates.
	costScoreUnknownRate = 40
	// Single known rate / no spread → still decent but not max.
	costScoreUniformPool = 85
	// Floor for the most expensive known rate in the pool.
	costScoreMinKnown = 5
)

// accountCostSignal is the pilot-side view of an account's billing rate.
type accountCostSignal struct {
	Composite float64
	Level     int  // 1 high … 3 low
	Known     bool // true when rate is not default_one / pure guess
}

// accountCostSignalOf resolves rate/recharge → composite + confidence for ranking.
func accountCostSignalOf(acc *Account) accountCostSignal {
	if acc == nil {
		return accountCostSignal{Composite: 1, Level: 3, Known: false}
	}
	rate, src := resolveAccountRate(acc)
	recharge := accountRechargeMultiplier(acc)
	rc := BuildRateConfidence(rate, recharge, src)
	known := rc.Level <= 2 || (rc.Primary != "default_one" && rc.Primary != "estimated")
	// Level-3 default_one (rate=1 no import) is "unknown", not truly 1× expensive.
	if rc.Level >= 3 && (rc.Primary == "default_one" || rc.Primary == "estimated") {
		return accountCostSignal{Composite: rc.TrustedComposite, Level: rc.Level, Known: false}
	}
	return accountCostSignal{Composite: rc.TrustedComposite, Level: rc.Level, Known: known || rc.Level <= 2}
}

// deriveCostScoreMap ranks accounts by trusted composite (cheaper → higher cost score).
// Unknown rates get a fixed mid-low score and are excluded from the min/max scale
// so they never look as cheap as a peer with composite=0.06.
func deriveCostScoreMap(accounts []Account) map[int64]float64 {
	out := make(map[int64]float64, len(accounts))
	if len(accounts) == 0 {
		return out
	}
	type sample struct {
		id   int64
		comp float64
		ok   bool
	}
	samples := make([]sample, 0, len(accounts))
	var known []float64
	for i := range accounts {
		sig := accountCostSignalOf(&accounts[i])
		s := sample{id: accounts[i].ID, comp: sig.Composite, ok: sig.Known && sig.Composite > 0}
		samples = append(samples, s)
		if s.ok {
			known = append(known, s.comp)
		}
	}
	minC, maxC := 0.0, 0.0
	if len(known) > 0 {
		minC, maxC = known[0], known[0]
		for _, c := range known[1:] {
			if c < minC {
				minC = c
			}
			if c > maxC {
				maxC = c
			}
		}
	}
	for _, s := range samples {
		if !s.ok {
			out[s.id] = costScoreUnknownRate
			continue
		}
		if len(known) < 2 || maxC <= minC*1.001 {
			out[s.id] = costScoreUniformPool
			continue
		}
		// Log scale so 0.06 vs 0.11 still separates, and 100× outliers don't crush the rest.
		span := math.Log(maxC) - math.Log(minC)
		if span <= 0 || s.comp <= 0 {
			out[s.id] = costScoreUniformPool
			continue
		}
		t := (math.Log(s.comp) - math.Log(minC)) / span // 0=cheapest … 1=most expensive
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		out[s.id] = clampScore(100 - t*(100-costScoreMinKnown))
	}
	return out
}

// applyDeterministicCostScores overwrites LLM cost dims with backend ranking from money,
// then forces overall recompute so 性价比 weight actually moves set_weight targets.
func applyDeterministicCostScores(scores []AIAccountScore, accounts []Account, w ScoreWeights) {
	if len(scores) == 0 || len(accounts) == 0 {
		return
	}
	costByID := deriveCostScoreMap(accounts)
	w = w.Normalized()
	for i := range scores {
		derived, ok := costByID[scores[i].AccountID]
		if !ok {
			continue
		}
		prev := scores[i].Cost
		scores[i].Cost = derived
		// Always recompute overall after cost overwrite (even if within drift).
		want := w.Stability*clampScore(scores[i].Stability) +
			w.Latency*clampScore(scores[i].Latency) +
			w.Throughput*clampScore(scores[i].Throughput) +
			w.Cost*derived
		scores[i].Overall = clampScore(want)
		note := strings.TrimSpace(scores[i].Note)
		tag := "[cost 由后端 composite 重算]"
		if !strings.Contains(note, "cost 由后端") {
			if note != "" {
				note += " "
			}
			note += tag
		}
		if math.Abs(prev-derived) > 15 {
			note += " (模型 cost 偏差已覆盖)"
		}
		if len(note) > 300 {
			note = note[:300]
		}
		scores[i].Note = note
	}
}
