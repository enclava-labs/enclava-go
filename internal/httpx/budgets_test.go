package httpx

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/store/sqlite"
)

func TestEnforceBudgetUsesActiveBudgetRows(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "budgets.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	keyID := insertBudgetAPIKey(t, db, "budget-test")
	usageRepo := sqlite.NewUsageRepo(db.SQL)
	if err := usageRepo.Record(ctx, sqlite.UsageRecord{
		ID:          "usage_budget_test",
		APIKeyID:    keyID,
		Endpoint:    "/api/v1/chat/completions",
		Model:       "model",
		TotalTokens: 5,
		CostCents:   10,
		StatusCode:  200,
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	limitCents := int64(10)
	if err := sqlite.NewBudgetsRepo(db.SQL).Upsert(ctx, sqlite.Budget{
		ID:          "budget_test",
		Name:        "strict monthly spend",
		SubjectType: "api_key",
		APIKeyID:    &keyID,
		LimitCents:  &limitCents,
		Period:      "total",
		IsActive:    true,
	}); err != nil {
		t.Fatalf("upsert budget: %v", err)
	}

	app := &App{
		usageRepo:   usageRepo,
		budgetsRepo: sqlite.NewBudgetsRepo(db.SQL),
	}
	err = app.EnforceBudget(ctx, auth.Principal{APIKeyID: keyID})
	if err == nil {
		t.Fatalf("expected budget exceeded")
	}
	var budgetErr budgetExceededError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected budgetExceededError, got %T %v", err, err)
	}
}

func TestFractionalEmbeddingCostTripsOneCentBudget(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fractional-budget.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	singleKey := insertBudgetAPIKey(t, db, "fractional-one")
	manyKey := insertBudgetAPIKey(t, db, "fractional-ten")
	if _, err := sqlite.NewPricingRepo(db.SQL).Create(ctx, sqlite.ProviderPricing{
		ProviderID:               "tinfoil",
		ModelID:                  "embed-budget",
		Currency:                 "USD",
		EmbeddingTokenCentsPer1K: 0.12,
		EffectiveAt:              time.Now().Add(-time.Minute).UTC(),
	}, sqlite.PricingAuditLog{ID: "priceaudit_budget_fractional"}); err != nil {
		t.Fatalf("create pricing: %v", err)
	}

	usageRepo := sqlite.NewUsageRepo(db.SQL)
	recordEmbedding := func(id string, keyID int64) {
		t.Helper()
		if err := usageRepo.Record(ctx, sqlite.UsageRecord{
			ID:          id,
			APIKeyID:    keyID,
			Endpoint:    "/api/v1/embeddings",
			Model:       "embed-budget",
			ProviderID:  "tinfoil",
			TotalTokens: 1000,
			StatusCode:  200,
		}); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	recordEmbedding("usage_fractional_single", singleKey)
	var costCents, costMicrocents int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT cost_cents, cost_microcents FROM usage_records WHERE id = ?`, "usage_fractional_single").Scan(&costCents, &costMicrocents); err != nil {
		t.Fatalf("query single row: %v", err)
	}
	if costCents != 0 || costMicrocents == 0 {
		t.Fatalf("single sub-cent row cost_cents=%d cost_microcents=%d", costCents, costMicrocents)
	}

	limit := int64(1)
	budgetsRepo := sqlite.NewBudgetsRepo(db.SQL)
	if err := budgetsRepo.Upsert(ctx, sqlite.Budget{
		ID:          "budget_fractional_single",
		Name:        "one cent single",
		SubjectType: "api_key",
		APIKeyID:    &singleKey,
		LimitCents:  &limit,
		Period:      "total",
		IsActive:    true,
	}); err != nil {
		t.Fatalf("upsert single budget: %v", err)
	}
	app := &App{usageRepo: usageRepo, budgetsRepo: budgetsRepo}
	if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: singleKey}); err != nil {
		t.Fatalf("single sub-cent row should not trip a 1 cent budget: %v", err)
	}

	for i := 0; i < 10; i++ {
		recordEmbedding("usage_fractional_many_"+strconv.Itoa(i), manyKey)
	}
	var sumMicro int64
	if err := db.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_microcents), 0) FROM usage_records WHERE api_key_id = ?`, manyKey).Scan(&sumMicro); err != nil {
		t.Fatalf("sum microcents: %v", err)
	}
	if sumMicro == 0 || sumMicro < 1_000_000 {
		t.Fatalf("ten embedding rows should sum past 1 cent, got %d microcents", sumMicro)
	}
	if err := budgetsRepo.Upsert(ctx, sqlite.Budget{
		ID:          "budget_fractional_many",
		Name:        "one cent many",
		SubjectType: "api_key",
		APIKeyID:    &manyKey,
		LimitCents:  &limit,
		Period:      "total",
		IsActive:    true,
	}); err != nil {
		t.Fatalf("upsert many budget: %v", err)
	}
	err = app.EnforceBudget(ctx, auth.Principal{APIKeyID: manyKey})
	if err == nil {
		t.Fatalf("expected 1 cent budget to trip, sum=%d", sumMicro)
	}
	var budgetErr budgetExceededError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("expected budgetExceededError, got %T %v", err, err)
	}
}

func TestPeriodicBudgetsResetWhenStartsAtIsSet(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "period-budgets.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	usageRepo := sqlite.NewUsageRepo(db.SQL)
	budgetsRepo := sqlite.NewBudgetsRepo(db.SQL)
	app := &App{usageRepo: usageRepo, budgetsRepo: budgetsRepo}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lastMonth := monthStart.AddDate(0, -1, 0)
	thisMonthUsage := now.Add(-time.Second)
	if thisMonthUsage.Before(monthStart) {
		thisMonthUsage = monthStart
	}

	t.Run("monthly activated last month ignores last month", func(t *testing.T) {
		quiet := insertBudgetAPIKey(t, db, "monthly-old-quiet")
		insertBudgetUsage(t, db, "usage_monthly_old_quiet_prev", quiet, 100, lastMonth.AddDate(0, 0, 2))
		insertBudgetUsage(t, db, "usage_monthly_old_quiet_now", quiet, 1, thisMonthUsage)
		upsertCentBudget(t, budgetsRepo, "budget_monthly_old_quiet", quiet, 50, "monthly", &lastMonth)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: quiet}); err != nil {
			t.Fatalf("last month usage should not count: %v", err)
		}

		hot := insertBudgetAPIKey(t, db, "monthly-old-hot")
		insertBudgetUsage(t, db, "usage_monthly_old_hot_now", hot, 60, thisMonthUsage)
		upsertCentBudget(t, budgetsRepo, "budget_monthly_old_hot", hot, 50, "monthly", &lastMonth)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: hot}); err == nil {
			t.Fatal("current month usage should still count")
		}
	})

	t.Run("monthly activated this month ignores earlier usage", func(t *testing.T) {
		if now.Sub(monthStart) < 2*time.Second {
			t.Skip("too close to UTC month start")
		}
		activation := monthStart.Add(now.Sub(monthStart) / 2).Truncate(time.Second)
		if !activation.After(monthStart) {
			activation = monthStart.Add(time.Second)
		}
		after := activation.Add(time.Second)
		if after.After(now) {
			after = now
		}
		if !after.After(activation) {
			t.Skip("unable to place usage after activation")
		}

		quiet := insertBudgetAPIKey(t, db, "monthly-new-quiet")
		insertBudgetUsage(t, db, "usage_monthly_new_quiet_before", quiet, 100, monthStart)
		insertBudgetUsage(t, db, "usage_monthly_new_quiet_after", quiet, 1, after)
		upsertCentBudget(t, budgetsRepo, "budget_monthly_new_quiet", quiet, 50, "monthly", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: quiet}); err != nil {
			t.Fatalf("usage before activation should not count: %v", err)
		}

		hot := insertBudgetAPIKey(t, db, "monthly-new-hot")
		insertBudgetUsage(t, db, "usage_monthly_new_hot_before", hot, 1, monthStart)
		insertBudgetUsage(t, db, "usage_monthly_new_hot_after", hot, 60, after)
		upsertCentBudget(t, budgetsRepo, "budget_monthly_new_hot", hot, 50, "monthly", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: hot}); err == nil {
			t.Fatal("usage after activation should count")
		}
	})

	t.Run("daily activated yesterday ignores yesterday", func(t *testing.T) {
		activation := dayStart.Add(-12 * time.Hour)
		yesterday := dayStart.Add(-2 * time.Hour)
		todayUsage := now.Add(-time.Second)
		if todayUsage.Before(dayStart) {
			todayUsage = dayStart
		}
		quiet := insertBudgetAPIKey(t, db, "daily-old-quiet")
		insertBudgetUsage(t, db, "usage_daily_old_quiet_prev", quiet, 100, yesterday)
		insertBudgetUsage(t, db, "usage_daily_old_quiet_now", quiet, 1, todayUsage)
		upsertCentBudget(t, budgetsRepo, "budget_daily_old_quiet", quiet, 50, "daily", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: quiet}); err != nil {
			t.Fatalf("yesterday usage should not count: %v", err)
		}

		hot := insertBudgetAPIKey(t, db, "daily-old-hot")
		insertBudgetUsage(t, db, "usage_daily_old_hot_now", hot, 60, todayUsage)
		upsertCentBudget(t, budgetsRepo, "budget_daily_old_hot", hot, 50, "daily", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: hot}); err == nil {
			t.Fatal("today usage should still count")
		}
	})

	t.Run("daily activated today ignores earlier today", func(t *testing.T) {
		if now.Sub(dayStart) < 2*time.Second {
			t.Skip("too close to UTC midnight")
		}
		activation := dayStart.Add(now.Sub(dayStart) / 2).Truncate(time.Second)
		if !activation.After(dayStart) {
			activation = dayStart.Add(time.Second)
		}
		after := activation.Add(time.Second)
		if after.After(now) {
			after = now
		}
		if !after.After(activation) {
			t.Skip("unable to place usage after activation")
		}

		quiet := insertBudgetAPIKey(t, db, "daily-new-quiet")
		insertBudgetUsage(t, db, "usage_daily_new_quiet_before", quiet, 100, dayStart)
		insertBudgetUsage(t, db, "usage_daily_new_quiet_after", quiet, 1, after)
		upsertCentBudget(t, budgetsRepo, "budget_daily_new_quiet", quiet, 50, "daily", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: quiet}); err != nil {
			t.Fatalf("usage before today's activation should not count: %v", err)
		}

		hot := insertBudgetAPIKey(t, db, "daily-new-hot")
		insertBudgetUsage(t, db, "usage_daily_new_hot_before", hot, 1, dayStart)
		insertBudgetUsage(t, db, "usage_daily_new_hot_after", hot, 60, after)
		upsertCentBudget(t, budgetsRepo, "budget_daily_new_hot", hot, 50, "daily", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: hot}); err == nil {
			t.Fatal("usage after today's activation should count")
		}
	})

	t.Run("total with starts_at sums from activation", func(t *testing.T) {
		activation := dayStart.Add(-time.Hour)
		if !activation.Before(dayStart) && now.Sub(dayStart) < time.Second {
			activation = now.Add(-time.Millisecond)
		}
		before := activation.Add(-time.Hour)
		after := now.Add(-time.Second)
		if !after.After(activation) {
			after = activation
		}

		quiet := insertBudgetAPIKey(t, db, "total-start-quiet")
		setKeyLifetimeCost(t, db, quiet, 500)
		insertBudgetUsage(t, db, "usage_total_start_quiet_before", quiet, 500, before)
		insertBudgetUsage(t, db, "usage_total_start_quiet_after", quiet, 1, after)
		upsertCentBudget(t, budgetsRepo, "budget_total_start_quiet", quiet, 10, "total", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: quiet}); err != nil {
			t.Fatalf("total budget should ignore usage before starts_at: %v", err)
		}

		hot := insertBudgetAPIKey(t, db, "total-start-hot")
		setKeyLifetimeCost(t, db, hot, 0)
		insertBudgetUsage(t, db, "usage_total_start_hot_before", hot, 1, before)
		insertBudgetUsage(t, db, "usage_total_start_hot_after", hot, 20, after)
		upsertCentBudget(t, budgetsRepo, "budget_total_start_hot", hot, 10, "total", &activation)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: hot}); err == nil {
			t.Fatal("total budget with starts_at should include usage after activation")
		}
	})

	t.Run("total without starts_at uses lifetime totals", func(t *testing.T) {
		key := insertBudgetAPIKey(t, db, "total-lifetime")
		if err := usageRepo.Record(ctx, sqlite.UsageRecord{
			ID:          "usage_total_lifetime_recorded",
			APIKeyID:    key,
			Endpoint:    "/api/v1/chat/completions",
			TotalTokens: 5,
			CostCents:   5,
			StatusCode:  200,
		}); err != nil {
			t.Fatalf("record usage: %v", err)
		}
		insertBudgetUsage(t, db, "usage_total_lifetime_untracked", key, 100, lastMonth)
		upsertCentBudget(t, budgetsRepo, "budget_total_lifetime", key, 10, "total", nil)
		if err := app.EnforceBudget(ctx, auth.Principal{APIKeyID: key}); err != nil {
			t.Fatalf("lifetime total of 5 cents should stay under a 10 cent budget: %v", err)
		}
	})
}

// insertBudgetAPIKey seeds a legacy sqlite api_keys row directly — the
// numeric id is what budgets/usage still FK against until 01-17 ports them.
// (The admin key surface lives on the postgres identity store; httpx has no
// sqlite keys repo anymore.)
func insertBudgetAPIKey(t *testing.T, db *sqlite.DB, name string) int64 {
	t.Helper()
	res, err := db.SQL.ExecContext(context.Background(), `
		INSERT INTO api_keys(name, key_prefix, key_hash, key_hash_algo, is_active, scopes)
		VALUES (?, 'enc_test_', ?, 'sha256', 1, '[]')
	`, name, name+"-secret")
	if err != nil {
		t.Fatalf("insert api key %s: %v", name, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertBudgetUsage(t *testing.T, db *sqlite.DB, id string, keyID, cents int64, at time.Time) {
	t.Helper()
	_, err := db.SQL.ExecContext(context.Background(), `
		INSERT INTO usage_records(id, api_key_id, endpoint, total_tokens, cost_cents, cost_microcents, status_code, created_at)
		VALUES (?, ?, '/api/v1/chat/completions', 1, ?, ?, 200, ?)
	`, id, keyID, cents, cents*1_000_000, at.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		t.Fatalf("insert usage %s: %v", id, err)
	}
}

func setKeyLifetimeCost(t *testing.T, db *sqlite.DB, keyID, cents int64) {
	t.Helper()
	_, err := db.SQL.ExecContext(context.Background(), `
		UPDATE api_keys
		SET total_cost_cents = ?, total_cost_microcents = ?
		WHERE id = ?
	`, cents, cents*1_000_000, keyID)
	if err != nil {
		t.Fatalf("set lifetime cost: %v", err)
	}
}

func upsertCentBudget(t *testing.T, repo *sqlite.BudgetsRepo, id string, keyID, limitCents int64, period string, startsAt *time.Time) {
	t.Helper()
	if err := repo.Upsert(context.Background(), sqlite.Budget{
		ID:          id,
		Name:        id,
		SubjectType: "api_key",
		APIKeyID:    &keyID,
		LimitCents:  &limitCents,
		Period:      period,
		StartsAt:    startsAt,
		IsActive:    true,
	}); err != nil {
		t.Fatalf("upsert budget %s: %v", id, err)
	}
}
