package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// AIPilotRepository persists AI autopilot runs/actions via raw SQL.
type AIPilotRepository struct {
	db *sql.DB
}

func NewAIPilotRepository(db *sql.DB) *AIPilotRepository {
	return &AIPilotRepository{db: db}
}

func (r *AIPilotRepository) CreateRun(ctx context.Context, run service.AIRun) (service.AIRun, error) {
	if run.TS.IsZero() {
		run.TS = time.Now()
	}
	if run.Turns <= 0 {
		run.Turns = 1
	}
	if run.Observations == "" {
		run.Observations = "[]"
	}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO ai_runs (
			ts, trigger, model, window_from, window_to, status, error, summary, observations,
			input_tokens, output_tokens, cost_micros, latency_ms, turns, raw_request, raw_response
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14,$15,$16)
		RETURNING id
	`,
		run.TS, run.Trigger, run.Model, run.WindowFrom, run.WindowTo, run.Status, run.Error, run.Summary, run.Observations,
		run.InputTokens, run.OutputTokens, run.CostMicros, run.LatencyMs, run.Turns, run.RawRequest, run.RawResponse,
	).Scan(&run.ID)
	return run, err
}

func (r *AIPilotRepository) CreateAction(ctx context.Context, a service.AIAction) (service.AIAction, error) {
	if a.TS.IsZero() {
		a.TS = time.Now()
	}
	if a.BaselineJSON == "" {
		a.BaselineJSON = "{}"
	}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO ai_actions (
			run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, rolled_back_at, baseline_json
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id
	`,
		a.RunID, a.TS, a.AccountID, a.AccountName, a.Op, a.Before, a.After, a.Reason, a.Confidence,
		a.State, a.RejectReason, a.Outcome, a.RolledBackAt, a.BaselineJSON,
	).Scan(&a.ID)
	return a, err
}

func (r *AIPilotRepository) ListRuns(ctx context.Context, limit int, beforeID int64) ([]service.AIRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := `
		SELECT r.id, r.ts, r.trigger, r.model, r.window_from, r.window_to, r.status, r.error,
			r.summary, r.observations::text, r.input_tokens, r.output_tokens, r.cost_micros,
			r.latency_ms, r.turns,
			COALESCE((SELECT COUNT(*) FROM ai_actions a WHERE a.run_id = r.id AND a.state = $1), 0) AS suggested_count
		FROM ai_runs r
	`
	args := []any{service.AIActionSuggested}
	if beforeID > 0 {
		q += ` WHERE r.id < $2`
		args = append(args, beforeID)
		q += ` ORDER BY r.id DESC LIMIT $3`
		args = append(args, limit)
	} else {
		q += ` ORDER BY r.id DESC LIMIT $2`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]service.AIRun, 0)
	for rows.Next() {
		var run service.AIRun
		var obs string
		if err := rows.Scan(
			&run.ID, &run.TS, &run.Trigger, &run.Model, &run.WindowFrom, &run.WindowTo, &run.Status, &run.Error,
			&run.Summary, &obs, &run.InputTokens, &run.OutputTokens, &run.CostMicros,
			&run.LatencyMs, &run.Turns, &run.SuggestedCount,
		); err != nil {
			return nil, err
		}
		run.Observations = obs
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *AIPilotRepository) GetRun(ctx context.Context, id int64) (*service.AIRun, error) {
	var run service.AIRun
	var obs string
	err := r.db.QueryRowContext(ctx, `
		SELECT id, ts, trigger, model, window_from, window_to, status, error, summary, observations::text,
			input_tokens, output_tokens, cost_micros, latency_ms, turns, raw_request, raw_response
		FROM ai_runs WHERE id = $1
	`, id).Scan(
		&run.ID, &run.TS, &run.Trigger, &run.Model, &run.WindowFrom, &run.WindowTo, &run.Status, &run.Error,
		&run.Summary, &obs, &run.InputTokens, &run.OutputTokens, &run.CostMicros, &run.LatencyMs, &run.Turns,
		&run.RawRequest, &run.RawResponse,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("ai run not found")
	}
	if err != nil {
		return nil, err
	}
	run.Observations = obs
	actions, err := r.ListActionsByRun(ctx, id)
	if err != nil {
		return nil, err
	}
	run.Actions = actions
	for _, a := range actions {
		if a.State == service.AIActionSuggested {
			run.SuggestedCount++
		}
	}
	return &run, nil
}

func (r *AIPilotRepository) ListActionsByRun(ctx context.Context, runID int64) ([]service.AIAction, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, rolled_back_at
		FROM ai_actions WHERE run_id = $1 ORDER BY id ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAIActions(rows)
}

func (r *AIPilotRepository) GetAction(ctx context.Context, id int64) (*service.AIAction, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, rolled_back_at
		FROM ai_actions WHERE id = $1
	`, id)
	a, err := scanAIAction(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("ai action not found")
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (r *AIPilotRepository) UpdateActionState(ctx context.Context, id int64, state, rejectReason string, rolledBackAt *time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE ai_actions SET state = $2, reject_reason = $3, rolled_back_at = $4 WHERE id = $1
	`, id, state, rejectReason, rolledBackAt)
	return err
}

func (r *AIPilotRepository) ListSuggestions(ctx context.Context, limit int) ([]service.AIAction, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Only surface recent suggestions; older ones are stale noise in the panel.
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, rolled_back_at
		FROM ai_actions
		WHERE state = $1 AND ts > NOW() - INTERVAL '2 hours'
		ORDER BY ts DESC LIMIT $2
	`, service.AIActionSuggested, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAIActions(rows)
}

// DismissSuggestions auto-clears pending suggestions (stale age and/or all when auto mode).
func (r *AIPilotRepository) DismissSuggestions(ctx context.Context, all bool, olderThan *time.Time, reason string) (int64, error) {
	if reason == "" {
		reason = "auto-dismiss"
	}
	var res sql.Result
	var err error
	if all {
		res, err = r.db.ExecContext(ctx, `
			UPDATE ai_actions SET state = $1, reject_reason = $2
			WHERE state = $3
		`, service.AIActionDismissed, reason, service.AIActionSuggested)
	} else if olderThan != nil {
		res, err = r.db.ExecContext(ctx, `
			UPDATE ai_actions SET state = $1, reject_reason = $2
			WHERE state = $3 AND ts < $4
		`, service.AIActionDismissed, reason, service.AIActionSuggested, *olderThan)
	} else {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// LastAppliedAt returns the last applied AI action time for an account (for cooldown).
func (r *AIPilotRepository) LastAppliedAt(ctx context.Context, accountID int64) (*time.Time, error) {
	var ts sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT ts FROM ai_actions
		WHERE account_id = $1 AND state = $2
		ORDER BY ts DESC LIMIT 1
	`, accountID, service.AIActionApplied).Scan(&ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !ts.Valid {
		return nil, nil
	}
	t := ts.Time
	return &t, nil
}

// LastSpareDemotions returns latest applied set_priority demotion into spare tier
// (after_val > before_val and after_val ≥ 150) per account since `since`.
func (r *AIPilotRepository) LastSpareDemotions(ctx context.Context, accountIDs []int64, since time.Time) (map[int64]time.Time, error) {
	out := make(map[int64]time.Time)
	if len(accountIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(accountIDs))
	args := make([]any, 0, len(accountIDs)+3)
	args = append(args, service.AIActionApplied, service.AIOpSetPriority, since)
	for i, id := range accountIDs {
		ph[i] = fmt.Sprintf("$%d", i+4)
		args = append(args, id)
	}
	// before_val/after_val are text; only pure integer rows count.
	q := fmt.Sprintf(`
		SELECT account_id, MAX(ts) AS last_ts
		FROM ai_actions
		WHERE state = $1
		  AND op = $2
		  AND ts >= $3
		  AND account_id IN (%s)
		  AND before_val ~ '^[0-9]+$'
		  AND after_val ~ '^[0-9]+$'
		  AND after_val::int > before_val::int
		  AND after_val::int >= %d
		GROUP BY account_id
	`, strings.Join(ph, ","), service.AIPriorityBuriedThreshold)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ts time.Time
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// AggregateAccountTraffic loads usage_logs aggregates for openai accounts in window.
func (r *AIPilotRepository) AggregateAccountTraffic(ctx context.Context, from, to time.Time, accountIDs []int64) (map[int64]service.AccountTrafficStats, error) {
	out := make(map[int64]service.AccountTrafficStats)
	if len(accountIDs) == 0 {
		return out, nil
	}
	// Build placeholders
	ph := make([]string, len(accountIDs))
	args := make([]any, 0, len(accountIDs)+2)
	args = append(args, from, to)
	for i, id := range accountIDs {
		ph[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, id)
	}
	// usage_logs records completed billable traffic (mostly successes). Error rates
	// come from ops_error_logs when available.
	q := fmt.Sprintf(`
		SELECT account_id,
			COUNT(*)::int AS requests,
			COALESCE(AVG(duration_ms) FILTER (WHERE duration_ms IS NOT NULL AND duration_ms > 0), 0)::float8 AS avg_duration,
			COALESCE(AVG(first_token_ms) FILTER (WHERE first_token_ms IS NOT NULL AND first_token_ms > 0), 0)::float8 AS avg_ttfb
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
			AND account_id IN (%s)
		GROUP BY account_id
	`, strings.Join(ph, ","))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return out, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s service.AccountTrafficStats
		if err := rows.Scan(&s.AccountID, &s.Requests, &s.AvgDuration, &s.AvgFirstToken); err != nil {
			return nil, err
		}
		s.Successes = s.Requests
		out[s.AccountID] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Merge ops error counts when table exists.
	// Not account death: 499 cancel, 429 rate limit, non-4xx (status 200 "errors").
	// 429-majority is what made grok-4.6 disable 2chat (TTFB 3.7s, still completing).
	// 401/403/502/5xx still count.
	eq := fmt.Sprintf(`
		SELECT account_id, COUNT(*)::int
		FROM ops_error_logs
		WHERE created_at >= $1 AND created_at < $2
			AND account_id IN (%s)
			AND COALESCE(error_type, '') <> 'rate_limit_error'
			AND (
				status_code IS NULL
				OR (status_code >= 400 AND status_code NOT IN (429, 499))
			)
		GROUP BY account_id
	`, strings.Join(ph, ","))
	erows, err := r.db.QueryContext(ctx, eq, args...)
	if err == nil {
		defer erows.Close()
		for erows.Next() {
			var id int64
			var n int
			if err := erows.Scan(&id, &n); err != nil {
				break
			}
			s := out[id]
			s.AccountID = id
			s.Errors = n
			out[id] = s
		}
	}
	return out, nil
}

// RecentErrorSamples fetches a few recent error snippets per account from ops_error_logs.
func (r *AIPilotRepository) RecentErrorSamples(ctx context.Context, from time.Time, accountID int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE(
			NULLIF(TRIM(COALESCE(error_message, '')), ''),
			NULLIF(TRIM(COALESCE(upstream_error_message, '')), ''),
			'type=' || COALESCE(error_type, '?') || ' status=' || COALESCE(status_code::text, '?')
		)
		FROM ops_error_logs
		WHERE account_id = $1 AND created_at >= $2
			AND COALESCE(error_type, '') <> 'rate_limit_error'
			AND (
				status_code IS NULL
				OR (status_code >= 400 AND status_code NOT IN (429, 499))
			)
		ORDER BY created_at DESC
		LIMIT $3
	`, accountID, from, limit)
	if err != nil {
		// Column names vary across ops migrations; degrade gracefully.
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if len(s) > 300 {
			s = s[:300]
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanAIActions(rows *sql.Rows) ([]service.AIAction, error) {
	out := make([]service.AIAction, 0)
	for rows.Next() {
		a, err := scanAIAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type aiPilotScannable interface {
	Scan(dest ...any) error
}

func scanAIAction(row aiPilotScannable) (service.AIAction, error) {
	var a service.AIAction
	err := row.Scan(
		&a.ID, &a.RunID, &a.TS, &a.AccountID, &a.AccountName, &a.Op, &a.Before, &a.After, &a.Reason, &a.Confidence,
		&a.State, &a.RejectReason, &a.Outcome, &a.RolledBackAt,
	)
	return a, err
}

// EnsureObservationsJSON validates observations is valid JSON array string.
func EnsureObservationsJSON(s string) string {
	if s == "" {
		return "[]"
	}
	var raw any
	if json.Unmarshal([]byte(s), &raw) != nil {
		return "[]"
	}
	return s
}

// ListRunsAsc returns the oldest-first slice of the most recent limit runs (trends axis).
func (r *AIPilotRepository) ListRunsAsc(ctx context.Context, limit int) ([]service.AIRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	// Fetch newest N then reverse for ascending timeline.
	runs, err := r.ListRuns(ctx, limit, 0)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs, nil
}

// ListActionsInRecentRuns returns all actions for the most recent runLimit runs.
func (r *AIPilotRepository) ListActionsInRecentRuns(ctx context.Context, runLimit int) ([]service.AIAction, error) {
	if runLimit <= 0 || runLimit > 200 {
		runLimit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.ts, a.account_id, a.account_name, a.op, a.before_val, a.after_val,
			a.reason, a.confidence, a.state, a.reject_reason, a.outcome, a.rolled_back_at
		FROM ai_actions a
		WHERE a.run_id IN (
			SELECT id FROM ai_runs ORDER BY id DESC LIMIT $1
		)
		ORDER BY a.id ASC
	`, runLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAIActions(rows)
}

// AppendAccountScores inserts a batch of scores for one run.
func (r *AIPilotRepository) AppendAccountScores(ctx context.Context, runID int64, items []service.AIAccountScore) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO ai_account_scores
			(ts, run_id, account_id, account_name, stability, latency, throughput, cost, overall, confidence, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now()
	for _, it := range items {
		if it.AccountID <= 0 {
			continue
		}
		it = service.NormalizeAccountScore(it)
		ts := it.TS
		if ts.IsZero() {
			ts = now
		}
		if _, err := stmt.ExecContext(ctx, ts, runID, it.AccountID, it.AccountName,
			it.Stability, it.Latency, it.Throughput, it.Cost, it.Overall, it.Confidence, it.Note); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LatestAccountScores returns the newest score per account.
func (r *AIPilotRepository) LatestAccountScores(ctx context.Context) (map[int64]service.AIAccountScore, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.id, c.ts, c.run_id, c.account_id, c.account_name,
			c.stability, c.latency, c.throughput, c.cost, c.overall, c.confidence, c.note
		FROM ai_account_scores c
		INNER JOIN (
			SELECT account_id, MAX(ts) AS mts FROM ai_account_scores GROUP BY account_id
		) t ON c.account_id = t.account_id AND c.ts = t.mts
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]service.AIAccountScore)
	for rows.Next() {
		var s service.AIAccountScore
		if err := rows.Scan(&s.ID, &s.TS, &s.RunID, &s.AccountID, &s.AccountName,
			&s.Stability, &s.Latency, &s.Throughput, &s.Cost, &s.Overall, &s.Confidence, &s.Note); err != nil {
			return nil, err
		}
		if prev, ok := out[s.AccountID]; ok && prev.ID > s.ID {
			continue
		}
		out[s.AccountID] = s
	}
	return out, rows.Err()
}

// AccountScoreHistory returns ascending history for charts.
func (r *AIPilotRepository) AccountScoreHistory(ctx context.Context, accountID int64, limit int) ([]service.AIAccountScore, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, ts, run_id, account_id, account_name,
			stability, latency, throughput, cost, overall, confidence, note
		FROM (
			SELECT id, ts, run_id, account_id, account_name,
				stability, latency, throughput, cost, overall, confidence, note
			FROM ai_account_scores
			WHERE account_id = $1
			ORDER BY ts DESC, id DESC
			LIMIT $2
		) sub
		ORDER BY ts ASC, id ASC
	`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []service.AIAccountScore
	for rows.Next() {
		var s service.AIAccountScore
		if err := rows.Scan(&s.ID, &s.TS, &s.RunID, &s.AccountID, &s.AccountName,
			&s.Stability, &s.Latency, &s.Throughput, &s.Cost, &s.Overall, &s.Confidence, &s.Note); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PruneAccountScores deletes scores older than keepDays.
func (r *AIPilotRepository) PruneAccountScores(ctx context.Context, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM ai_account_scores WHERE ts < NOW() - ($1 || ' days')::interval
	`, keepDays)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AggregateAccountTrafficOne aggregates one account in [from,to).
func (r *AIPilotRepository) AggregateAccountTrafficOne(ctx context.Context, accountID int64, from, to time.Time) (service.AccountTrafficStats, error) {
	m, err := r.AggregateAccountTraffic(ctx, from, to, []int64{accountID})
	if err != nil {
		return service.AccountTrafficStats{}, err
	}
	s := m[accountID]
	s.AccountID = accountID
	return s, nil
}

// ListActionsPendingOutcome returns applied actions without outcome yet (oldest first).
func (r *AIPilotRepository) ListActionsPendingOutcome(ctx context.Context, since time.Time, limit int) ([]service.AIAction, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, COALESCE(baseline_json,'{}'), outcome_at, decayed_at, rolled_back_at
		FROM ai_actions
		WHERE state = $1 AND (outcome = '' OR outcome IS NULL) AND ts >= $2
		ORDER BY ts ASC
		LIMIT $3
	`, service.AIActionApplied, since, limit)
	if err != nil {
		// Pre-migration: column may be missing.
		if strings.Contains(err.Error(), "baseline_json") || strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	return scanAIActionsExtra(rows)
}

// NextActionTS returns the next applied action timestamp for the account after afterTS.
func (r *AIPilotRepository) NextActionTS(ctx context.Context, accountID int64, afterTS time.Time) (*time.Time, error) {
	var ts time.Time
	err := r.db.QueryRowContext(ctx, `
		SELECT ts FROM ai_actions
		WHERE account_id = $1 AND state = $2 AND ts > $3
		ORDER BY ts ASC LIMIT 1
	`, accountID, service.AIActionApplied, afterTS).Scan(&ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ts, nil
}

// SetActionOutcome writes outcome + outcome_at.
func (r *AIPilotRepository) SetActionOutcome(ctx context.Context, id int64, outcome string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE ai_actions SET outcome = $2, outcome_at = $3 WHERE id = $1
	`, id, outcome, at)
	return err
}

// ListActionsPendingDecay returns applied demotions not yet decay-processed.
func (r *AIPilotRepository) ListActionsPendingDecay(ctx context.Context, since time.Time, limit int) ([]service.AIAction, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, run_id, ts, account_id, account_name, op, before_val, after_val, reason, confidence,
			state, reject_reason, outcome, COALESCE(baseline_json,'{}'), outcome_at, decayed_at, rolled_back_at
		FROM ai_actions
		WHERE state = $1 AND decayed_at IS NULL AND ts >= $2
			AND op IN ($3,$4,$5)
		ORDER BY ts ASC
		LIMIT $6
	`, service.AIActionApplied, since, service.AIOpSetPriority, service.AIOpSetWeight, service.AIOpDisable, limit)
	if err != nil {
		if strings.Contains(err.Error(), "decayed_at") || strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	return scanAIActionsExtra(rows)
}

// MarkActionDecayed stamps decayed_at.
func (r *AIPilotRepository) MarkActionDecayed(ctx context.Context, id int64, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE ai_actions SET decayed_at = $2 WHERE id = $1`, id, at)
	return err
}

func scanAIActionsExtra(rows *sql.Rows) ([]service.AIAction, error) {
	out := make([]service.AIAction, 0)
	for rows.Next() {
		var a service.AIAction
		var outcomeAt, decayedAt, rolled sql.NullTime
		if err := rows.Scan(
			&a.ID, &a.RunID, &a.TS, &a.AccountID, &a.AccountName, &a.Op, &a.Before, &a.After, &a.Reason, &a.Confidence,
			&a.State, &a.RejectReason, &a.Outcome, &a.BaselineJSON, &outcomeAt, &decayedAt, &rolled,
		); err != nil {
			return nil, err
		}
		if outcomeAt.Valid {
			t := outcomeAt.Time
			a.OutcomeAt = &t
		}
		if decayedAt.Valid {
			t := decayedAt.Time
			a.DecayedAt = &t
		}
		if rolled.Valid {
			t := rolled.Time
			a.RolledBackAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
