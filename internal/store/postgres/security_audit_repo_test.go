package postgres

import (
	"context"
	"testing"
	"time"
)

func TestSecurityAuditRepository(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant, other := tenantForTest(901), tenantForTest(902)
	seedBindingTenant(t, db, tenant)
	seedBindingTenant(t, db, other)
	repo := NewSecurityAuditRepo(db)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := SecurityAuditRow{
		TenantID: tenant, ActorID: "user", ActorKind: "user", CredentialID: "session",
		Action: "key.create", TargetKind: "key", TargetID: "key-1", Outcome: "success",
		ClientIP: "192.0.2.1", RequestID: "request", OccurredAt: now,
		Details: map[string]any{"reason": "test"},
	}
	for _, id := range []string{"a", "b", "c"} {
		row := base
		row.ID = id
		if id == "a" {
			row.OccurredAt = now.Add(-time.Second)
		}
		if err := repo.Insert(ctx, row); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	foreign := base
	foreign.TenantID, foreign.ID = other, "foreign"
	if err := repo.Insert(ctx, foreign); err != nil {
		t.Fatalf("insert foreign: %v", err)
	}

	first, err := repo.List(ctx, tenant, 1, "")
	if err != nil || len(first) != 1 || first[0].ID != "c" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	got := first[0]
	if got.TenantID != tenant || got.ActorID != base.ActorID || got.ActorKind != base.ActorKind ||
		got.CredentialID != base.CredentialID || got.Action != base.Action || got.TargetKind != base.TargetKind ||
		got.TargetID != base.TargetID || got.Outcome != base.Outcome || got.ClientIP != base.ClientIP ||
		got.RequestID != base.RequestID || !got.OccurredAt.Equal(now) || got.Details["reason"] != "test" {
		t.Fatalf("row did not round trip: %+v", got)
	}
	second, err := repo.List(ctx, tenant, 2, got.ID)
	if err != nil || len(second) != 2 || second[0].ID != "b" || second[1].ID != "a" {
		t.Fatalf("second page: %+v, %v", second, err)
	}
	for _, cursor := range []string{"foreign", "missing", "a"} {
		rows, err := repo.List(ctx, tenant, 10, cursor)
		if err != nil || len(rows) != 0 {
			t.Fatalf("cursor %q: %+v, %v", cursor, rows, err)
		}
	}
	if rows, err := repo.List(ctx, other, 10, ""); err != nil || len(rows) != 1 || rows[0].ID != "foreign" {
		t.Fatalf("foreign tenant page: %+v, %v", rows, err)
	}
	duplicate := base
	duplicate.ID = "c"
	if err := repo.Insert(ctx, duplicate); err == nil {
		t.Fatal("duplicate audit ID accepted")
	}
	for _, query := range []string{
		`UPDATE security_audit SET outcome = 'changed' WHERE tenant_id = $1`,
		`DELETE FROM security_audit WHERE tenant_id = $1`,
	} {
		if _, err := db.SQL.ExecContext(ctx, query, tenant); err == nil {
			t.Fatalf("mutation accepted: %s", query)
		}
	}
	if _, err := db.SQL.ExecContext(ctx, `TRUNCATE security_audit`); err == nil {
		t.Fatal("audit truncation accepted")
	}
}

func TestSecurityAuditInvalidInput(t *testing.T) {
	db := RequireTestDB(t)
	ctx := context.Background()
	tenant := tenantForTest(903)
	seedBindingTenant(t, db, tenant)
	repo := NewSecurityAuditRepo(db)
	if err := repo.Insert(ctx, SecurityAuditRow{}); err == nil {
		t.Fatal("missing required fields accepted")
	}
	row := SecurityAuditRow{TenantID: tenant, ID: "id", Action: "test", OccurredAt: time.Now(), Details: map[string]any{"bad": make(chan int)}}
	if err := repo.Insert(ctx, row); err == nil {
		t.Fatal("invalid details accepted")
	}
	row.Details = nil
	if err := repo.Insert(ctx, row); err != nil {
		t.Fatalf("insert nil details: %v", err)
	}
	rows, err := repo.List(ctx, tenant, 1, "")
	if err != nil || len(rows) != 1 || rows[0].Details == nil || len(rows[0].Details) != 0 {
		t.Fatalf("nil details round trip: %+v, %v", rows, err)
	}
	if _, err := repo.List(ctx, tenant, 0, ""); err == nil {
		t.Fatal("nonpositive limit accepted")
	}
	if _, err := repo.List(ctx, "", 1, ""); err == nil {
		t.Fatal("missing tenant accepted")
	}
}
