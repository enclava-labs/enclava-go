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

// Tx is the transaction handle connector observation writes run under —
// satisfied by *sql.Tx. The database/sql import is types-only and keeps the
// signature assignable without a wrapper.
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
	ObserveObject(ctx context.Context, tenantID, objectID, parentID string, upstream *string) error
	ListObjectsByConnection(ctx context.Context, tenantID, connectionID string) ([]Object, error)
	MarkObjectDeleted(ctx context.Context, tenantID, id string) error
	TombstoneObject(ctx context.Context, tenantID, id string) error

	// Versions are insert-only immutable observations. InsertObjectVersion
	// asserts ver.ObservedSeq is strictly greater than every existing seq
	// for the object (durable orchestration owns sequence allocation).
	InsertObjectVersion(ctx context.Context, tenantID string, ver ObjectVersion) error
	GetObjectVersion(ctx context.Context, tenantID, objectID string, observedSeq int64) (ObjectVersion, error)
	LatestObjectVersion(ctx context.Context, tenantID, objectID string) (ObjectVersion, error)
	// IngestHandoffExists reports whether the observed object version durably
	// reached ingest for the target collection: true when a live jobs row
	// (queued/running/succeeded — failed/cancelled/deleted count as missing
	// handoff) exists for (tenant, connection, object, observed_seq) carrying
	// this collection in its policy, or when an index generation exists for
	// that object version in the target collection. Lets a connector
	// re-enqueue work stranded by the crash window or a retarget.
	IngestHandoffExists(ctx context.Context, tenantID, connectionID, collectionID, objectID string, observedSeq int64) (bool, error)
	// IngestFailed reports whether the newest index generation of the object
	// version in the collection failed (its state or its embed/ingest job), so
	// a later sync can observe the unchanged item again and retry indexing.
	IngestFailed(ctx context.Context, tenantID, collectionID, objectID string, observedSeq int64) (bool, error)

	// CollectionsForObjects returns, per object ID, the live (removed_at IS
	// NULL) collections containing it — the read set AuthorizationCollection
	// evaluates. Objects in no collection are absent from the map.
	CollectionsForObjects(ctx context.Context, tenantID string, objectIDs []string) (map[string][]CollectionAccess, error)
	// Restrictions (server-owned, can only tighten).
	SetRestriction(ctx context.Context, r SourceRestriction) error
	RestrictionsForSubject(ctx context.Context, tenantID, subjectKind, subjectID string) ([]SourceRestriction, error)
}
