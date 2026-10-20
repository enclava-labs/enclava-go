// service.go is the single authorized priced execution boundary (INF-02,
// JOB-03/JOB-04). Execute resolves policy, picks a deterministic eligible
// endpoint, reserves the immutable rate snapshot before dispatch, marks
// dispatch durably before any network write, then settles exact usage once —
// or retains unknown liability — exactly once. No caller path bypasses it.
package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"enclava-go/internal/auth"
	"enclava-go/internal/billing"
	"enclava-go/internal/llm"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
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
	admission      *Admission

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

// WithAdmission overrides the process-local concurrency gate. Tests pass a
// small AdmissionConfig to observe explicit bounds; nil uses the defaults.
func WithAdmission(a *Admission) Option {
	return func(s *Service) {
		if a != nil {
			s.admission = a
		}
	}
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
		admission:      NewAdmission(AdmissionConfig{}),
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

func (s *Service) Close() error {
	s.mu.Lock()
	entries := s.transports
	s.transports = make(map[string]transportEntry)
	s.mu.Unlock()
	var errs []error
	for _, ent := range entries {
		if closer, ok := ent.up.(interface{ Close() error }); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
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

	// Admit before reserving: queued attempts never hold a budget hold.
	// Job-originated (internal fan-out) work draws from its own pool so a
	// nested or background model call can never deadlock an outer permit
	// held by the request pool.
	class := AdmissionExecute
	if req.JobID != "" {
		class = AdmissionFanOut
	}
	permit, err := s.admission.Acquire(ctx, class, p.TenantID)
	if err != nil {
		return nil, err
	}

	// AttemptID binds the reservation to a durable job_attempts row when the
	// caller runs inside a job; standalone calls leave it empty and the
	// reservation id is the attempt identity.
	res, err := s.reserve(ctx, p, req, endpoint, capability, upstreamModel, req.AttemptID)
	if err != nil {
		permit.Release()
		return nil, err
	}

	// Everything before MarkDispatched is provable never-dispatched; the
	// reservation releases. After it, outcomes may be accepted upstream and
	// liability is settled or retained — never released or retried.
	up, err := s.transportFor(ctx, endpoint)
	if err != nil {
		permit.Release()
		s.releaseUndispatched(ctx, p, res.ID)
		return nil, fmt.Errorf("transport for endpoint: %w", err)
	}
	if err := s.billing.MarkDispatched(ctx, p, res.ID); err != nil {
		permit.Release()
		return nil, fmt.Errorf("mark dispatched: %w", err)
	}
	dctx, cancel := context.WithTimeout(ctx, s.attemptTimeout)
	defer cancel()

	if req.Stream {
		// Streams bound their own lifetime via idle/wall limits on the body;
		// the attempt timeout would kill valid held streams on return.
		raw, err := s.dispatchStream(ctx, up, capability, req, body)
		if err != nil {
			permit.Release()
			s.failAttempt(ctx, p, res, err)
			return nil, err
		}
		// The permit stays held until the stream body actually closes —
		// headers returning early never frees the active slot.
		stream := newAccountingStream(raw, func(usageRaw []byte, terminal bool) {
			defer permit.Release()
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
		permit.Release()
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
	permit.Release()
	if endpoint.Protocol == "confidential_sdk" {
		s.stampVerification(ctx, endpoint)
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

// Manage dispatches one unpriced lifecycle follow-up — retrieve, cancel,
// delete, input_items, stream-resume — on the binding-resolved endpoint.
// There is no reservation and no model work, so it stays available under an
// exhausted inference budget; it also never retries: one attempt on the
// bound endpoint, whatever the outcome.
func (s *Service) Manage(ctx context.Context, p auth.Principal, req ManageRequest) (*ManageResult, error) {
	if p.TenantID == "" || p.ActorID == "" {
		return nil, fmt.Errorf("%w: principal lacks tenant/actor", ErrDenied)
	}
	if err := s.validateManageTarget(req); err != nil {
		return nil, err
	}
	e, err := s.endpoints.Get(ctx, p.TenantID, req.EndpointID)
	if err != nil || e.TenantID != p.TenantID {
		return nil, fmt.Errorf("%w", ErrNoRoute)
	}
	if !auth.EndpointAllowed(p, e.ID) {
		return nil, fmt.Errorf("%w", ErrDenied)
	}
	permit, err := s.admission.Acquire(ctx, AdmissionManage, p.TenantID)
	if err != nil {
		return nil, err
	}
	up, err := s.transportFor(ctx, e)
	if err != nil {
		permit.Release()
		return nil, fmt.Errorf("transport for endpoint: %w", err)
	}
	path := req.Path
	if req.Query != "" {
		path += "?" + req.Query
	}
	mctx, cancel := context.WithTimeout(ctx, s.attemptTimeout)
	if req.Stream {
		raw, err := up.DoResponsesStream(mctx, req.Method, path, req.Body)
		if err != nil {
			cancel()
			permit.Release()
			return nil, err
		}
		return &ManageResult{EndpointID: e.ID, Body: &cancelOnClose{ReadCloser: raw, done: func() { cancel(); permit.Release() }}}, nil
	}
	defer cancel()
	defer permit.Release()
	raw, err := up.DoResponsesRequest(mctx, req.Method, path, req.Body)
	if err != nil {
		return nil, err
	}
	return &ManageResult{EndpointID: e.ID, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}

// cancelOnClose releases the attempt deadline when the stream's owner closes.
type cancelOnClose struct {
	io.ReadCloser
	done context.CancelFunc
	once sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.done)
	return err
}

// validateManageTarget bounds a management request to the /responses/{id}
// operation surface: GET/POST/DELETE on a two-segment tail plus an optional
// server-built query of plain key=value pairs.
func (s *Service) validateManageTarget(req ManageRequest) error {
	switch strings.ToUpper(req.Method) {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		return fmt.Errorf("%w: responses method", ErrDenied)
	}
	if s.payloadLimit > 0 && int64(len(req.Body)) > s.payloadLimit {
		return fmt.Errorf("%w: payload exceeds bound", ErrDenied)
	}
	p := req.Path
	if !strings.HasPrefix(p, "/responses/") || strings.Contains(p, "..") ||
		strings.ContainsAny(p, "?#") ||
		!validPathTail(strings.TrimPrefix(p, "/responses")) {
		return fmt.Errorf("%w: responses path", ErrDenied)
	}
	if len(req.Query) > 512 || strings.Contains(req.Query, " ") {
		return fmt.Errorf("%w: responses query", ErrDenied)
	}
	for _, r := range req.Query {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '=' && r != '&' &&
			r != '_' && r != '-' && r != '.' && r != '%' {
			return fmt.Errorf("%w: responses query", ErrDenied)
		}
	}
	return nil
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

func (s *Service) dispatch(ctx context.Context, up Upstream, capability string, req Request, body []byte) ([]byte, error) {
	if unsupported, ok := up.(interface{ Supports(string) bool }); ok && !unsupported.Supports(capability) {
		return nil, ErrUnsupportedCapability
	}
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
	if unsupported, ok := up.(interface{ Supports(string) bool }); ok && !unsupported.Supports(capability) {
		return nil, ErrUnsupportedCapability
	}
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
	if e.Protocol == "confidential_sdk" {
		path := os.Getenv("CONFIDENTIAL_SDK_CONFIG")
		config, err := os.ReadFile(path)
		if path != "" && err != nil {
			return nil, fmt.Errorf("read confidential SDK config: %w", err)
		}
		sig += "|" + fmt.Sprintf("%x", sha256.Sum256(config))
	}
	credential, err := s.resolveCredential(e)
	if err != nil {
		return nil, err
	}
	sig += "|" + fmt.Sprintf("%x", sha256.Sum256([]byte(credential)))
	key := e.TenantID + "\x00" + e.ID
	s.mu.Lock()
	ent, ok := s.transports[key]
	s.mu.Unlock()
	if ok && ent.signature == sig {
		return ent.up, nil
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
	old, replaced := s.transports[key]
	s.transports[key] = transportEntry{signature: sig, up: up}
	s.mu.Unlock()
	if replaced {
		if closer, ok := old.up.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
	return up, nil
}

// transportSignature fingerprints the fields a transport is bound to; any
// change rebuilds the client rather than inheriting stale assurance,
// credentials or origin.
func transportSignature(e Endpoint) string {
	parts := []string{
		e.Protocol, e.SDKProvider, e.BaseOrigin, e.CredentialRef,
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

// CredentialReferences returns safe operator-configured reference IDs for a tenant.
func (s *Service) CredentialReferences(tenantID string) []auth.SecretRefMeta {
	if s == nil || s.secrets == nil {
		return nil
	}
	refs := s.secrets.Metadata(tenantID)
	out := refs[:0]
	for _, ref := range refs {
		if ref.Purpose == auth.SecretPurposeInference {
			out = append(out, ref)
		}
	}
	return out
}

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
	case "confidential_sdk":
		return llm.NewConfidentialSDK(os.Getenv("CONFIDENTIAL_SDK_CONFIG"), e.SDKProvider, e.BaseOrigin, credential)
	default:
		return nil, fmt.Errorf("%w: protocol %q", ErrUnsupportedCapability, e.Protocol)
	}
}
