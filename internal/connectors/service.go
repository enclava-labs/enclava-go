package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

// Repository is the tenant-scoped persistence boundary for connector
// connections: source.Connection rows over the 003_source schema.
// Implemented by postgres.SourceRepo.
type Repository interface {
	CreateConnection(ctx context.Context, c source.Connection) error
	GetConnection(ctx context.Context, tenantID, id string) (source.Connection, error)
	ListConnections(ctx context.Context, tenantID string) ([]source.Connection, error)
	UpdateConnection(ctx context.Context, c source.Connection) error
	DeleteConnection(ctx context.Context, tenantID, id string) error
	SetSyncState(ctx context.Context, tenantID, id string, syncedAt time.Time, syncStatus, syncError string) error
	// ListSyncCandidates feeds the scheduler: active connections with a
	// target collection, oldest-synced first, across tenants.
	ListSyncCandidates(ctx context.Context, limit int) ([]source.Connection, error)
}

// JobReader is the owner-bound durable job view for SyncStatus/ListJobs.
type JobReader interface {
	GetOwnedJob(ctx context.Context, tenantID, id, submitterKey string) (jobs.JobRow, error)
	ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]jobs.JobRow, error)
	ListConnectionJobs(ctx context.Context, tenantID, connectionID, status string, limit int) ([]jobs.JobRow, error)
}

type CanonicalSourceRepository interface {
	source.SourceRepository
	WithTx(context.Context, func(source.Tx) error) error
	ObjectForNativeID(context.Context, string, string, string, string) (source.Object, error)
	ObjectForNativeIDCrossKind(context.Context, string, string, string) (source.Object, error)
	SourceIdentityForNativeID(context.Context, string, string, string) (source.SourceIdentity, error)
	SourceGroupForNativeID(context.Context, string, string, string) (source.SourceGroup, error)
	InsertSourceIdentity(context.Context, source.SourceIdentity) error
	ObserveSourceIdentity(context.Context, source.SourceIdentity) error
	InsertSourceGroup(context.Context, source.SourceGroup) error
	SourceIdentitiesForConnection(context.Context, string, string) ([]source.SourceIdentity, error)
	UserExists(context.Context, string, string) (bool, error)
	SourceIdentityByID(context.Context, string, string) (source.SourceIdentity, error)
	ObservedSourceIdentity(context.Context, string, string, string) (source.SourceIdentity, error)
}

type Service struct {
	repo                Repository
	jobs                *jobs.Service
	jobRows             JobReader
	adapters            AdapterFactories
	observations        ObservationRepository
	artifacts           *artifacts.Store
	secrets             *auth.SecretResolver
	principalForActor   func(context.Context, string, string) (auth.Principal, error)
	canonical           CanonicalSourceRepository
	httpClient          *http.Client
	allowPrivateBaseURL bool
}

func NewService(repo Repository, jobSvc *jobs.Service, jobRows JobReader) *Service {
	return &Service{
		repo:    repo,
		jobs:    jobSvc,
		jobRows: jobRows,
		httpClient: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: refuseConnectorRedirect,
		},
	}
}

func (s *Service) SetSyncDependencies(adapters AdapterFactories, observations ObservationRepository, artifactStore *artifacts.Store, canonical CanonicalSourceRepository) {
	s.adapters, s.observations, s.artifacts, s.canonical = adapters, observations, artifactStore, canonical
}
func (s *Service) SetCredentialResolver(resolver *auth.SecretResolver) {
	s.secrets = resolver
}

func (s *Service) SetPrincipalResolver(resolve func(context.Context, string, string) (auth.Principal, error)) {
	s.principalForActor = resolve
}

func (s *Service) CredentialRefs(p auth.Principal, kind string) []auth.SecretRefMeta {
	if s.secrets == nil || !auth.HasScope(p, "connectors.manage") {
		return nil
	}
	provider := credentialProviderForType(kind)
	refs := s.secrets.Metadata(p.TenantID)
	out := make([]auth.SecretRefMeta, 0, len(refs))
	for _, ref := range refs {
		if ref.Purpose == auth.SecretPurposeConnector && strings.HasPrefix(ref.RefID, "CONNECTOR_"+provider+"_") {
			out = append(out, ref)
		}
	}
	return out
}
func (s *Service) GetCapabilities(ctx context.Context, p auth.Principal, sourceID string) (AdapterCapabilities, error) {
	v, err := s.GetSource(ctx, p, sourceID)
	if err != nil {
		return AdapterCapabilities{}, err
	}
	factory := s.adapters[SourceKind(v.Type)]
	if factory == nil {
		return AdapterCapabilities{}, errors.New("connector adapter is not configured")
	}
	adapter, err := factory(viewConnection(v, p.TenantID, p.ActorID))
	if err != nil {
		return AdapterCapabilities{}, err
	}
	return adapter.Capabilities(), nil
}
func (s *Service) GetStreamState(ctx context.Context, p auth.Principal, sourceID, stream string) (StreamState, error) {
	if !auth.HasScope(p, "connectors.manage") {
		return StreamState{}, source.ErrDenied
	}
	if stream != string(StreamContent) && stream != string(StreamAccess) && stream != string(StreamMembership) {
		return StreamState{}, errors.New("invalid connector stream")
	}
	v, err := s.GetSource(ctx, p, sourceID)
	if err != nil {
		return StreamState{}, err
	}
	if s.observations == nil {
		return StreamState{}, errors.New("connector observations are not configured")
	}
	return s.observations.GetStream(ctx, p.TenantID, v.ID, stream)
}

func (s *Service) setAllowPrivateBaseURL(allow bool) {
	s.allowPrivateBaseURL = allow
}

// ---------------------------------------------------------------------
// View mapping: source.Connection ↔ wire-stable Source.

func connectionView(c source.Connection) Source {
	checkpoint, _ := c.Config["checkpoint"].(map[string]any)
	cfg := map[string]any{}
	for k, v := range c.Config {
		if k == "checkpoint" {
			continue
		}
		cfg[k] = v
	}
	return Source{
		ID:                 c.ID,
		Type:               c.Kind,
		Name:               c.Name,
		TargetCollectionID: c.TargetCollectionID,
		Config:             cfg,
		CredentialRef:      c.CredentialRef,
		Checkpoint:         checkpoint,
		IsEnabled:          c.Status == "active",
		CreatedAt:          c.CreatedAt,
		UpdatedAt:          c.UpdatedAt,
		LastSyncAt:         c.SyncedAt,
	}
}

func viewConnection(v Source, tenantID, actorID string) source.Connection {
	cfg := map[string]any{}
	for k, val := range v.Config {
		cfg[k] = val
	}
	if len(v.Checkpoint) > 0 {
		cfg["checkpoint"] = v.Checkpoint
	}
	status := "disabled"
	if v.IsEnabled {
		status = "active"
	}
	return source.Connection{
		TenantID:           tenantID,
		ID:                 v.ID,
		Kind:               v.Type,
		Name:               v.Name,
		CredentialRef:      v.CredentialRef,
		Config:             cfg,
		TargetCollectionID: v.TargetCollectionID,
		Status:             status,
		CreatedBy:          actorID,
		SyncedAt:           v.LastSyncAt,
	}
}

// ---------------------------------------------------------------------
// Source CRUD — tenant + principal scoped.

func requireTenant(p auth.Principal) error {
	if strings.TrimSpace(p.TenantID) == "" {
		return fmt.Errorf("%w: principal tenant is required", source.ErrDenied)
	}
	return nil
}

func (s *Service) CreateSource(ctx context.Context, p auth.Principal, input CreateSourceInput) (Source, error) {
	if err := requireTenant(p); err != nil {
		return Source{}, err
	}
	sourceType, err := normalizeSourceType(input.Type)
	if err != nil {
		return Source{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return Source{}, errors.New("name is required")
	}
	enabled := true
	if input.IsEnabled != nil {
		enabled = *input.IsEnabled
	}
	v := Source{
		ID:                 newID("connsrc"),
		Type:               sourceType,
		Name:               name,
		TargetCollectionID: strings.TrimSpace(input.TargetCollectionID),
		Config:             nonNilMap(input.Config),
		CredentialRef:      strings.TrimSpace(input.CredentialRef),
		Checkpoint:         map[string]any{},
		IsEnabled:          enabled,
	}
	if err := s.validateConnectorSource(ctx, v); err != nil {
		return Source{}, err
	}
	conn := viewConnection(v, p.TenantID, p.ActorID)
	if err := s.repo.CreateConnection(ctx, conn); err != nil {
		return Source{}, err
	}
	stored, err := s.repo.GetConnection(ctx, p.TenantID, conn.ID)
	if err != nil {
		return Source{}, err
	}
	return connectionView(stored), nil
}

func (s *Service) ListSources(ctx context.Context, p auth.Principal, sourceType string, limit, offset int) ([]Source, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	conns, err := s.repo.ListConnections(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	wanted := strings.TrimSpace(sourceType)
	out := []Source{}
	for _, c := range conns {
		if wanted != "" && c.Kind != wanted {
			continue
		}
		view := connectionView(c)
		if s.observations != nil {
			view.UnmappedPrincipals, err = s.observations.UnmappedPrincipals(ctx, p.TenantID, c.ID)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, view)
	}
	if offset > 0 {
		if offset >= len(out) {
			return out[:0], nil
		}
		out = out[offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Service) GetSource(ctx context.Context, p auth.Principal, id string) (Source, error) {
	if err := requireTenant(p); err != nil {
		return Source{}, err
	}
	c, err := s.repo.GetConnection(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Source{}, err
	}
	view := connectionView(c)
	if s.observations != nil {
		view.UnmappedPrincipals, err = s.observations.UnmappedPrincipals(ctx, p.TenantID, c.ID)
		if err != nil {
			return Source{}, err
		}
	}
	return view, nil
}

func (s *Service) UpdateSource(ctx context.Context, p auth.Principal, id string, input UpdateSourceInput) (Source, error) {
	if err := requireTenant(p); err != nil {
		return Source{}, err
	}
	conn, err := s.repo.GetConnection(ctx, p.TenantID, strings.TrimSpace(id))
	if err != nil {
		return Source{}, err
	}
	v := connectionView(conn)
	if input.Type != nil {
		sourceType, err := normalizeSourceType(*input.Type)
		if err != nil {
			return Source{}, err
		}
		v.Type = sourceType
	}
	if input.Name != nil {
		v.Name = strings.TrimSpace(*input.Name)
	}
	if v.Name == "" {
		return Source{}, errors.New("name is required")
	}
	if input.TargetCollectionID != nil {
		v.TargetCollectionID = strings.TrimSpace(*input.TargetCollectionID)
	}
	if input.Config != nil {
		v.Config = nonNilMap(input.Config)
	}
	if input.CredentialRef != nil {
		v.CredentialRef = strings.TrimSpace(*input.CredentialRef)
	}
	if input.Checkpoint != nil {
		v.Checkpoint = nonNilMap(input.Checkpoint)
	}
	if input.IsEnabled != nil {
		v.IsEnabled = *input.IsEnabled
	}
	if err := s.validateConnectorSource(ctx, v); err != nil {
		return Source{}, err
	}
	updated := viewConnection(v, p.TenantID, conn.CreatedBy)
	if err := s.repo.UpdateConnection(ctx, updated); err != nil {
		return Source{}, err
	}
	stored, err := s.repo.GetConnection(ctx, p.TenantID, conn.ID)
	if err != nil {
		return Source{}, err
	}
	return connectionView(stored), nil
}

func (s *Service) DeleteSource(ctx context.Context, p auth.Principal, id string) error {
	if err := requireTenant(p); err != nil {
		return err
	}
	return s.repo.DeleteConnection(ctx, p.TenantID, strings.TrimSpace(id))
}

// ---------------------------------------------------------------------
// Durable sync: SyncSource enqueues connector.sync and returns the queued
// job view — the job persists with NO dispatch until the executor registers
// the handler. Idempotency binds tenant + connection + config identity.

var ErrSourceDisabled = errors.New("connector source is disabled; enable it before syncing")
var ErrInvalidSource = errors.New("invalid connector source")

func (s *Service) SyncSource(ctx context.Context, p auth.Principal, sourceID string) (SyncJob, error) {
	return s.SyncSourceKeyed(ctx, p, sourceID, "")
}

// SyncSourceKeyed uses a caller-provided key to safely retry a manual request.
func (s *Service) SyncSourceKeyed(ctx context.Context, p auth.Principal, sourceID, key string) (SyncJob, error) {
	if strings.TrimSpace(key) == "" {
		key = newID("manual")
	}
	sum := sha256.Sum256([]byte(key))
	return s.syncSource(ctx, p, sourceID, "m"+hex.EncodeToString(sum[:]))
}

// SyncSourceWindowed enqueues a scheduled sync whose idempotency key carries
// the schedule window, so each interval produces one job instead of deduping
// into the previous (already succeeded) manual/scheduled job.
func (s *Service) SyncSourceWindowed(ctx context.Context, p auth.Principal, sourceID string, window int64) (SyncJob, error) {
	return s.syncSource(ctx, p, sourceID, "w"+strconv.FormatInt(window, 10))
}

func (s *Service) syncSource(ctx context.Context, p auth.Principal, sourceID string, window string) (SyncJob, error) {
	if err := requireTenant(p); err != nil {
		return SyncJob{}, err
	}
	conn, err := s.repo.GetConnection(ctx, p.TenantID, strings.TrimSpace(sourceID))
	if err != nil {
		return SyncJob{}, err
	}
	if conn.Status != "active" {
		return SyncJob{}, ErrSourceDisabled
	}
	if s.jobs == nil || s.observations == nil || s.artifacts == nil || s.canonical == nil || s.adapters == nil {
		return SyncJob{}, errors.New("connectors: durable sync is not configured")
	}
	if err := s.validateConnectorSource(ctx, connectionView(conn)); err != nil {
		return SyncJob{}, fmt.Errorf("%w: %v", ErrInvalidSource, err)
	}
	factory, ok := s.adapters[SourceKind(conn.Kind)]
	if !ok {
		return SyncJob{}, fmt.Errorf("connector kind %q is not configured", conn.Kind)
	}
	adapter, err := factory(conn)
	if err != nil {
		return SyncJob{}, err
	}
	capabilities := adapter.Capabilities()
	streams := []StreamKind{}
	if capabilities.Content {
		streams = append(streams, StreamContent)
	}
	if capabilities.Access {
		streams = append(streams, StreamAccess)
	}
	if capabilities.Membership {
		streams = append(streams, StreamMembership)
	}
	if len(streams) == 0 {
		return SyncJob{}, errors.New("connector adapter exposes no sync streams")
	}
	configHash, err := connectorConfigHash(conn)
	if err != nil {
		return SyncJob{}, err
	}
	key := "connector.sync:" + conn.ID + ":" + configHash + ":" + window
	var response jobs.Job
	for _, stream := range streams {
		job, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
			TenantID:       p.TenantID,
			Operation:      OperationSync,
			IdempotencyKey: key + ":" + string(stream),
			Subject:        jobs.Subject{ConnectionID: conn.ID},
			InputRef:       conn.ID,
			InputHash:      configHash,
			Policy:         map[string]any{"connection_id": conn.ID, "stream": string(stream)},
		})
		if err != nil {
			return SyncJob{}, err
		}
		if stream == StreamContent || response.ID == "" {
			response = job
		}
	}
	return jobView(response), nil
}

func connectorConfigHash(conn source.Connection) (string, error) {
	cfgBytes, err := json.Marshal(conn.Config)
	if err != nil {
		return "", fmt.Errorf("hash connection config: %w", err)
	}
	sum := sha256.Sum256(append(cfgBytes, []byte("\x00"+conn.CredentialRef)...))
	return hex.EncodeToString(sum[:]), nil
}

func jobView(j jobs.Job) SyncJob {
	var stats map[string]any
	if len(j.Policy) > 0 {
		stats = map[string]any{"policy": j.Policy}
	}
	return SyncJob{
		ID:                j.ID,
		ConnectorSourceID: j.Subject.ConnectionID,
		Status:            j.Status,
		StartedAt:         nil,
		CompletedAt:       j.CompletedAt,
		ErrorMessage:      j.Error,
		Stats:             stats,
		CreatedAt:         j.CreatedAt,
	}
}

// ListSyncJobs returns the caller's durable connector.sync jobs, newest
// first, optionally narrowed to one connection. Reads are owner-bound: other
// submitters' jobs are indistinguishable from absent.
func (s *Service) ListSyncJobs(ctx context.Context, p auth.Principal, sourceID, status string, limit, offset int) ([]SyncJob, error) {
	if err := requireTenant(p); err != nil {
		return nil, err
	}
	if s.jobRows == nil {
		return nil, errors.New("connectors: durable jobs are not configured")
	}
	if limit <= 0 {
		limit = 25
	}
	var rows []jobs.JobRow
	var err error
	if wanted := strings.TrimSpace(sourceID); wanted != "" {
		// Scheduled syncs run under the scheduler's principal; a source-scoped
		// listing must show them to the source's managers, not just the last
		// manual submitter.
		rows, err = s.jobRows.ListConnectionJobs(ctx, p.TenantID, wanted, status, limit+offset)
	} else {
		rows, err = s.jobRows.ListOwnedJobs(ctx, p.TenantID, jobs.SubmitterKey(p), limit*4+offset)
	}
	if err != nil {
		return nil, err
	}
	out := []SyncJob{}
	for _, row := range rows {
		if row.Operation != OperationSync {
			continue
		}
		if wanted := strings.TrimSpace(sourceID); wanted != "" && row.Subject.ConnectionID != wanted {
			continue
		}
		if wanted := strings.TrimSpace(status); wanted != "" && row.Status != wanted {
			continue
		}
		out = append(out, jobView(row.Job))
	}
	if offset > 0 {
		if offset >= len(out) {
			return out[:0], nil
		}
		out = out[offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Service) GetSyncJob(ctx context.Context, p auth.Principal, id string) (SyncJob, error) {
	if err := requireTenant(p); err != nil {
		return SyncJob{}, err
	}
	if s.jobRows == nil {
		return SyncJob{}, errors.New("connectors: durable jobs are not configured")
	}
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, strings.TrimSpace(id), jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationSync {
		return SyncJob{}, source.ErrDenied
	}
	return jobView(row.Job), nil
}

func (s *Service) CancelSyncJob(ctx context.Context, p auth.Principal, id string) error {
	if err := requireTenant(p); err != nil {
		return err
	}
	if s.jobRows == nil || s.jobs == nil {
		return errors.New("connectors: durable jobs are not configured")
	}
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, strings.TrimSpace(id), jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationSync {
		return source.ErrDenied
	}
	return s.jobs.Cancel(ctx, p, row.ID)
}

// JobHandler is the jobs.Runner handler for connector.sync. It runs the
// fetch/upload pipeline under the fenced lease using the frozen submitting
// principal — registered by the executor plan; not dispatched until then.
func (s *Service) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		return s.attemptSync(ctx, claim)
	}
}

func (s *Service) attemptSync(ctx context.Context, claim *jobs.Claim) (string, error) {
	job := claim.Job()
	var principal auth.Principal
	if err := json.Unmarshal(job.PrincipalSnapshot, &principal); err != nil {
		return "", fmt.Errorf("decode frozen principal: %w", err)
	}
	conn, err := s.repo.GetConnection(ctx, job.TenantID, job.Subject.ConnectionID)
	if err != nil {
		return "", err
	}
	if conn.Status != "active" {
		return "", errors.New("connector source is no longer active")
	}
	if s.principalForActor == nil {
		return "", errors.New("connector creator authorization resolver is not configured")
	}
	if strings.TrimSpace(conn.CreatedBy) == "" {
		return "", errors.New("connector source has no creator; assign an active user creator before syncing")
	}
	principal, err = s.principalForActor(ctx, conn.TenantID, conn.CreatedBy)
	if err != nil {
		return "", fmt.Errorf("resolve connector creator authorization: %w", err)
	}
	if principal.TenantID != conn.TenantID || principal.ActorID != conn.CreatedBy {
		return "", errors.New("connector creator authorization returned an invalid principal")
	}
	if s.observations == nil || s.artifacts == nil || s.canonical == nil || s.adapters == nil {
		return "", errors.New("connectors: durable sync is not configured")
	}
	stream := StreamKind(stringFromAny(job.Policy["stream"]))
	switch stream {
	case StreamContent, StreamAccess, StreamMembership:
	default:
		return "", errors.New("connector sync job has no valid stream")
	}
	factory, ok := s.adapters[SourceKind(conn.Kind)]
	if !ok {
		return "", fmt.Errorf("connector kind %q is not configured", conn.Kind)
	}
	adapter, err := factory(conn)
	if err != nil {
		return "", err
	}
	if err := s.validateConnectorSource(ctx, connectionView(conn)); err != nil {
		return "", err
	}
	capabilities := adapter.Capabilities()
	if stream == StreamContent && !capabilities.Content ||
		stream == StreamAccess && !capabilities.Access ||
		stream == StreamMembership && !capabilities.Membership {
		return "", fmt.Errorf("connector adapter does not support %s observations", stream)
	}
	if err := claim.MarkDispatched(ctx); err != nil {
		return "", err
	}
	if err := s.runDurableStream(ctx, claim, principal, conn, adapter, stream); err != nil {
		_ = s.repo.SetSyncState(ctx, conn.TenantID, conn.ID, time.Now().UTC(), JobStatusFailed, err.Error())
		return "", err
	}
	if stream == StreamContent {
		if err := s.repo.SetSyncState(ctx, conn.TenantID, conn.ID, time.Now().UTC(), JobStatusSucceeded, ""); err != nil {
			return "", err
		}
	}
	return `{"stream":"` + string(stream) + `","complete":true}`, nil
}

// ---------------------------------------------------------------------

type connectorDocument struct {
	FileName  string
	MimeType  string
	Content   string
	SourceID  string
	SourceURI string
	Revision  string
}

func (s *Service) documentsForSource(ctx context.Context, source Source) ([]connectorDocument, string, map[string]any, error) {
	if documents := documentsFromConfig(source.Config); len(documents) > 0 {
		return documents, "config", nil, nil
	}
	documents, checkpoint, err := s.fetchAPIDocuments(ctx, source)
	if err != nil {
		return nil, "api", checkpoint, err
	}
	return documents, "api", checkpoint, nil
}

func documentsFromConfig(config map[string]any) []connectorDocument {
	rawDocs, ok := config["documents"].([]any)
	if !ok {
		if content := strings.TrimSpace(stringFromAny(config["content"])); content != "" {
			return []connectorDocument{{
				FileName:  strings.TrimSpace(stringFromAny(config["file_name"])),
				MimeType:  strings.TrimSpace(stringFromAny(config["mime_type"])),
				Content:   content,
				SourceID:  strings.TrimSpace(stringFromAny(config["source_id"])),
				SourceURI: strings.TrimSpace(stringFromAny(config["source_uri"])),
			}}
		}
		return nil
	}
	out := []connectorDocument{}
	for _, raw := range rawDocs {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, connectorDocument{
			FileName:  strings.TrimSpace(stringFromAny(item["file_name"])),
			MimeType:  strings.TrimSpace(stringFromAny(item["mime_type"])),
			Content:   stringFromAny(item["content"]),
			SourceID:  strings.TrimSpace(stringFromAny(item["source_id"])),
			SourceURI: strings.TrimSpace(stringFromAny(item["source_uri"])),
		})
	}
	return out
}

func (s *Service) fetchAPIDocuments(ctx context.Context, source Source) ([]connectorDocument, map[string]any, error) {
	switch source.Type {
	case SourceTypeGitHub:
		docs, err := s.fetchGitHubDocuments(ctx, source)
		return docs, nil, err
	case SourceTypeSlack:
		return s.fetchSlackDocuments(ctx, source)
	case SourceTypeNotion:
		docs, err := s.fetchNotionDocuments(ctx, source)
		return docs, nil, err
	case SourceTypeTeams:
		docs, err := s.fetchTeamsDocuments(ctx, source)
		return docs, nil, err
	case SourceTypeGoogleDrive:
		docs, err := s.fetchGoogleDriveDocuments(ctx, source)
		return docs, nil, err
	default:
		return nil, nil, errors.New("unsupported connector source type")
	}
}

func (s *Service) fetchGitHubDocuments(ctx context.Context, source Source) ([]connectorDocument, error) {
	owner := strings.TrimSpace(stringFromAny(source.Config["owner"]))
	repo := strings.TrimSpace(stringFromAny(source.Config["repo"]))
	if owner == "" || repo == "" {
		return nil, errors.New("github config.owner and config.repo are required")
	}
	paths := stringListFromAny(source.Config["paths"])
	if len(paths) == 0 {
		if path := strings.TrimSpace(stringFromAny(source.Config["path"])); path != "" {
			paths = []string{path}
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("github config.paths must include at least one file path")
	}
	baseURL := connectorBaseURL(source, "https://api.github.com")
	docs := make([]connectorDocument, 0, len(paths))
	for _, filePath := range paths {
		filePath = strings.Trim(strings.TrimSpace(filePath), "/")
		if filePath == "" {
			continue
		}
		endpoint := strings.TrimRight(baseURL, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + escapePathSegments(filePath)
		req, err := s.newConnectorRequest(ctx, source, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			Type        string `json:"type"`
			Encoding    string `json:"encoding"`
			Content     string `json:"content"`
			HTMLURL     string `json:"html_url"`
			DownloadURL string `json:"download_url"`
		}
		if err := s.doJSON(req, &payload); err != nil {
			return nil, err
		}
		if payload.Type != "" && payload.Type != "file" {
			return nil, fmt.Errorf("github path %q is not a file", filePath)
		}
		content := strings.ReplaceAll(payload.Content, "\n", "")
		var decoded []byte
		if strings.EqualFold(payload.Encoding, "base64") {
			decoded, err = base64.StdEncoding.DecodeString(content)
			if err != nil {
				return nil, fmt.Errorf("decode github file %q: %w", filePath, err)
			}
		} else {
			decoded = []byte(payload.Content)
		}
		name := payload.Name
		if name == "" {
			name = filepath.Base(filePath)
		}
		docs = append(docs, connectorDocument{
			FileName:  name,
			MimeType:  mimeTypeForFile(name),
			Content:   string(decoded),
			SourceID:  "github:" + owner + "/" + repo + ":" + firstNonEmpty(payload.Path, filePath),
			SourceURI: firstNonEmpty(payload.HTMLURL, payload.DownloadURL),
		})
	}
	return docs, nil
}

func (s *Service) fetchSlackDocuments(ctx context.Context, source Source) ([]connectorDocument, map[string]any, error) {
	channelID := strings.TrimSpace(firstNonEmpty(stringFromAny(source.Config["channel_id"]), stringFromAny(source.Config["channel"])))
	if channelID == "" {
		return nil, nil, errors.New("slack config.channel_id is required")
	}
	limit := intFromAny(source.Config["limit"], 50)
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	baseURL := connectorBaseURL(source, "https://slack.com/api")
	values := url.Values{}
	values.Set("channel", channelID)
	values.Set("limit", strconv.Itoa(limit))
	if latestTS := strings.TrimSpace(stringFromAny(nonNilMap(source.Checkpoint)["slack_latest_ts"])); latestTS != "" {
		values.Set("oldest", latestTS)
		values.Set("inclusive", "false")
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/conversations.history?" + values.Encode()
	req, err := s.newConnectorRequest(ctx, source, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	var payload struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			User string `json:"user"`
			Text string `json:"text"`
			TS   string `json:"ts"`
		} `json:"messages"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, nil, err
	}
	if !payload.OK {
		return nil, nil, fmt.Errorf("slack conversations.history failed: %s", firstNonEmpty(payload.Error, "unknown_error"))
	}
	// One document per message: the retrieval unit is the individual message,
	// so an incremental sync adds new messages without replacing the indexed
	// history of earlier ones. Thread relationships are a follow-up.
	latestTS := strings.TrimSpace(stringFromAny(nonNilMap(source.Checkpoint)["slack_latest_ts"]))
	documents := make([]connectorDocument, 0, len(payload.Messages))
	for _, msg := range payload.Messages {
		latestTS = maxSlackTimestamp(latestTS, msg.TS)
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		documents = append(documents, connectorDocument{
			FileName:  "slack-" + channelID + "-" + msg.TS + ".txt",
			MimeType:  "text/plain",
			Content:   fmt.Sprintf("[%s %s] %s", msg.TS, msg.User, text),
			SourceID:  "slack:" + channelID + ":" + msg.TS,
			SourceURI: "slack://channel/" + channelID + "/" + msg.TS,
			Revision:  msg.TS,
		})
	}
	checkpoint := map[string]any{}
	if latestTS != "" {
		checkpoint["slack_latest_ts"] = latestTS
	}
	if len(documents) == 0 {
		return nil, checkpoint, nil
	}
	return documents, checkpoint, nil
}

func (s *Service) fetchNotionDocuments(ctx context.Context, source Source) ([]connectorDocument, error) {
	pageID := strings.TrimSpace(firstNonEmpty(stringFromAny(source.Config["page_id"]), stringFromAny(source.Config["block_id"])))
	if pageID == "" {
		return nil, errors.New("notion config.page_id is required")
	}
	baseURL := connectorBaseURL(source, "https://api.notion.com/v1")
	values := url.Values{}
	values.Set("page_size", strconv.Itoa(intFromAny(source.Config["page_size"], 100)))
	endpoint := strings.TrimRight(baseURL, "/") + "/blocks/" + url.PathEscape(pageID) + "/children?" + values.Encode()
	req, err := s.newConnectorRequest(ctx, source, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []map[string]any `json:"results"`
	}
	if err := s.doJSON(req, &payload); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, block := range payload.Results {
		if text := notionBlockText(block); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return []connectorDocument{{
		FileName:  "notion-" + pageID + ".txt",
		MimeType:  "text/plain",
		Content:   strings.TrimSpace(b.String()),
		SourceID:  "notion:" + pageID,
		SourceURI: "notion://block/" + pageID,
	}}, nil
}

func (s *Service) newConnectorRequest(ctx context.Context, source Source, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("build connector request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token, err := connectorToken(source); err != nil {
		return nil, err
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if source.Type == SourceTypeNotion {
		req.Header.Set("Notion-Version", firstNonEmpty(stringFromAny(source.Config["notion_version"]), "2022-06-28"))
	}
	return req, nil
}

func (s *Service) doJSON(req *http.Request, out any) error {
	resp, err := s.connectorHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("connector request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if readErr != nil {
		return fmt.Errorf("read connector response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("connector upstream returned status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode connector response: %w", err)
	}
	return nil
}

func normalizeSourceType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case SourceTypeGitHub:
		return SourceTypeGitHub, nil
	case SourceTypeSlack:
		return SourceTypeSlack, nil
	case SourceTypeTeams:
		return SourceTypeTeams, nil
	case SourceTypeNotion:
		return SourceTypeNotion, nil
	case SourceTypeGoogleDrive:
		return SourceTypeGoogleDrive, nil
	default:
		return "", errors.New("type must be one of: github, slack, teams, notion, google_drive")
	}
}

func nonNilMap(v map[string]any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func stringListFromAny(v any) []string {
	switch raw := v.(type) {
	case []string:
		return raw
	case []any:
		out := []string{}
		for _, item := range raw {
			if value := strings.TrimSpace(stringFromAny(item)); value != "" {
				out = append(out, value)
			}
		}
		return out
	case string:
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		parts := strings.Split(raw, ",")
		out := []string{}
		for _, part := range parts {
			if value := strings.TrimSpace(part); value != "" {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}

func intFromAny(v any, fallback int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err == nil {
			return i
		}
	}
	return fallback
}

func maxSlackTimestamp(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	af, aErr := strconv.ParseFloat(a, 64)
	bf, bErr := strconv.ParseFloat(b, 64)
	if aErr == nil && bErr == nil {
		if bf > af {
			return b
		}
		return a
	}
	if b > a {
		return b
	}
	return a
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func connectorBaseURL(source Source, fallback string) string {
	return firstNonEmpty(stringFromAny(source.Config["base_url"]), fallback)
}

func connectorToken(source Source) (string, error) {
	if err := validateCredentialRef(source); err != nil {
		return "", err
	}
	ref := strings.TrimSpace(source.CredentialRef)
	if ref == "" {
		return "", nil
	}
	token := strings.TrimSpace(os.Getenv(ref))
	if token == "" && connectorRequiresCredential(source) {
		return "", fmt.Errorf("credential_ref %q environment variable is not set", ref)
	}
	return token, nil
}

func connectorRequiresCredential(source Source) bool {
	// Inline documents are stored locally and are not fetched with a bearer token.
	if len(documentsFromConfig(source.Config)) > 0 {
		return false
	}
	if strings.TrimSpace(stringFromAny(source.Config["base_url"])) != "" && strings.EqualFold(stringFromAny(source.Config["allow_unauthenticated"]), "true") {
		return false
	}
	switch source.Type {
	case SourceTypeSlack, SourceTypeNotion, SourceTypeTeams, SourceTypeGoogleDrive:
		return true
	default:
		return false
	}

}

var connectorCredentialRefPattern = regexp.MustCompile(`^CONNECTOR_(GITHUB|SLACK|TEAMS|NOTION|GOOGLE_DRIVE)_[A-Z0-9_]+$`)

func refuseConnectorRedirect(*http.Request, []*http.Request) error {
	return errors.New("connector redirects are not followed")
}

// requireAPIOrigin enforces https for connector API bases; plain http is
// allowed only for loopback hosts so hermetic fixtures keep working.
func requireAPIOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("connector API base URL is invalid")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			return u, nil
		}
		return nil, errors.New("connector API base URL must be https (http is only allowed on loopback)")
	}
	return nil, errors.New("connector API base URL must be http(s)")
}

func (s *Service) validateConnectorSource(ctx context.Context, source Source) error {
	if source.Type == SourceTypeGitHub {
		if strings.TrimSpace(stringFromAny(source.Config["owner"])) == "" {
			return errors.New("config.owner is required for a GitHub source")
		}
		if strings.TrimSpace(stringFromAny(source.Config["repo"])) == "" {
			return errors.New("config.repo is required for a GitHub source")
		}
	}
	if err := validateCredentialRef(source); err != nil {
		return err
	}
	return s.validateBaseURL(ctx, source)
}

func validateCredentialRef(source Source) error {
	ref := strings.TrimSpace(source.CredentialRef)
	if ref == "" {
		if connectorRequiresCredential(source) {
			return errors.New(source.Type + " credential_ref is required")
		}
		return nil
	}
	match := connectorCredentialRefPattern.FindStringSubmatch(ref)
	if match == nil || match[1] != credentialProviderForType(source.Type) {
		return fmt.Errorf("credential_ref must name a CONNECTOR_%s_* binding in SECRET_BINDINGS", credentialProviderForType(source.Type))
	}
	return nil
}

func credentialProviderForType(sourceType string) string {
	switch sourceType {
	case SourceTypeGitHub:
		return "GITHUB"
	case SourceTypeSlack:
		return "SLACK"
	case SourceTypeTeams:
		return "TEAMS"
	case SourceTypeNotion:
		return "NOTION"
	case SourceTypeGoogleDrive:
		return "GOOGLE_DRIVE"
	default:
		return ""
	}
}

func (s *Service) validateBaseURL(ctx context.Context, source Source) error {
	raw := strings.TrimSpace(stringFromAny(source.Config["base_url"]))
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "#") {
		return errors.New("config.base_url must not include a fragment")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("config.base_url must be an http or https URL")
	}
	if parsed.User != nil {
		return errors.New("config.base_url must not include userinfo")
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if host == "" {
		return errors.New("config.base_url host is required")
	}
	ips, err := connectorHostIPs(ctx, host)
	if err != nil {
		return fmt.Errorf("config.base_url must be a public http(s) origin: %w", err)
	}
	if s != nil && s.allowPrivateBaseURL {
		return nil
	}
	for _, ip := range ips {
		if disallowedConnectorIP(ip) {
			return errors.New("config.base_url must be a public http(s) origin")
		}
	}
	return nil
}

func connectorHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if len(addr.IP) == 0 {
			continue
		}
		ips = append(ips, addr.IP)
	}
	if len(ips) == 0 {
		return nil, errors.New("host did not resolve")
	}
	return ips, nil
}

func disallowedConnectorIP(ip net.IP) bool {
	return len(ip) == 0 || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (s *Service) connectorHTTPClient() *http.Client {
	if s != nil && s.httpClient != nil {
		return s.httpClient
	}
	return newConnectorHTTPClient()
}

// newConnectorHTTPClient enforces the public-origin policy at DIAL time:
// validation-time DNS answers can change before the request dials
// (rebinding), so the transport refuses non-public resolved addresses
// there. Loopback stays reachable for hermetic fixtures.
func newConnectorHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && disallowedConnectorIP(ip) {
			return fmt.Errorf("connector dial to non-public address %s refused", address)
		}
		return nil
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: refuseConnectorRedirect,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func escapePathSegments(raw string) string {
	parts := strings.Split(raw, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func mimeTypeForFile(name string) string {
	if mimeType := mime.TypeByExtension(filepath.Ext(name)); mimeType != "" {
		return mimeType
	}
	return "text/plain"
}

func notionBlockText(block map[string]any) string {
	blockType := strings.TrimSpace(stringFromAny(block["type"]))
	if blockType == "" {
		return ""
	}
	payload, _ := block[blockType].(map[string]any)
	if len(payload) == 0 {
		return strings.TrimSpace(stringFromAny(block["plain_text"]))
	}
	if title := strings.TrimSpace(stringFromAny(payload["title"])); title != "" {
		return title
	}
	richText, _ := payload["rich_text"].([]any)
	parts := []string{}
	for _, raw := range richText {
		item, _ := raw.(map[string]any)
		if text := strings.TrimSpace(stringFromAny(item["plain_text"])); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

type legacyAdapter struct {
	service *Service
	kind    SourceKind
}

func NewLegacyAdapter(service *Service, kind SourceKind) Adapter {
	return legacyAdapter{service: service, kind: kind}
}

func (a legacyAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Content: true, UnsupportedScopes: []string{"access", "membership"}}
}

func (a legacyAdapter) Content(ctx context.Context, connection source.Connection, cursor Cursor, limit int) (ContentPage, error) {
	view := connectionView(connection)
	if string(a.kind) != connection.Kind {
		return ContentPage{}, errors.New("connector adapter kind does not match connection")
	}
	if cursor != "" {
		return ContentPage{}, errors.New("legacy connector adapter does not support paging")
	}
	if limit <= 0 || limit > 100 {
		return ContentPage{}, errors.New("connector page limit must be between 1 and 100")
	}
	docs, _, _, err := a.service.documentsForSource(ctx, view)
	if err != nil {
		return ContentPage{}, err
	}
	if len(docs) > limit {
		return ContentPage{Completeness: CompletenessIncomplete, Snapshot: false}, errors.New("legacy adapter listing exceeds bounded page size")
	}
	page := ContentPage{Completeness: CompletenessComplete, Snapshot: true}
	for _, doc := range docs {
		nativeID := firstNonEmpty(doc.SourceID, doc.SourceURI, doc.FileName)
		if nativeID == "" {
			return ContentPage{}, errors.New("connector item has no native identity")
		}
		page.Items = append(page.Items, ContentItem{
			NativeID: nativeID, Kind: connection.Kind, NativeRevision: doc.Revision,
			Filename: doc.FileName, MIMEType: doc.MimeType, Content: []byte(doc.Content), URL: doc.SourceURI,
		})
	}
	return page, nil
}

func (legacyAdapter) Access(context.Context, source.Connection, Cursor, int) (AccessPage, error) {
	return AccessPage{Completeness: CompletenessIncomplete}, errors.New("source does not expose access enumeration through the legacy adapter")
}

func (legacyAdapter) Membership(context.Context, source.Connection, Cursor, int) (MembershipPage, error) {
	return MembershipPage{Completeness: CompletenessIncomplete}, errors.New("source does not expose membership enumeration through the legacy adapter")
}
