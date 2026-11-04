// progress.go — the canonical per-source indexing-progress aggregate. A pure
// type: the store computes it, connectors projects it, the dashboard renders
// it.
package source

// IndexProgress is the indexing rollup for one connection's live objects
// inside its target collection. Status buckets mirror the document
// projection exactly (a failed embed/ingest job or a failed generation wins
// over every other state, published counts only when nothing failed,
// everything else is processing).
type IndexProgress struct {
	Items         int64
	Indexed       int64
	Failed        int64
	Processing    int64
	FailureReason string // most common failed-job message; "" when none
	FailureCount  int64  // items carrying FailureReason
}
