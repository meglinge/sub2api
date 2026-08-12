package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// UpstreamRouter-aligned outcome verdicts (written into ai_actions.outcome).
const (
	verdictPrefix      = "判定="
	verdictEffective   = "有效"
	verdictNoChange    = "无效(无变化)"
	verdictStarved     = "无效(改后零流量)"
	verdictWorse       = "恶化"
	verdictThinSamples = "样本不足"
)

type actionBaseline struct {
	WindowMs  int64   `json:"windowMs"`
	Requests  int     `json:"requests"`
	Success   int     `json:"success"`
	AvgTtfbMs float64 `json:"avgTtfbMs"`
}

func captureActionBaseline(ctx context.Context, repo AIPilotStore, accountID int64, now time.Time, cfg AIAutopilotSettings) string {
	if repo == nil {
		return "{}"
	}
	wMin := cfg.OutcomeMaxWaitMinutes
	if wMin <= 0 {
		wMin = 5
	}
	from := now.Add(-time.Duration(wMin) * time.Minute)
	st, err := repo.AggregateAccountTrafficOne(ctx, accountID, from, now)
	if err != nil {
		return "{}"
	}
	b, err := json.Marshal(actionBaseline{
		WindowMs:  int64(wMin) * 60 * 1000,
		Requests:  st.Requests,
		Success:   st.Successes,
		AvgTtfbMs: st.AvgFirstToken,
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func isDemotionAction(a AIAction) bool {
	switch a.Op {
	case AIOpDisable:
		return true
	case AIOpSetPriority:
		b, v, ok := parseIntPair(a.Before, a.After)
		return ok && v > b
	case AIOpSetWeight:
		b, v, ok := parseIntPair(a.Before, a.After)
		return ok && v < b
	}
	return false
}

func parseIntPair(before, after string) (int, int, bool) {
	b, err1 := strconv.Atoi(strings.TrimSpace(before))
	v, err2 := strconv.Atoi(strings.TrimSpace(after))
	return b, v, err1 == nil && err2 == nil
}

// backfillOutcomes settles applied actions that still lack outcome.
// Called at the start of Analyze so memory already carries verdicts.
func (p *AIPilotService) backfillOutcomes(ctx context.Context, cfg AIAutopilotSettings) {
	if p == nil || p.Repo == nil {
		return
	}
	minSamples := cfg.OutcomeMinSamples
	if minSamples <= 0 {
		minSamples = 20
	}
	maxWait := time.Duration(cfg.OutcomeMaxWaitMinutes) * time.Minute
	if maxWait <= 0 {
		maxWait = 5 * time.Minute
	}
	// Only look back a few days so we don't reprocess ancient empty outcomes forever.
	since := time.Now().Add(-72 * time.Hour)
	acts, err := p.Repo.ListActionsPendingOutcome(ctx, since, 20)
	if err != nil || len(acts) == 0 {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	now := time.Now()
	settled := 0
	for _, a := range acts {
		if time.Now().After(deadline) {
			break
		}
		end := now
		interrupted := false
		if next, nerr := p.Repo.NextActionTS(ctx, a.AccountID, a.TS); nerr == nil && next != nil && next.Before(end) {
			end = *next
			interrupted = true
		}
		if !end.After(a.TS) {
			continue
		}
		after, aerr := p.Repo.AggregateAccountTrafficOne(ctx, a.AccountID, a.TS, end)
		if aerr != nil {
			continue
		}
		if after.Requests < minSamples && now.Sub(a.TS) < maxWait && !interrupted {
			continue
		}
		outcome := composeActionOutcome(a, after, end.Sub(a.TS), interrupted, minSamples)
		if err := p.Repo.SetActionOutcome(ctx, a.ID, outcome, now); err != nil {
			if p.Log != nil {
				p.Log.Warn("set action outcome failed", "id", a.ID, "err", err)
			}
			continue
		}
		settled++
	}
	if settled > 0 && p.Log != nil {
		p.Log.Info("ai pilot outcome backfill", "settled", settled)
	}
}

func composeActionOutcome(a AIAction, after AccountTrafficStats, span time.Duration, interrupted bool, minSamples int) string {
	var base actionBaseline
	if a.BaselineJSON != "" {
		_ = json.Unmarshal([]byte(a.BaselineJSON), &base)
	}
	verdict, note := judgeActionOutcome(a, base, after, minSamples)
	var b strings.Builder
	b.WriteString(verdictPrefix)
	b.WriteString(verdict)
	b.WriteString(" | 改后")
	b.WriteString(fmtDurationShort(span))
	b.WriteString(fmt.Sprintf(": req %.1f→%.1f/min",
		perMin(base.Requests, base.WindowMs), perMin(after.Requests, span.Milliseconds())))
	if base.Requests > 0 && after.Requests > 0 {
		b.WriteString(fmt.Sprintf(", 成功率 %.0f%%→%.0f%%",
			ratePct(base.Success, base.Requests), ratePct(after.Successes, after.Requests)))
		if base.AvgTtfbMs > 0 && after.AvgFirstToken > 0 {
			b.WriteString(fmt.Sprintf(", TTFB %.1fs→%.1fs", base.AvgTtfbMs/1000, after.AvgFirstToken/1000))
		}
	} else if after.Requests > 0 {
		b.WriteString(fmt.Sprintf(", 成功率 %.0f%%", ratePct(after.Successes, after.Requests)))
	}
	if note != "" {
		b.WriteString("; ")
		b.WriteString(note)
	}
	if interrupted {
		b.WriteString(" · 观察期被同账号新动作打断")
	}
	return b.String()
}

func judgeActionOutcome(a AIAction, base actionBaseline, after AccountTrafficStats, minSamples int) (string, string) {
	if after.Requests == 0 {
		if isDemotionAction(a) {
			return verdictStarved, "降级让它不吃流量了,但从此不再产生真实数据 — 别再据此继续压"
		}
		return verdictStarved, "改完仍零流量,这一步没换来可用信息"
	}
	weak := after.Requests < minSamples || base.Requests < minSamples
	srGate := 0.10
	ttfbGate := 1.3
	if weak {
		srGate = 0.30
		ttfbGate = 2.0
	}
	if base.Requests <= 0 {
		if weak {
			return verdictThinSamples, "基线样本不足,仅见改后有流量"
		}
		return verdictEffective, "改后有流量"
	}
	beforeSR := rate(base.Success, base.Requests)
	afterSR := rate(after.Successes, after.Requests)
	// Demotion that improves SR / TTFB → effective; worsen → worse; else no change / thin.
	if isDemotionAction(a) {
		if afterSR > beforeSR+srGate {
			return verdictEffective, "降级后成功率明显改善"
		}
		if afterSR < beforeSR-srGate {
			return verdictWorse, "降级后成功率反而变差"
		}
		if base.AvgTtfbMs > 0 && after.AvgFirstToken > 0 {
			if after.AvgFirstToken < base.AvgTtfbMs/ttfbGate {
				return verdictEffective, "降级后 TTFB 明显改善"
			}
			if after.AvgFirstToken > base.AvgTtfbMs*ttfbGate {
				return verdictWorse, "降级后 TTFB 明显变差"
			}
		}
		if weak {
			return verdictThinSamples, "样本偏少,看不出降级是否值得"
		}
		return verdictNoChange, "降级后表现与改前接近"
	}
	// Non-demotion (enable / lift / raise weight)
	if afterSR < beforeSR-srGate {
		return verdictWorse, "抬升/恢复后成功率变差"
	}
	if afterSR > beforeSR+srGate {
		return verdictEffective, "抬升/恢复后成功率改善"
	}
	if weak {
		return verdictThinSamples, "样本偏少"
	}
	return verdictNoChange, "表现与改前接近"
}

func rate(success, requests int) float64 {
	if requests <= 0 {
		return 0
	}
	return float64(success) / float64(requests)
}

func ratePct(success, requests int) float64 {
	return rate(success, requests) * 100
}

func perMin(requests int, windowMs int64) float64 {
	if windowMs <= 0 {
		return 0
	}
	return float64(requests) / (float64(windowMs) / 60000.0)
}

func fmtDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
	return fmt.Sprintf("%.1fm", d.Minutes())
}

func verdictOfOutcome(outcome string) string {
	if !strings.HasPrefix(outcome, verdictPrefix) {
		return ""
	}
	rest := outcome[len(verdictPrefix):]
	if i := strings.IndexAny(rest, " |"); i >= 0 {
		return rest[:i]
	}
	return rest
}
