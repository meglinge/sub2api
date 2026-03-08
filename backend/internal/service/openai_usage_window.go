package service

import "time"

const (
	openAIUsageWindowYellow5hPercent = 85.0
	openAIUsageWindowYellow7dPercent = 90.0
	openAIUsageWindowStaleTTL        = 30 * time.Minute
)

type openAIUsageWindowState uint8

const (
	openAIUsageWindowStateGreen openAIUsageWindowState = iota
	openAIUsageWindowStateUnknown
	openAIUsageWindowStateYellow
	openAIUsageWindowStateRed
)

type openAIUsageWindowEvaluation struct {
	State      openAIUsageWindowState
	NeedsProbe bool
	UpdatedAt  *time.Time
	Reset5hAt  *time.Time
	Reset7dAt  *time.Time
}

type openAIWindowAccountCandidate struct {
	account *Account
	window  openAIUsageWindowEvaluation
}

func (e openAIUsageWindowEvaluation) windowFactor() float64 {
	switch e.State {
	case openAIUsageWindowStateGreen:
		return 1.0
	case openAIUsageWindowStateUnknown:
		return 0.82
	case openAIUsageWindowStateYellow:
		return 0.18
	default:
		return 0.0
	}
}

func evaluateOpenAIUsageWindow(account *Account, now time.Time) openAIUsageWindowEvaluation {
	eval := openAIUsageWindowEvaluation{
		State: openAIUsageWindowStateUnknown,
	}
	if account == nil || !account.IsOpenAI() {
		return eval
	}

	updatedAt := openAICodexUsageUpdatedAt(account.Extra)
	if updatedAt != nil {
		eval.UpdatedAt = updatedAt
	}

	progress5h := buildCodexUsageProgressFromExtra(account.Extra, "5h", now)
	progress7d := buildCodexUsageProgressFromExtra(account.Extra, "7d", now)
	if progress5h != nil {
		eval.Reset5hAt = progress5h.ResetsAt
	}
	if progress7d != nil {
		eval.Reset7dAt = progress7d.ResetsAt
	}

	state5h, known5h := classifyOpenAIUsageProgress(progress5h, updatedAt, openAIUsageWindowYellow5hPercent, now)
	state7d, known7d := classifyOpenAIUsageProgress(progress7d, updatedAt, openAIUsageWindowYellow7dPercent, now)

	switch {
	case state5h == openAIUsageWindowStateRed || state7d == openAIUsageWindowStateRed:
		eval.State = openAIUsageWindowStateRed
	case state5h == openAIUsageWindowStateYellow || state7d == openAIUsageWindowStateYellow:
		eval.State = openAIUsageWindowStateYellow
	case state5h == openAIUsageWindowStateUnknown || state7d == openAIUsageWindowStateUnknown:
		eval.State = openAIUsageWindowStateUnknown
	default:
		eval.State = openAIUsageWindowStateGreen
	}

	if !known5h && !known7d {
		eval.State = openAIUsageWindowStateUnknown
	}

	eval.NeedsProbe = eval.State == openAIUsageWindowStateUnknown
	if updatedAt != nil && now.Sub(*updatedAt) > openAIUsageWindowStaleTTL && eval.State != openAIUsageWindowStateGreen {
		eval.State = openAIUsageWindowStateUnknown
		eval.NeedsProbe = true
	}

	return eval
}

func classifyOpenAIUsageProgress(progress *UsageProgress, updatedAt *time.Time, yellowThreshold float64, now time.Time) (openAIUsageWindowState, bool) {
	if progress == nil {
		return openAIUsageWindowStateUnknown, false
	}

	if progress.ResetsAt != nil && !progress.ResetsAt.After(now) {
		return openAIUsageWindowStateUnknown, true
	}

	utilization := progress.Utilization
	switch {
	case utilization >= 100:
		if updatedAt != nil && now.Sub(*updatedAt) > openAIUsageWindowStaleTTL {
			return openAIUsageWindowStateUnknown, true
		}
		return openAIUsageWindowStateRed, true
	case utilization >= yellowThreshold:
		if updatedAt != nil && now.Sub(*updatedAt) > openAIUsageWindowStaleTTL {
			return openAIUsageWindowStateUnknown, true
		}
		return openAIUsageWindowStateYellow, true
	default:
		return openAIUsageWindowStateGreen, true
	}
}

func openAICodexUsageUpdatedAt(extra map[string]any) *time.Time {
	if len(extra) == 0 {
		return nil
	}
	raw, ok := extra["codex_usage_updated_at"]
	if !ok || raw == nil {
		return nil
	}
	rawString := toString(raw)
	if rawString == "" {
		return nil
	}
	t, err := parseTime(rawString)
	if err != nil {
		return nil
	}
	return &t
}

func partitionOpenAIWindowCandidates(accounts []*Account, now time.Time) (preferred []openAIWindowAccountCandidate, degraded []openAIWindowAccountCandidate) {
	if len(accounts) == 0 {
		return nil, nil
	}
	preferred = make([]openAIWindowAccountCandidate, 0, len(accounts))
	degraded = make([]openAIWindowAccountCandidate, 0, len(accounts))
	for _, account := range accounts {
		eval := evaluateOpenAIUsageWindow(account, now)
		candidate := openAIWindowAccountCandidate{
			account: account,
			window:  eval,
		}
		switch eval.State {
		case openAIUsageWindowStateRed:
			continue
		case openAIUsageWindowStateYellow:
			degraded = append(degraded, candidate)
		default:
			preferred = append(preferred, candidate)
		}
	}
	return preferred, degraded
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}
