// types.go holds the bounded request/result contracts for the single
// authorized inference boundary (INF-02). A Request carries only
// client-selectable inputs: the operation capability, the approved model
// alias, the optional pinned endpoint, a bounded provider payload, immutable
// input references and a per-kind unit upper bound. Tenant, billing and
// credential fields exist on the principal and endpoint records only — a
// request can never supply or widen them.
package inference

import (
	"errors"
	"io"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/billing"
	"enclava-go/internal/source"
)

// Canonical operation capabilities. An endpoint must advertise the resolved
// capability in Endpoint.Capabilities to be an eligible destination; the same
// string keys the billing price set.
const (
	CapabilityChat       = "chat/completions"
	CapabilityEmbeddings = "embeddings"
	CapabilityVision     = "vision"
	CapabilityResponses  = "responses"
	CapabilityCompact    = "responses/compact"
)

// Capability prefix that binds an approved model alias to an upstream model
// id inside Endpoint.Capabilities: "model:<alias>" exposes the alias verbatim;
// "model:<alias>=<upstream-id>" exposes it translated to the given upstream
// id. An endpoint with no "model:" entries exposes no models — there is no
// implicit literal pass-through.
const ModelCapabilityPrefix = "model:"

// Request is the only client-facing input to Service.Execute and
// AuthorizeDestination. Every field is either a narrowing choice or a
// reference to server-owned state; none can widen persisted policy.
type Request struct {
	// Capability is the operation family; it selects eligible endpoints and
	// the price set. Unknown capabilities deny.
	Capability string

	// Model is the approved alias resolved against the candidate endpoint's
	// "model:" capability bindings, intersected with the actor's model
	// allowlist. It is never an arbitrary upstream model id.
	Model string

	// EndpointID optionally pins the request to one configured endpoint; the
	// endpoint still must pass capability, alias, allowlist and assurance
	// checks. It names a persisted endpoint id, never a URL.
	EndpointID string

	// Payload is the bounded provider request body (JSON object). Fields
	// carrying identity, credentials, URLs, upstream retention flags or
	// unverifiable hosted-resource ids are stripped or rejected by the
	// policy pass — see validatePayload.
	Payload []byte

	// Stream requests a streaming upstream response. The returned Result.Body
	// remains owned by the caller until Close; usage is settled once from the
	// terminal stream event or liability is retained.
	Stream bool

	// Method/Path select the Responses operation for CapabilityResponses and
	// CapabilityCompact requests (e.g. POST /responses). Other capabilities
	// have a fixed dispatch shape and ignore these fields.
	Method string
	Path   string

	// Inputs are immutable source evidence references consumed by this
	// execution. Each is resolved against tenant-owned storage and current
	// source authorization; ObservedSeq 0 means the object's latest version.
	Inputs []source.ObjectRef

	// Artifacts references stored byte artifacts the request consumes;
	// fields asserted by the caller are verified against the durable row.
	Artifacts []artifacts.Ref

	// Conversations names retained tenant conversations whose messages
	// contribute context. Ownership is verified against durable storage.
	Conversations []string

	// Restriction is an optional request-supplied narrowing of the effective
	// persisted policy. It may add denied capabilities or raise required
	// assurance, never widen.
	Restriction *source.SourceRestriction

	// Estimate is the caller-declared per-kind unit upper bound for the
	// attempt. It must be non-empty, within the operation's configured bound,
	// and carry only non-negative units. CachedInputUnits stays disjoint from
	// InputUnits.
	Estimate billing.Usage

	// JobID/AttemptID link the reservation to durable work when the caller
	// executes inside a job; the executor mints an attempt id when empty.
	JobID     string
	AttemptID string
}

// Decision is the resolved route policy for one request/destination pair —
// the shared output of the input-resolution and endpoint-eligibility checks.
type Decision struct {
	// Capability is the effective billed/routed capability after payload
	// upgrade (chat with image inputs routes as vision).
	Capability string

	// UpstreamModel is the endpoint-bound upstream model id the dispatched
	// body must carry.
	UpstreamModel string

	// Effective is the merged restriction set across every resolved input,
	// referenced artifact/conversation, tenant policy and request narrowing.
	Effective source.SourceRestriction
}

// Result is the owned outcome of one dispatched attempt. Body is owned by the
// caller until Close; it is never an unowned raw upstream resource — terminal
// usage settlement is bound to its lifetime for streams.
type Result struct {
	// AttemptID/ReservationID identify the attempt and its held reservation.
	AttemptID     string
	ReservationID string

	// Capability is the effective routed capability; EndpointID/Model the
	// endpoint record and upstream model id actually dispatched.
	Capability string
	EndpointID string
	Model      string

	// Body is the bounded response or stream. Callers must Close it exactly;
	// closing is idempotent. For non-streaming results usage is already
	// settled before Execute returns.
	Body io.ReadCloser

	// Usage is the settlement evidence for non-streaming results. Streaming
	// usage is retrievable after Body.Close via Billing.GetUsageRecord.
	Usage *billing.UsageRecord

	// SettlementError records a non-nil accounting error when dispatch
	// succeeded but settlement could not complete; the reservation is marked
	// unknown and liability retained in that case.
	SettlementError error
}

// ManageRequest is one unpriced lifecycle follow-up on a retained upstream
// resource (retrieve, cancel, delete, input_items, stream-resume). It never
// reserves or settles: management operations must stay available when the
// ordinary inference budget is exhausted, and they must never be retried or
// replayed onto another endpoint.
type ManageRequest struct {
	// EndpointID is the binding-resolved origin endpoint — the only
	// destination the follow-up may reach.
	EndpointID string

	// Method/Path/Query select the upstream operation under /responses/.
	// Path carries the bound upstream ID; Query is a pre-validated
	// server-built query string (already-resolved cursors only).
	Method string
	Path   string
	Query  string

	// Body is a bounded request body for POST-style management calls.
	Body []byte

	// Stream asks for the streaming variant (GET ?stream=true resumes).
	Stream bool
}

// ManageResult is the relayed outcome of one management call.
type ManageResult struct {
	// EndpointID is the endpoint record actually dispatched.
	EndpointID string
	// Body is the bounded upstream payload or stream; caller must Close.
	Body io.ReadCloser
}

var (
	// ErrDenied is the single public outcome for authorization failures —
	// unknown references, cross-tenant targets, actor/model/endpoint
	// allowlist misses, narrowed-out capabilities and forged request fields
	// are indistinguishable.
	ErrDenied = errors.New("inference: request denied")

	// ErrNoRoute means no eligible endpoint exists for the resolved
	// capability, model alias, actor allowlists and input assurance.
	ErrNoRoute = errors.New("inference: no eligible endpoint")

	// ErrUnsupportedCapability means the request's capability or a nested
	// tool/resource shape has no enforceable bounded rule.
	ErrUnsupportedCapability = errors.New("inference: unsupported capability")

	// ErrUnbounded means the estimate is empty, negative or above the
	// operation's configured upper bound — unpriced or unbounded attempts
	// are never dispatched.
	ErrUnbounded = errors.New("inference: unbounded or missing unit estimate")
)

// assuranceSatisfied reports whether a persisted endpoint meets the merged
// required assurance level. Unknown persisted levels rank above everything
// (fail-closed, same ordering as source.assuranceRank); an endpoint satisfies
// "high" or stronger only with an actual recorded attestation timestamp —
// a purported assurance_mode label is never proof.
func assuranceSatisfied(e Endpoint, required string) bool {
	switch required {
	case "":
		return true
	case "standard":
		return true
	default:
		// "high" or any unrecognized level requires observed verification.
		return e.AssuranceVerifiedAt != nil
	}
}

// upstreamModelFor resolves a public model alias against the endpoint's
// approved "model:" capability bindings. No bindings means no models.
func upstreamModelFor(e Endpoint, alias string) (string, bool) {
	for _, cap := range e.Capabilities {
		binding, ok := strings.CutPrefix(cap, ModelCapabilityPrefix)
		if !ok {
			continue
		}
		if a, upstream, found := strings.Cut(binding, "="); found {
			if a == alias && upstream != "" {
				return upstream, true
			}
			continue
		}
		if binding == alias {
			return alias, true
		}
	}
	return "", false
}

// capabilityAdvertised reports whether the endpoint advertises a capability.
func capabilityAdvertised(e Endpoint, capability string) bool {
	for _, c := range e.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// operationBounds are the server-owned upper bounds per capability. An
// estimate above any named bound or naming an empty estimate refuses
// dispatch — text, embeddings, vision and Responses/compact are all bounded
// and priced; nothing defaults to zero-cost.
var operationBounds = map[string]billing.Usage{
	CapabilityChat: {
		InputUnits:       1 << 22,
		OutputUnits:      1 << 17,
		CachedInputUnits: 1 << 22,
	},
	CapabilityEmbeddings: {
		InputUnits:       1 << 22,
		CachedInputUnits: 1 << 22,
	},
	CapabilityVision: {
		InputUnits:       1 << 22,
		OutputUnits:      1 << 17,
		CachedInputUnits: 1 << 22,
	},
	CapabilityResponses: {
		InputUnits:       1 << 22,
		OutputUnits:      1 << 17,
		CachedInputUnits: 1 << 22,
	},
	CapabilityCompact: {
		InputUnits:       1 << 22,
		OutputUnits:      1 << 17,
		CachedInputUnits: 1 << 22,
	},
}

// boundFor returns the operation bound for a capability.
func boundFor(capability string) (billing.Usage, bool) {
	b, ok := operationBounds[capability]
	return b, ok
}

// checkEstimate validates the caller bound: non-empty, non-negative and under
// the operation bound.
func checkEstimate(capability string, est billing.Usage) error {
	bound, ok := boundFor(capability)
	if !ok {
		return ErrUnsupportedCapability
	}
	pairs := [][2]int64{
		{est.InputUnits, bound.InputUnits},
		{est.OutputUnits, bound.OutputUnits},
		{est.CachedInputUnits, bound.CachedInputUnits},
	}
	for _, p := range pairs {
		if p[0] < 0 || p[0] > p[1] {
			return ErrUnbounded
		}
	}
	for k, v := range est.OtherUnits {
		if k == "" || v < 0 || v > 1<<22 {
			return ErrUnbounded
		}
	}
	if est.InputUnits == 0 && est.OutputUnits == 0 && est.CachedInputUnits == 0 && len(est.OtherUnits) == 0 {
		return ErrUnbounded
	}
	if est.InputUnits == 0 && est.CachedInputUnits == 0 && len(est.OtherUnits) == 0 && est.OutputUnits > 0 {
		// Generative attempts always consume input; a pure-output estimate is
		// an invalid bound.
		return ErrUnbounded
	}
	return nil
}

// accountingTimeout bounds settlement/mark operations so a wedged store can
// never pin a closed stream forever.
const accountingTimeout = 15 * time.Second
