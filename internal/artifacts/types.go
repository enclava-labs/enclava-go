// Package artifacts holds the canonical opaque reference to durable stored
// bytes. A Ref is an opaque server-generated identifier — never a filesystem
// path, and a database reference is not permission to expose one.
package artifacts

import "time"

// Artifact kinds.
const (
	KindOriginal = "original"
	KindResult   = "result"
	KindDerived  = "derived"
)

// Ref is the opaque artifact reference carried by every DTO that points at
// stored bytes.
type Ref struct {
	TenantID string
	ID       string
	Kind     string // Kind* const set
	Version  int
	SHA256   string
	ByteSize int64
}

// Meta is the durable metadata row behind a Ref.
type Meta struct {
	Ref        Ref
	MimeType   string
	CreatedAt  time.Time
	Finalized  bool
	Tombstoned bool
}
