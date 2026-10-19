package source

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrDenied is the single public outcome for every denied source read:
// unknown, forbidden, stale-grant, incomplete-snapshot and tombstoned
// objects are indistinguishable to callers. Callers must not reveal titles,
// metadata, links or counts before a successful CanRead.
var ErrDenied = errors.New("source: access denied")

// Tx is the transaction handle Apply*Snapshot requires — satisfied by
// *sql.Tx. The database/sql import is types-only and keeps the signature
// assignable without a wrapper.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ReadOptions carries per-request narrowing (AUTH-06). RequestedRestrictions
// may only add denied capabilities/policy labels or raise required assurance
// relative to the persisted effective set — a request that would drop a
// persisted denied capability is rejected.
type ReadOptions struct {
	// Now overrides the evaluation clock; zero uses time.Now. Tests use it
	// to exercise snapshot staleness without sleeping.
	Now                  time.Time
	RequestedRestriction *SourceRestriction
}

// SourceRepository is the narrow persistence boundary consumed by Authorizer
// and by connector observation flows. Implemented by postgres.SourceRepo.
// All methods are tenant-scoped; callers pass tenantIDs explicitly except on
// row-shaped writes that carry TenantID.
//
// Snapshot semantics: only a COMPLETE access_snapshot revision is
// authoritative. ApplyAccessSnapshot/ApplyMembershipSnapshot run inside one
// transaction each — they write revision-qualified rows, mark the snapshot
// complete, then delete rows unseen at that revision. An incomplete
// replacement can never shadow or retire the last complete set.
type SourceRepository interface {
	// Connections
	CreateConnection(ctx context.Context, c Connection) error
	GetConnection(ctx context.Context, tenantID, id string) (Connection, error)
	ListConnections(ctx context.Context, tenantID string) ([]Connection, error)
	UpdateConnectionStatus(ctx context.Context, tenantID, id, status string) error
	SetSyncState(ctx context.Context, tenantID, id string, syncedAt time.Time, syncStatus, syncError string) error

	// Objects. UpsertObject inserts or returns the existing row for the same
	// (tenant, connection, kind, native_id) identity — it never mutates
	// identity fields. MarkObjectDeleted/TombstoneObject set the matching
	// timestamps and block subsequent reads immediately.
	UpsertObject(ctx context.Context, o Object) (Object, error)
	GetObject(ctx context.Context, tenantID, id string) (Object, error)
	ListObjectsByConnection(ctx context.Context, tenantID, connectionID string) ([]Object, error)
	MarkObjectDeleted(ctx context.Context, tenantID, id string) error
	TombstoneObject(ctx context.Context, tenantID, id string) error

	// Versions are insert-only immutable observations. InsertObjectVersion
	// asserts ver.ObservedSeq is strictly greater than every existing seq
	// for the object (durable orchestration owns sequence allocation).
	InsertObjectVersion(ctx context.Context, tenantID string, ver ObjectVersion) error
	GetObjectVersion(ctx context.Context, tenantID, objectID string, observedSeq int64) (ObjectVersion, error)
	LatestObjectVersion(ctx context.Context, tenantID, objectID string) (ObjectVersion, error)

	// Source identities, groups and memberships (upsert by unique keys).
	InsertSourceIdentity(ctx context.Context, id SourceIdentity) error
	InsertSourceGroup(ctx context.Context, g SourceGroup) error
	InsertSourceMembership(ctx context.Context, m SourceGroupMembership) error
	// SourceIdentityForUser returns the connection-scoped source identity
	// mapped to an Enclava user, or ErrDenied-shaped not-found.
	SourceIdentityForUser(ctx context.Context, tenantID, connectionID, userID string) (SourceIdentity, error)
	// GroupIDsForIdentity lists groups the identity belonged to at the
	// latest complete membership revision.
	GroupIDsForIdentity(ctx context.Context, tenantID, connectionID, sourceIdentityID string) ([]string, error)

	// Grants and snapshots.
	GrantsForObject(ctx context.Context, tenantID, objectID string, accessRevision int64) ([]SourceGrant, error)
	BeginSnapshot(ctx context.Context, tenantID, connectionID, kind string, revision int64) error
	// LatestCompleteRevision returns the newest COMPLETE snapshot revision
	// for (connection, kind) with its completion time; not-found when none.
	LatestCompleteRevision(ctx context.Context, tenantID, connectionID, kind string) (revision int64, completedAt time.Time, err error)

	// Atomic snapshot application. Both take an open transaction so the
	// row writes, completion marking and unseen-row pruning commit or roll
	// back as one observation.
	ApplyAccessSnapshot(ctx context.Context, tx Tx, tenantID, connectionID string, revision int64, grants []SourceGrant) error
	ApplyMembershipSnapshot(ctx context.Context, tx Tx, tenantID, connectionID string, revision int64, memberships []SourceGroupMembership) error

	// Restrictions (server-owned, can only tighten).
	SetRestriction(ctx context.Context, r SourceRestriction) error
	RestrictionsForSubject(ctx context.Context, tenantID, subjectKind, subjectID string) ([]SourceRestriction, error)
}
