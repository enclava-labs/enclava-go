// service.go is the single authorized priced execution boundary (INF-02,
// JOB-03/JOB-04). Execute resolves policy, picks a deterministic eligible
// endpoint, reserves the immutable rate snapshot before dispatch, marks
// dispatch durably before any network write, then settles exact usage once —
// or retains unknown liability — exactly once. No caller path bypasses it.
package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/llm"
)

// Billing is the reservation/settlement seam consumed by the executor;
// *billing.Service satisfies it.
type Billing interface {
	Reserve(ctx context.Context, p auth.Principal, req billing.ReserveRequest) (billing.Reservation, error)
	Settle(ctx context.Context, p auth.Principal, reservationID string, actual billing.Usage) (billing.UsageRecord, error)
	MarkUnknown(ctx context.Context, p auth.Principal, reservationID string) error
	MarkDispatched(ctx context.Context, p auth.Principal, reservationID string) error
	ReleaseUndispatched(ctx context.Context, p auth.Principal, reservationID string) error
}

// Upstream is the transport surface the executor dispatches on — the union
// the existing OpenAI-compatible adapter and attested Tinfoil provider share.
type Upstream interface {
	CreateChatCompletion(ctx context.Context, body []byte) ([]byte, error)
	CreateChatCompletionStream(ctx context.Context, body []byte) (io.ReadCloser, error)
	CreateEmbeddings(ctx context.Context, body []byte) ([]byte, error)
	DoResponsesRequest(ctx context.Context, method, path string, body []byte) ([]byte, error)
	DoResponsesStream(ctx context.Context, method, path string, body []byte) (io.ReadCloser, error)
}

// Deps are the authoritative seams the executor composes. Secrets may be nil
// — endpoints without a credential_ref resolve to no credential and any
// configured ref denies instead of leaking an ambient default.
type Deps struct {
	Endpoints     EndpointRepository
	Sources       SourceStore
	Authorizer    ObjectAuthorizer
	Billing       Billing
	Secrets       *auth.SecretResolver
	Artifacts     ArtifactStore
	Conversations ConversationStore
}

// Service is the one inference executor.
type Service struct {
	endpoints     EndpointRepository
	sources       SourceStore
	authorizer    ObjectAuthorizer
	artifacts     ArtifactStore
	conversations ConversationStore
	billing       Billing
	secrets       *auth.SecretResolver

	newTransport   func(ctx context.Context, e Endpoint, credential string) (Upstream, error)
	payloadLimit   int64
	attemptTimeout time.Duration

	mu         sync.Mutex
	transports map[string]transportEntry
}

type transportEntry struct {
	signature string
	up        Upstream
}

// Option configures a Service.
type Option func(*Service)

// WithTransportFactory overrides endpoint transport construction — tests use
// it to exercise lifecycle without real upstreams. The default maps the two
// existing transport kinds: OpenAI-compatible adapter and attested Tinfoil.
func WithTransportFactory(f func(ctx context.Context, e Endpoint, credential string) (Upstream, error)) Option {
	return func(s *Service) { s.newTransport = f }
}

// WithPayloadLimit overrides the bounded provider-body byte limit.
func WithPayloadLimit(n int64) Option {
	return func(s *Service) { s.payloadLimit = n }
}

// WithAttemptTimeout overrides the per-attempt dispatch deadline.
func WithAttemptTimeout(d time.Duration) Option {
	return func(s *Service) { s.attemptTimeout = d }
}

// NewService composes the executor over the authoritative seams.
func NewService(deps Deps, opts ...Option) (*Service, error) {
	if deps.Endpoints == nil || deps.Sources == nil || deps.Authorizer == nil || deps.Billing == nil {
		return nil, errors.New("inference: endpoints, sources, authorizer and billing are required")
	}
	s := &Service{
		endpoints:      deps.Endpoints,
		sources:        deps.Sources,
		authorizer:     deps.Authorizer,
		artifacts:      deps.Artifacts,
		conversations:  deps.Conversations,
		billing:        deps.Billing,
		secrets:        deps.Secrets,
		payloadLimit:   maxPayloadBytes,
		attemptTimeout: defaultAttemptTimeout,
		transports:     map[string]transportEntry{},
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.newTransport == nil {
		s.newTransport = s.defaultTransport
	}
	return s, nil
}

const (
	maxPayloadBytes       int64 = 8 * 1024 * 1024
	defaultAttemptTimeout       = 5 * time.Minute
)

// Execute is the single entry point every model call must use: resolve policy,
// route, reserve, dispatch, settle-or-retain. The principal's authority is
// never widened by request fields; the returned body is caller-owned.
func (s *Service) Execute(ctx context.Context, p auth.Principal, req Request) (*Result, error) {
	if p.TenantID == "" || p.ActorID == "" {
		return nil, fmt.Errorf("%w: principal lacks tenant/actor", ErrDenied)
	}
	if _, ok := boundFor(req.Capability); !ok {
		return nil, ErrUnsupportedCapability
	}
	info, err := s.analyzePayload(req)
	if err != nil {
		return nil, err
	}
	capability, err := effectiveCapability(req.Capability, info)
	if err != nil {
		return nil, err
	}
	if req.Stream && capability == CapabilityEmbeddings {
		return nil, ErrUnsupportedCapability
	}
	if err := checkEstimate(capability, req.Estimate); err != nil {
		return nil, err
	}

	endpoint, err := s.route(ctx, p, req, capability)
	if err != nil {
		return nil, err
	}
	decision, err := s.evaluate(ctx, p, req, endpoint, capability)
	if err != nil {
		return nil, err
	}
	upstreamModel := decision.UpstreamModel
	body, err := marshalPayload(info.root, upstreamModel, req)
	if err != nil {
		return nil, err
	}

	// AttemptID binds the reservation to a durable job_attempts row when the
	// caller runs inside a job; standalone calls leave it empty and the
	// reservation id is the attempt identity.
	res, err := s.reserve(ctx, p, req, endpoint, capability, upstreamModel, req.AttemptID)
	if err != nil {
		return nil, err
	}

	// Everything before MarkDispatched is provable never-dispatched; the
	// reservation releases. After it, outcomes may be accepted upstream and
	// liability is settled or retained — never released or retried.
	up, err := s.transportFor(ctx, endpoint)
	if err != nil {
		s.releaseUndispatched(ctx, p, res.ID)
		return nil, fmt.Errorf("transport for endpoint: %w", err)
	}
	if err := s.billing.MarkDispatched(ctx, p, res.ID); err != nil {
		return nil, fmt.Errorf("mark dispatched: %w", err)
	}

	dctx, cancel := context.WithTimeout(ctx, s.attemptTimeout)
	defer cancel()

	if req.Stream {
		raw, err := s.dispatchStream(dctx, up, capability, req, body)
		if err != nil {
			s.failAttempt(ctx, p, res, err)
			return nil, err
		}
		stream := newAccountingStream(raw, func(usageRaw []byte, terminal bool) {
			usage, known := usageFromJSON(usageRaw)
			if !terminal {
				known = false
			}
			if _, err := s.settleReservation(ctx, p, res, usage, known); err != nil {
				// Accounting failure retains liability by construction.
				_ = err
			}
		})
		return &Result{
			AttemptID:     req.AttemptID,
			ReservationID: res.ID,
			Capability:    capability,
			EndpointID:    endpoint.ID,
			Model:         upstreamModel,
			Body:          stream,
		}, nil
	}

	raw, err := s.dispatch(dctx, up, capability, req, body)
	if err != nil {
		if isUpstreamError(err) {
			// A definitive upstream error response settles its (possibly
			// zero) reported usage honestly.
			var ue *llm.UpstreamError
			var usage billing.Usage
			if errors.As(err, &ue) && len(ue.Body) > 0 {
				if u, ok := usageFromJSON(ue.Body); ok {
					usage = u
				}
			}
			rec, serr := s.settleReservation(ctx, p, res, usage, true)
			_ = rec
			if serr != nil {
				return nil, fmt.Errorf("upstream error %w; settlement also failed: %v", err, serr)
			}
		} else {
			s.failAttempt(ctx, p, res, err)
		}
		return nil, err
	}
	usage, known := usageFromJSON(raw)
	rec, serr := s.settleReservation(ctx, p, res, usage, known)
	result := &Result{
		AttemptID:     req.AttemptID,
		ReservationID: res.ID,
		Capability:    capability,
		EndpointID:    endpoint.ID,
		Model:         upstreamModel,
		Body:          io.NopCloser(bytes.NewReader(raw)),
		Usage:         rec,
	}
	if serr != nil {
		result.SettlementError = serr
	}
	return result, nil
}

// failAttempt retains liability for a dispatched attempt whose outcome is
// ambiguous: the request may have been accepted before the failure.
func (s *Service) failAttempt(ctx context.Context, p auth.Principal, res billing.Reservation, cause error) {
	if err := accounting(ctx, accountingTimeout, func(c context.Context) error {
		return s.billing.MarkUnknown(c, p, res.ID)
	}); err != nil {
		// Held state also retains liability; log nothing — the reservation
		// row is the evidence either way.
		_ = cause
	}
}

// releaseUndispatched frees a hold only while it is provably never
// dispatched. Failures here leave the hold — also safe.
func (s *Service) releaseUndispatched(ctx context.Context, p auth.Principal, reservationID string) {
	_ = accounting(ctx, accountingTimeout, func(c context.Context) error {
		return s.billing.ReleaseUndispatched(c, p, reservationID)
	})
}

// reserve takes the atomic priced hold: endpoint-priced first, falling back
// to the model's price set only when the endpoint prices nothing for the
// capability. Every required unit needs a rate — no implicit free units.
func (s *Service) reserve(ctx context.Context, p auth.Principal, req Request, endpoint Endpoint, capability, upstreamModel, attemptID string) (billing.Reservation, error) {
	base := billing.ReserveRequest{
		SubjectKeys: []billing.Subject{{Type: billing.BudgetSubjectTenant, ID: p.TenantID}},
		Capability:  capability,
		Model:       upstreamModel,
		EndpointID:  endpoint.ID,
		JobID:       req.JobID,
		AttemptID:   attemptID,
		Estimate:    req.Estimate,
		Price: billing.PriceKey{
			SubjectType: billing.PriceSubjectEndpoint,
			SubjectID:   endpoint.ID,
		},
	}
	res, err := s.billing.Reserve(ctx, p, base)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, billing.ErrPriceMissing) {
		return billing.Reservation{}, err
	}
	base.Price = billing.PriceKey{
		SubjectType: billing.PriceSubjectModel,
		SubjectID:   upstreamModel,
	}
	return s.billing.Reserve(ctx, p, base)
}

// dispatch sends the bounded body for non-streaming requests.
func (s *Service) dispatch(ctx context.Context, up Upstream, capability string, req Request, body []byte) ([]byte, error) {
	switch capability {
	case CapabilityChat, CapabilityVision:
		return up.CreateChatCompletion(ctx, body)
	case CapabilityEmbeddings:
		return up.CreateEmbeddings(ctx, body)
	case CapabilityResponses, CapabilityCompact:
		method, path := responsesTarget(capability, req)
		return up.DoResponsesRequest(ctx, method, path, body)
	default:
		return nil, ErrUnsupportedCapability
	}
}

// dispatchStream opens the streaming variant of the bounded request.
func (s *Service) dispatchStream(ctx context.Context, up Upstream, capability string, req Request, body []byte) (io.ReadCloser, error) {
	switch capability {
	case CapabilityChat, CapabilityVision:
		return up.CreateChatCompletionStream(ctx, body)
	case CapabilityResponses, CapabilityCompact:
		method, path := responsesTarget(capability, req)
		return up.DoResponsesStream(ctx, method, path, body)
	default:
		return nil, ErrUnsupportedCapability
	}
}

// responsesTarget resolves the bounded Responses operation path.
func responsesTarget(capability string, req Request) (string, string) {
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodPost
	}
	path := req.Path
	if path == "" {
		path = "/responses"
		if capability == CapabilityCompact {
			path = "/responses/compact"
		}
	}
	return method, path
}

// transportFor returns the cached per-endpoint transport, rebuilding only
// when the endpoint configuration signature changed — one client per
// approved endpoint revision, never per request.
func (s *Service) transportFor(ctx context.Context, e Endpoint) (Upstream, error) {
	sig := transportSignature(e)
	key := e.TenantID + "\x00" + e.ID
	s.mu.Lock()
	ent, ok := s.transports[key]
	s.mu.Unlock()
	if ok && ent.signature == sig {
		return ent.up, nil
	}

	credential, err := s.resolveCredential(e)
	if err != nil {
		return nil, err
	}
	up, err := s.newTransport(ctx, e, credential)
	if err != nil {
		return nil, err
	}
	// An endpoint whose protocol promises attestation must produce a
	// transport that can prove it — verification happens before dispatch and
	// the observed proof stamps the endpoint record. A transport without an
	// attestation path for such a protocol fails closed.
	if verifiable(e) {
		ap, ok := up.(interface {
			VerifyAttestation(context.Context) (llm.AttestationStatus, error)
		})
		if !ok {
			return nil, fmt.Errorf("%w: endpoint %q cannot attest", ErrUnsupportedCapability, e.ID)
		}
		if _, verr := ap.VerifyAttestation(ctx); verr != nil {
			return nil, fmt.Errorf("endpoint attestation: %w", verr)
		}
		s.stampVerification(ctx, e)
	}

	s.mu.Lock()
	s.transports[key] = transportEntry{signature: sig, up: up}
	s.mu.Unlock()
	return up, nil
}

// transportSignature fingerprints the fields a transport is bound to; any
// change rebuilds the client rather than inheriting stale assurance,
// credentials or origin.
func transportSignature(e Endpoint) string {
	parts := []string{
		e.Protocol, e.BaseOrigin, e.CredentialRef,
		fmt.Sprintf("%v", e.AuthorizedPrivate),
		e.AssuranceMode,
	}
	if e.AssuranceVerifiedAt != nil {
		parts = append(parts, e.AssuranceVerifiedAt.UTC().Format(time.RFC3339Nano))
	}
	caps := append([]string(nil), e.Capabilities...)
	sort.Strings(caps)
	parts = append(parts, strings.Join(caps, ","))
	return strings.Join(parts, "|")
}

// resolveCredential resolves the endpoint's credential_ref through the
// tenant/purpose/origin-bound resolver. An empty ref resolves to no
// credential; a configured ref that isn't bound denies.
func (s *Service) resolveCredential(e Endpoint) (string, error) {
	if e.CredentialRef == "" {
		return "", nil
	}
	if s.secrets == nil {
		return "", auth.ErrSecretDenied
	}
	return s.secrets.Resolve(e.TenantID, auth.SecretPurposeInference, e.BaseOrigin, e.CredentialRef)
}

// stampVerification best-effort persists observed attestation so the
// persisted assurance timestamp reflects the runtime proof.
func (s *Service) stampVerification(ctx context.Context, e Endpoint) {
	now := time.Now().UTC()
	if e.AssuranceVerifiedAt != nil && now.Sub(*e.AssuranceVerifiedAt) < time.Minute {
		return
	}
	e.AssuranceVerifiedAt = &now
	_ = s.endpoints.Update(ctx, e)
}

// defaultTransport builds the existing transport kinds only — there is no
// provider/plugin framework to extend.
func (s *Service) defaultTransport(ctx context.Context, e Endpoint, credential string) (Upstream, error) {
	switch e.Protocol {
	case "openai_compatible", "privatemode", "redpill":
		base := strings.TrimSuffix(e.BaseOrigin, "/") + "/v1"
		a := llm.NewOpenAICompatibleAdapter(base, credential, s.attemptTimeout)
		if e.AuthorizedPrivate {
			a.WithAllowPrivateEndpoint()
		}
		return a.WithProviderName(e.Protocol), nil
	case "tinfoil":
		host := strings.TrimPrefix(strings.TrimPrefix(e.BaseOrigin, "https://"), "http://")
		return llm.NewTinfoilProvider(credential, host, "", s.attemptTimeout, false, false, 5*time.Minute)
	default:
		return nil, fmt.Errorf("%w: protocol %q", ErrUnsupportedCapability, e.Protocol)
	}
}
