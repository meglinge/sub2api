package service

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Cache-aware autopilot: do not name vendors. Long-context Codex dies when the
// main layer is a wide 中转 pool (cache floor ~3712/7053). Rank by observed
// prompt-cache quality and soft-band priority so LRU (current Responses path)
// concentrates new sessions, while poor-cache accounts stay as failover spare.
const (
	cacheEligibleMinTokens   = 8192
	cacheBigMissMinInput     = 20000
	cacheBigMissMaxRead      = 8192
	cacheMinEligibleRequests = 10
	cacheMainQuality         = 0.80
	cacheOverflowQuality     = 0.55
	cacheMinMainAccounts     = 2
	cacheOverallFloor        = 0.40
	AICacheOverflowPriority  = 110
	cachePriorityBandPrefix  = "缓存分层:"
)

func isCachePriorityBandReason(reason string) bool {
	return strings.Contains(reason, "缓存分层")
}

func cacheStatsForScoring(recent, long AccountTrafficStats) AccountTrafficStats {
	if long.CacheEligibleRequests >= cacheMinEligibleRequests {
		return long
	}
	if recent.CacheEligibleRequests >= cacheMinEligibleRequests {
		return recent
	}
	if long.CacheEligibleRequests > 0 {
		return long
	}
	return recent
}

// cacheQualityOf returns token-weighted cache hit * (1 - big-miss rate) in [0,1].
// ok=false when the window has too few eligible prompts to judge.
func cacheQualityOf(st AccountTrafficStats) (quality float64, ok bool) {
	if st.CacheEligibleRequests < cacheMinEligibleRequests || st.CacheEligibleTokens <= 0 {
		return 0, false
	}
	hit := float64(st.CacheReadTokens) / float64(st.CacheEligibleTokens)
	if hit < 0 {
		hit = 0
	}
	if hit > 1 {
		hit = 1
	}
	missRate := float64(st.CacheBigMissRequests) / float64(st.CacheEligibleRequests)
	if missRate < 0 {
		missRate = 0
	}
	if missRate > 1 {
		missRate = 1
	}
	q := hit * (1 - missRate)
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	return q, true
}

func cachePriorityForQuality(quality float64) int {
	if quality >= cacheMainQuality {
		return AIObservationPriority
	}
	if quality >= cacheOverflowQuality {
		return AICacheOverflowPriority
	}
	return AIPriorityBuriedThreshold
}

func applyDeterministicCacheScores(scores []AIAccountScore, accounts []Account, long, recent map[int64]AccountTrafficStats) {
	if len(scores) == 0 || len(accounts) == 0 {
		return
	}
	byID := make(map[int64]int, len(scores))
	for i := range scores {
		byID[scores[i].AccountID] = i
	}
	for i := range accounts {
		id := accounts[i].ID
		idx, ok := byID[id]
		if !ok {
			continue
		}
		var rec, lg AccountTrafficStats
		if recent != nil {
			rec = recent[id]
		}
		if long != nil {
			lg = long[id]
		}
		q, enough := cacheQualityOf(cacheStatsForScoring(rec, lg))
		if !enough {
			continue
		}
		factor := cacheOverallFloor + (1-cacheOverallFloor)*q
		prev := scores[idx].Overall
		scores[idx].Overall = clampScore(math.Round(prev*factor*10) / 10)
		note := strings.TrimSpace(scores[idx].Note)
		tag := fmt.Sprintf("[cache quality=%.0f%% ×%.2f]", q*100, factor)
		if !strings.Contains(note, "[cache quality=") {
			if note != "" {
				note += " "
			}
			note += tag
		}
		if len(note) > 300 {
			note = note[:300]
		}
		scores[idx].Note = note
	}
}

type cacheBandCand struct {
	acc      *Account
	stats    AccountTrafficStats
	recent   AccountTrafficStats
	quality  float64
	target   int
	eligible bool
	hardFail bool
}

// injectCachePriorityBands writes set_priority from observed cache quality.
// Best cacheMinMainAccounts stay at p=100 even if below the main threshold, so
// failover still has a main layer when the whole pool is mediocre.
func injectCachePriorityBands(decision *decision, accounts []Account, cfg AIAutopilotSettings, long, recent map[int64]AccountTrafficStats) int {
	if decision == nil || !cfg.OpAllowed(AIOpSetPriority) {
		return 0
	}
	cands := make([]cacheBandCand, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if acc.Status != StatusActive || !acc.Schedulable || acc.AIDisabled {
			continue
		}
		if acc.IsExcludedFromSchedule() || !acc.AIManaged {
			continue
		}
		var rec, lg AccountTrafficStats
		if recent != nil {
			rec = recent[acc.ID]
		}
		if long != nil {
			lg = long[acc.ID]
		}
		st := cacheStatsForScoring(rec, lg)
		q, enough := cacheQualityOf(st)
		c := cacheBandCand{acc: acc, stats: st, recent: rec, quality: q, eligible: enough, target: acc.Priority}
		if recentWindowHardFail(rec) || longWindowDisableWorthy(lg) {
			// Cache quality is a long-window signal. A live 502 failover storm
			// (or a 60-minute SR<50% corpse) must not keep/promote the account
			// in the main layer just because prefix cache used to look good.
			c.hardFail = true
			c.eligible = true
			c.target = AIPriorityBuriedThreshold
		} else if enough {
			c.target = cachePriorityForQuality(q)
		}
		cands = append(cands, c)
	}
	ranked := make([]cacheBandCand, 0, len(cands))
	for _, c := range cands {
		if c.eligible && !c.hardFail {
			ranked = append(ranked, c)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].quality == ranked[j].quality {
			return ranked[i].acc.ID < ranked[j].acc.ID
		}
		return ranked[i].quality > ranked[j].quality
	})
	keep := cacheMinMainAccounts
	if keep > len(ranked) {
		keep = len(ranked)
	}
	keepIDs := map[int64]bool{}
	for i := 0; i < keep; i++ {
		keepIDs[ranked[i].acc.ID] = true
	}
	injected := 0
	for _, c := range cands {
		if !c.eligible {
			continue
		}
		target := c.target
		if !c.hardFail && keepIDs[c.acc.ID] {
			target = AIObservationPriority
		}
		if target == c.acc.Priority {
			continue
		}
		reason := fmt.Sprintf(
			"%s quality=%.0f%% (eligible=%d bigMiss=%d) priority %d → %d",
			cachePriorityBandPrefix, c.quality*100, c.stats.CacheEligibleRequests, c.stats.CacheBigMissRequests,
			c.acc.Priority, target,
		)
		if c.hardFail {
			reason = fmt.Sprintf(
				"%s 近窗失败风暴(成功=%d 错误=%d) 禁止主层 quality=%.0f%% priority %d → %d",
				cachePriorityBandPrefix, c.recent.Requests, c.recent.Errors, c.quality*100, c.acc.Priority, target,
			)
		}
		replaceOrAppendPriority(decision, decisionAction{
			AccountID:  c.acc.ID,
			Op:         AIOpSetPriority,
			Value:      strconv.Itoa(target),
			Reason:     reason,
			Confidence: 0.94,
		})
		injected++
	}
	return injected
}
