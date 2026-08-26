package service

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Default overallWeights: stability dominates — speed/cheapness do not matter if it fails.
// Overridable via AIAutopilotSettings score_weight_* (percentages).
const (
	defaultScoreWStability  = 0.4
	defaultScoreWLatency    = 0.3
	defaultScoreWThroughput = 0.2
	defaultScoreWCost       = 0.1
	// overallDriftTolerance: note when model overall disagrees with weighted dims.
	overallDriftTolerance = 1
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
	prev := s.Overall
	// Settings weights must always own overall — a 25pt drift slack meant 40/30/20/10
	// never showed up in the number the panel and set_weight look at.
	s.Overall = clampScore(math.Round(want*10) / 10)
	if prev <= 0 || math.Abs(prev-want) > overallDriftTolerance {
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
		scores[i].Overall = clampScore(math.Round(want*10) / 10)
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

const (
	scoreDimUnknown             = 40.0
	scoreDimIdleThroughput      = 15.0
	scoreWeightSyncReasonPrefix = "综合分对齐:"
	scoreWeightSyncEpsilon      = 2
)

type trafficScoreDims struct {
	stab, lat, thr float64
}

func isScoreWeightSyncReason(reason string) bool {
	return strings.Contains(reason, "综合分对齐")
}

func scoreToScheduleWeight(overall float64) int {
	n := int(math.Round(clampScore(overall)))
	if n < AIWeightHealthyFloor {
		n = AIWeightHealthyFloor
	}
	return n
}

func trafficForScoring(recent, long AccountTrafficStats) AccountTrafficStats {
	rn := recent.Requests + recent.Errors
	ln := long.Requests + long.Errors
	if rn >= 5 {
		return recent
	}
	if ln >= 5 {
		return long
	}
	if rn > 0 {
		return recent
	}
	return long
}

func stabilityScoreOf(st AccountTrafficStats) float64 {
	n := st.Requests + st.Errors
	if n <= 0 {
		return scoreDimUnknown
	}
	succ := st.Successes
	if succ <= 0 && st.Requests > 0 {
		succ = st.Requests
	}
	return clampScore(100 * float64(succ) / float64(n))
}

// logRankScores maps positive values onto [5,100]. Unknown/non-positive stay `unknown`.
func logRankScores(byID map[int64]float64, lowerIsBetter bool, unknown float64) map[int64]float64 {
	out := make(map[int64]float64, len(byID))
	var known []float64
	for _, v := range byID {
		if v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
			known = append(known, v)
		}
	}
	minV, maxV := 0.0, 0.0
	if len(known) > 0 {
		minV, maxV = known[0], known[0]
		for _, v := range known[1:] {
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
	}
	for id, v := range byID {
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			out[id] = unknown
			continue
		}
		if len(known) < 2 || maxV <= minV*1.001 {
			out[id] = costScoreUniformPool
			continue
		}
		span := math.Log(maxV) - math.Log(minV)
		if span <= 0 {
			out[id] = costScoreUniformPool
			continue
		}
		t := (math.Log(v) - math.Log(minV)) / span
		if t < 0 {
			t = 0
		}
		if t > 1 {
			t = 1
		}
		if lowerIsBetter {
			out[id] = clampScore(100 - t*(100-costScoreMinKnown))
		} else {
			out[id] = clampScore(costScoreMinKnown + t*(100-costScoreMinKnown))
		}
	}
	return out
}

func deriveTrafficDimensionScores(accounts []Account, long, recent map[int64]AccountTrafficStats) map[int64]trafficScoreDims {
	out := make(map[int64]trafficScoreDims, len(accounts))
	ttfb := make(map[int64]float64, len(accounts))
	vol := make(map[int64]float64, len(accounts))
	dur := make(map[int64]float64, len(accounts))
	for i := range accounts {
		id := accounts[i].ID
		var lg, rec AccountTrafficStats
		if long != nil {
			lg = long[id]
		}
		if recent != nil {
			rec = recent[id]
		}
		st := trafficForScoring(rec, lg)
		out[id] = trafficScoreDims{stab: stabilityScoreOf(st)}
		if st.AvgFirstToken > 0 {
			ttfb[id] = st.AvgFirstToken
		}
		volN := lg.Requests
		if volN <= 0 {
			volN = rec.Requests
		}
		if volN > 0 {
			vol[id] = float64(volN)
		}
		if st.AvgDuration > 0 {
			dur[id] = st.AvgDuration
		} else if st.AvgFirstToken > 0 {
			dur[id] = st.AvgFirstToken
		}
	}
	lat := logRankScores(ttfb, true, scoreDimUnknown)
	volS := logRankScores(vol, false, scoreDimIdleThroughput)
	durS := logRankScores(dur, true, scoreDimUnknown)
	for id, d := range out {
		d.lat = lat[id]
		if d.lat <= 0 {
			d.lat = scoreDimUnknown
		}
		vs, vok := volS[id]
		ds, dok := durS[id]
		switch {
		case vok && vs > scoreDimIdleThroughput && dok && ds > 0:
			d.thr = clampScore(0.5*vs + 0.5*ds)
		case vok && vs > 0:
			d.thr = vs
		default:
			d.thr = scoreDimIdleThroughput
		}
		out[id] = d
	}
	return out
}

// applyDeterministicScores overwrites all four dims from live traffic + composite
// so admin 稳/延迟/流畅/性价比 sliders actually rank the pool. LLM scores are display-only.
func applyDeterministicScores(scores []AIAccountScore, accounts []Account, long, recent map[int64]AccountTrafficStats, w ScoreWeights) []AIAccountScore {
	if len(accounts) == 0 {
		return scores
	}
	dims := deriveTrafficDimensionScores(accounts, long, recent)
	byID := make(map[int64]int, len(scores))
	for i := range scores {
		byID[scores[i].AccountID] = i
	}
	for i := range accounts {
		acc := &accounts[i]
		d := dims[acc.ID]
		idx, ok := byID[acc.ID]
		if !ok {
			scores = append(scores, AIAccountScore{AccountID: acc.ID, AccountName: acc.Name})
			idx = len(scores) - 1
			byID[acc.ID] = idx
		}
		scores[idx].Stability = d.stab
		scores[idx].Latency = d.lat
		scores[idx].Throughput = d.thr
		if scores[idx].AccountName == "" {
			scores[idx].AccountName = acc.Name
		}
		if scores[idx].Confidence <= 0 {
			scores[idx].Confidence = 90
		}
		note := fmt.Sprintf("后端四维 稳%.0f 延迟%.0f 流畅%.0f", d.stab, d.lat, d.thr)
		if prev := strings.TrimSpace(scores[idx].Note); prev != "" && !strings.Contains(prev, "后端四维") {
			note = prev + " " + note
		}
		scores[idx].Note = note
	}
	applyDeterministicCostScores(scores, accounts, w)
	applyDeterministicCacheScores(scores, accounts, long, recent)
	return scores
}

// injectScoreDrivenWeights writes schedule_weight = round(overall) so configured
// 40/30/20/10 (or whatever the admin set) actually moves Top-K lottery share.
// Bypasses ±20 abs caps at apply time via reason prefix. Skips cost-isolated w0.
func injectScoreDrivenWeights(decision *decision, accounts []Account, cfg AIAutopilotSettings, recent map[int64]AccountTrafficStats) int {
	if decision == nil || !cfg.OpAllowed(AIOpSetWeight) {
		return 0
	}
	scoresByID := make(map[int64]AIAccountScore, len(decision.Scores))
	for _, s := range decision.Scores {
		scoresByID[s.AccountID] = s
	}
	if len(scoresByID) == 0 {
		return 0
	}
	pendingEnable := map[int64]bool{}
	pendingDisable := map[int64]bool{}
	pendingW0 := map[int64]bool{}
	for _, a := range decision.Actions {
		switch a.Op {
		case AIOpEnable:
			pendingEnable[a.AccountID] = true
		case AIOpDisable:
			pendingDisable[a.AccountID] = true
		case AIOpSetWeight:
			if n, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && n <= costSoftQuarantineWeight {
				pendingW0[a.AccountID] = true
			}
		}
	}
	filtered := decision.Actions[:0]
	for _, a := range decision.Actions {
		if a.Op == AIOpSetWeight && !pendingW0[a.AccountID] && !pendingDisable[a.AccountID] {
			if _, ok := scoresByID[a.AccountID]; ok {
				continue // drop LLM ±20 / leftover release w=10; we rewrite from overall
			}
		}
		filtered = append(filtered, a)
	}
	decision.Actions = filtered

	sPct, lPct, tPct, cPct := cfg.ScoreWeights().Percents()
	injected := 0
	for i := range accounts {
		acc := &accounts[i]
		if acc.Status != StatusActive || !acc.Schedulable {
			continue
		}
		if acc.IsExcludedFromSchedule() || !acc.AIManaged {
			continue
		}
		if pendingDisable[acc.ID] {
			continue
		}
		if acc.AIDisabled && !pendingEnable[acc.ID] {
			continue
		}
		if pendingW0[acc.ID] || costJustifiedIsolation(acc, accounts, cfg, recent) {
			continue
		}
		sc, ok := scoresByID[acc.ID]
		if !ok || sc.Overall <= 0 {
			continue
		}
		target := scoreToScheduleWeight(sc.Overall)
		cur := acc.EffectiveScheduleWeight()
		if absInt(target-cur) < scoreWeightSyncEpsilon {
			continue
		}
		decision.Actions = append(decision.Actions, decisionAction{
			AccountID: acc.ID,
			Op:        AIOpSetWeight,
			Value:     strconv.Itoa(target),
			Reason: fmt.Sprintf(
				"%s overall=%.1f (稳%.0f 延迟%.0f 流畅%.0f 性价比%.0f) 按设置 %d/%d/%d/%d → weight %d",
				scoreWeightSyncReasonPrefix, sc.Overall, sc.Stability, sc.Latency, sc.Throughput, sc.Cost,
				sPct, lPct, tPct, cPct, target,
			),
			Confidence: 0.95,
		})
		injected++
	}
	return injected
}
