package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

type UsageRecord struct {
	ID                       string    `json:"id"`
	APIKeyID                 int64     `json:"api_key_id"`
	Endpoint                 string    `json:"endpoint"`
	Model                    string    `json:"model,omitempty"`
	ChatbotID                string    `json:"chatbot_id,omitempty"`
	ProviderID               string    `json:"provider_id,omitempty"`
	ProviderPricingID        *int64    `json:"provider_pricing_id,omitempty"`
	PricingCurrency          string    `json:"pricing_currency,omitempty"`
	InputTokenCentsPer1K     float64   `json:"input_token_cents_per_1k,omitempty"`
	OutputTokenCentsPer1K    float64   `json:"output_token_cents_per_1k,omitempty"`
	EmbeddingTokenCentsPer1K float64   `json:"embedding_token_cents_per_1k,omitempty"`
	PromptTokens             int64     `json:"prompt_tokens"`
	CompletionTokens         int64     `json:"completion_tokens"`
	TotalTokens              int64     `json:"total_tokens"`
	CostCents                int64     `json:"cost_cents"`
	CostMicrocents           int64     `json:"-"`
	StatusCode               int       `json:"status_code"`
	CreatedAt                time.Time `json:"created_at"`
}

const microcentsPerCent int64 = 1_000_000

type UsageRepo struct {
	db *sql.DB
}

func NewUsageRepo(db *sql.DB) *UsageRepo {
	return &UsageRepo{db: db}
}

func (r *UsageRepo) Record(ctx context.Context, rec UsageRecord) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := populateUsagePricingSnapshot(ctx, tx, &rec); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_records(
			id, api_key_id, endpoint, model, chatbot_id,
			provider_id, provider_pricing_id, pricing_currency,
			input_token_cents_per_1k, output_token_cents_per_1k, embedding_token_cents_per_1k,
			prompt_tokens, completion_tokens, total_tokens, cost_cents, cost_microcents,
			status_code, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
	`, rec.ID, rec.APIKeyID, rec.Endpoint, nullableString(rec.Model),
		nullableString(rec.ChatbotID), nullableString(rec.ProviderID), nullableInt64(rec.ProviderPricingID),
		nullableString(rec.PricingCurrency), rec.InputTokenCentsPer1K, rec.OutputTokenCentsPer1K,
		rec.EmbeddingTokenCentsPer1K, rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens, rec.CostCents,
		rec.CostMicrocents, rec.StatusCode,
	); err != nil {
		return fmt.Errorf("insert usage record: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE api_keys
		SET total_tokens = total_tokens + ?,
		    total_cost_cents = total_cost_cents + ?,
		    total_cost_microcents = total_cost_microcents + ?,
		    updated_at = datetime('now')
		WHERE id = ?
		  AND is_deleted = 0
	`, rec.TotalTokens, rec.CostCents, rec.CostMicrocents, rec.APIKeyID); err != nil {
		return fmt.Errorf("update api key totals: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage tx: %w", err)
	}
	return nil
}

func populateUsagePricingSnapshot(ctx context.Context, tx *sql.Tx, rec *UsageRecord) error {
	if rec == nil {
		return nil
	}
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
	if strings.TrimSpace(rec.Model) != "" {
		pricing, err := latestUsagePricing(ctx, tx, rec.ProviderID, rec.Model)
		if err != nil {
			return err
		}
		if pricing != nil {
			rec.ProviderID = pricing.ProviderID
			rec.ProviderPricingID = &pricing.ID
			rec.PricingCurrency = pricing.Currency
			rec.InputTokenCentsPer1K = pricing.InputTokenCentsPer1K
			rec.OutputTokenCentsPer1K = pricing.OutputTokenCentsPer1K
			rec.EmbeddingTokenCentsPer1K = pricing.EmbeddingTokenCentsPer1K
			computed := calculateUsageCostMicrocents(*rec)
			if computed > 0 || rec.CostCents == 0 {
				rec.CostMicrocents = computed
				rec.CostCents = microcentsToCents(computed)
				return nil
			}
		}
	}
	if rec.CostMicrocents == 0 {
		rec.CostMicrocents = centsToMicrocents(rec.CostCents)
	}
	return nil
}

func latestUsagePricing(ctx context.Context, tx *sql.Tx, providerID, modelID string) (*ProviderPricing, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil, nil
	}
	args := []any{modelID}
	where := "model_id = ?"
	if strings.TrimSpace(providerID) != "" {
		where += " AND provider_id = ?"
		args = append(args, strings.TrimSpace(providerID))
	}
	args = append(args, formatDBTime(time.Now().UTC()))
	row := tx.QueryRowContext(ctx, `
		SELECT id, provider_id, model_id, currency,
		       input_token_cents_per_1k, output_token_cents_per_1k, embedding_token_cents_per_1k,
		       effective_at, created_at
		FROM provider_pricing
		WHERE `+where+`
		  AND effective_at <= ?
		ORDER BY effective_at DESC, id DESC
		LIMIT 1
	`, args...)
	pricing, err := scanProviderPricing(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup usage pricing: %w", err)
	}
	return &pricing, nil
}

func calculateUsageCostMicrocents(rec UsageRecord) int64 {
	var cost float64
	if strings.Contains(rec.Endpoint, "/embeddings") {
		tokens := rec.TotalTokens
		if tokens == 0 {
			tokens = rec.PromptTokens + rec.CompletionTokens
		}
		cost = float64(tokens) * rec.EmbeddingTokenCentsPer1K * 1000
	} else {
		cost = float64(rec.PromptTokens)*rec.InputTokenCentsPer1K*1000 +
			float64(rec.CompletionTokens)*rec.OutputTokenCentsPer1K*1000
	}
	if cost <= 0 {
		return 0
	}
	rounded := math.Round(cost)
	if rounded >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(rounded)
}

func centsToMicrocents(cents int64) int64 {
	if cents > math.MaxInt64/microcentsPerCent {
		return math.MaxInt64
	}
	if cents < math.MinInt64/microcentsPerCent {
		return math.MinInt64
	}
	return cents * microcentsPerCent
}

func microcentsToCents(micro int64) int64 {
	if micro <= 0 {
		return 0
	}
	return (micro + microcentsPerCent/2) / microcentsPerCent
}

type UsageSummary struct {
	APIKeyID         int64 `json:"api_key_id"`
	TotalTokens      int64 `json:"total_tokens"`
	TotalCostCents   int64 `json:"total_cost_cents"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	RequestCount     int64 `json:"request_count"`
}

func (r *UsageRepo) SumAll(ctx context.Context, since time.Time) ([]UsageSummary, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT api_key_id,
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_microcents), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COUNT(*)
		FROM usage_records
		WHERE created_at >= ?
		GROUP BY api_key_id
	`, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("sum all usage: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var out []UsageSummary
	for rows.Next() {
		var s UsageSummary
		var costMicrocents int64
		if err := rows.Scan(&s.APIKeyID, &s.TotalTokens, &costMicrocents, &s.PromptTokens, &s.CompletionTokens, &s.RequestCount); err != nil {
			return nil, fmt.Errorf("scan usage summary: %w", err)
		}
		s.TotalCostCents = microcentsToCents(costMicrocents)
		out = append(out, s)
	}
	return out, nil
}

type UsageByEndpoint struct {
	Endpoint     string
	RequestCount int64
	TotalTokens  int64
}

type UsageByModel struct {
	Model          string
	RequestCount   int64
	TotalTokens    int64
	TotalCostCents int64
}

func (r *UsageRepo) SumByEndpoint(ctx context.Context, since time.Time) ([]UsageByEndpoint, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT endpoint, COUNT(*) as request_count, COALESCE(SUM(total_tokens),0) as total_tokens
		FROM usage_records
		WHERE created_at >= ?
		GROUP BY endpoint
		ORDER BY request_count DESC
	`, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("sum by endpoint: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageByEndpoint
	for rows.Next() {
		var e UsageByEndpoint
		if err := rows.Scan(&e.Endpoint, &e.RequestCount, &e.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan usage by endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

func (r *UsageRepo) SumByModel(ctx context.Context, since time.Time) ([]UsageByModel, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT model, COUNT(*) as request_count, COALESCE(SUM(total_tokens),0) as total_tokens, COALESCE(SUM(cost_microcents),0) as total_cost_microcents
		FROM usage_records
		WHERE created_at >= ?
		GROUP BY model
		ORDER BY request_count DESC
	`, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("sum by model: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UsageByModel
	for rows.Next() {
		var m UsageByModel
		var costMicrocents int64
		if err := rows.Scan(&m.Model, &m.RequestCount, &m.TotalTokens, &costMicrocents); err != nil {
			return nil, fmt.Errorf("scan usage by model: %w", err)
		}
		m.TotalCostCents = microcentsToCents(costMicrocents)
		out = append(out, m)
	}
	return out, nil
}

func (r *UsageRepo) SumTotal(ctx context.Context, since time.Time) (totalRequests, totalTokens, totalCostCents int64, err error) {
	var costMicrocents int64
	err = r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) as total_requests, COALESCE(SUM(total_tokens),0) as total_tokens, COALESCE(SUM(cost_microcents),0) as total_cost_microcents
		FROM usage_records
		WHERE created_at >= ?
	`, since.UTC().Format("2006-01-02 15:04:05")).Scan(&totalRequests, &totalTokens, &costMicrocents)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sum total usage: %w", err)
	}
	return totalRequests, totalTokens, microcentsToCents(costMicrocents), nil
}

func (r *UsageRepo) SumSince(ctx context.Context, apiKeyID int64, since time.Time) (totalTokens int64, costMicrocents int64, err error) {
	var tokens sql.NullInt64
	var micro sql.NullInt64
	err = r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_microcents), 0)
		FROM usage_records
		WHERE api_key_id = ?
		  AND created_at >= ?
	`, apiKeyID, since.UTC().Format("2006-01-02 15:04:05")).Scan(&tokens, &micro)
	if err != nil {
		return 0, 0, fmt.Errorf("sum usage since: %w", err)
	}
	return tokens.Int64, micro.Int64, nil
}

// KeyTotals returns the lifetime api_keys counters Record maintains — the
// budget lifetime path needs them after the admin key surface moved to the
// postgres identity store (01-16); both die with this store in 01-17.
func (r *UsageRepo) KeyTotals(ctx context.Context, apiKeyID int64) (totalTokens, costMicrocents int64, err error) {
	var tokens sql.NullInt64
	var micro sql.NullInt64
	err = r.db.QueryRowContext(ctx, `
		SELECT total_tokens, total_cost_microcents
		FROM api_keys
		WHERE id = ? AND is_deleted = 0
	`, apiKeyID).Scan(&tokens, &micro)
	if err != nil {
		return 0, 0, fmt.Errorf("key totals: %w", err)
	}
	return tokens.Int64, micro.Int64, nil
}
