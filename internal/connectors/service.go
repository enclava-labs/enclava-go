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
	"time"

	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/rag"
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
}

// RAGUploader is the rag ingest surface the sync attempt hands observed
// documents to. The principal is the frozen job principal — never forged.
type RAGUploader interface {
	UploadDocument(ctx context.Context, p auth.Principal, collectionID string, input rag.UploadDocumentInput) (rag.UploadDocumentOutput, error)
}

// JobReader is the owner-bound durable job view for SyncStatus/ListJobs.
type JobReader interface {
	GetOwnedJob(ctx context.Context, tenantID, id, submitterKey string) (jobs.JobRow, error)
	ListOwnedJobs(ctx context.Context, tenantID, submitterKey string, limit int) ([]jobs.JobRow, error)
}

type Service struct {
	repo       Repository
	jobs       *jobs.Service
	jobRows    JobReader
	rag        RAGUploader
	httpClient *http.Client
	// allowPrivateBaseURL permits loopback and private config.base_url hosts.
	// NewService leaves it false; tests enable it for httptest servers.
	allowPrivateBaseURL bool
}

func NewService(repo Repository, jobSvc *jobs.Service, jobRows JobReader, ragSvc RAGUploader) *Service {
	return &Service{
		repo:    repo,
		jobs:    jobSvc,
		jobRows: jobRows,
		rag:     ragSvc,
		httpClient: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: refuseConnectorRedirect,
		},
	}
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
		out = append(out, connectionView(c))
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
	return connectionView(c), nil
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

func (s *Service) SyncSource(ctx context.Context, p auth.Principal, sourceID string) (SyncJob, error) {
	if err := requireTenant(p); err != nil {
		return SyncJob{}, err
	}
	if s.jobs == nil {
		return SyncJob{}, errors.New("connectors: durable jobs are not configured")
	}
	conn, err := s.repo.GetConnection(ctx, p.TenantID, strings.TrimSpace(sourceID))
	if err != nil {
		return SyncJob{}, err
	}
	if conn.Status != "active" {
		return SyncJob{}, errors.New("connector source is disabled")
	}
	cfgBytes, err := json.Marshal(conn.Config)
	if err != nil {
		return SyncJob{}, fmt.Errorf("hash connection config: %w", err)
	}
	sum := sha256.Sum256(append(cfgBytes, []byte("\x00"+conn.CredentialRef)...))
	job, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID:       p.TenantID,
		Operation:      OperationSync,
		IdempotencyKey: "connector.sync:" + conn.ID + ":" + hex.EncodeToString(sum[:]),
		Subject:        jobs.Subject{ConnectionID: conn.ID},
		InputRef:       conn.ID,
		InputHash:      hex.EncodeToString(sum[:]),
		Policy:         map[string]any{"connection_id": conn.ID},
	})
	if err != nil {
		return SyncJob{}, err
	}
	return jobView(job), nil
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
	rows, err := s.jobRows.ListOwnedJobs(ctx, p.TenantID, jobs.SubmitterKey(p), limit*4+offset)
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
	var p auth.Principal
	if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
		return "", fmt.Errorf("decode frozen principal: %w", err)
	}
	conn, err := s.repo.GetConnection(ctx, job.TenantID, job.Subject.ConnectionID)
	if err != nil {
		return "", err
	}
	if err := claim.MarkDispatched(ctx); err != nil {
		return "", err
	}
	v := connectionView(conn)
	stats, checkpoint, syncErr := s.syncDocuments(ctx, p, v)
	completedAt := time.Now().UTC()
	status := JobStatusSucceeded
	errMsg := ""
	if syncErr != nil {
		status = JobStatusFailed
		errMsg = syncErr.Error()
	}
	_ = s.repo.SetSyncState(ctx, job.TenantID, conn.ID, completedAt, status, errMsg)
	if syncErr != nil {
		return "", syncErr
	}
	if len(checkpoint) > 0 {
		v.Checkpoint = checkpoint
		conn2 := viewConnection(v, job.TenantID, conn.CreatedBy)
		conn2.Status = conn.Status
		if err := s.repo.UpdateConnection(ctx, conn2); err != nil && !errors.Is(err, source.ErrDenied) {
			return "", err
		}
	}
	statsJSON, _ := json.Marshal(stats)
	return string(statsJSON), nil
}

// ---------------------------------------------------------------------
// Fetch/upload pipeline — worker-side only now; SyncSource enqueues.

func (s *Service) syncDocuments(ctx context.Context, p auth.Principal, source Source) (map[string]any, map[string]any, error) {
	if s.rag == nil {
		return map[string]any{"uploaded_documents": 0}, nil, errors.New("rag service is not configured")
	}
	if strings.TrimSpace(source.TargetCollectionID) == "" {
		return map[string]any{"uploaded_documents": 0}, nil, errors.New("target_collection_id is required for connector sync")
	}
	if err := s.validateConnectorSource(ctx, source); err != nil {
		return map[string]any{"uploaded_documents": 0}, nil, err
	}
	documents, documentSource, checkpoint, err := s.documentsForSource(ctx, source)
	if err != nil {
		return map[string]any{"uploaded_documents": 0}, checkpoint, err
	}
	if len(documents) == 0 {
		stats := map[string]any{"uploaded_documents": 0, "fetched_documents": 0, "document_source": documentSource}
		if documentSource == "api" {
			stats["no_changes"] = true
			return stats, checkpoint, nil
		}
		return stats, checkpoint, errors.New("connector sync produced no documents")
	}

	forceFullSync := boolFromAny(source.Config["force_full_sync"], false)
	previousHashes := stringMapFromAny(nonNilMap(source.Checkpoint)["document_hashes"])
	nextHashes := copyStringMap(previousHashes)
	uploaded := 0
	skippedUnchanged := 0
	for i, doc := range documents {
		fileName := strings.TrimSpace(doc.FileName)
		if fileName == "" {
			fileName = fmt.Sprintf("%s-document-%d.txt", source.Type, i+1)
		}
		mimeType := strings.TrimSpace(doc.MimeType)
		if mimeType == "" {
			mimeType = "text/plain"
		}
		content := []byte(doc.Content)
		if len(content) == 0 {
			continue
		}
		sourceID := firstNonEmpty(doc.SourceID, fileName)
		hash := contentHash(content)
		if !forceFullSync && previousHashes[sourceID] == hash {
			nextHashes[sourceID] = hash
			skippedUnchanged++
			continue
		}
		if _, err := s.rag.UploadDocument(ctx, p, source.TargetCollectionID, rag.UploadDocumentInput{
			FileName:   fileName,
			MimeType:   mimeType,
			Bytes:      content,
			SourceType: source.Type,
			SourceID:   sourceID,
			SourceURI:  doc.SourceURI,
			Metadata: map[string]any{
				"connector_source_id": source.ID,
				"connector_type":      source.Type,
			},
		}); err != nil {
			return map[string]any{"uploaded_documents": uploaded}, checkpoint, err
		}
		nextHashes[sourceID] = hash
		uploaded++
	}
	if uploaded == 0 {
		stats := map[string]any{
			"uploaded_documents": uploaded,
			"fetched_documents":  len(documents),
			"document_source":    documentSource,
		}
		if skippedUnchanged > 0 {
			stats["skipped_unchanged"] = skippedUnchanged
			checkpoint = nonNilMap(checkpoint)
			checkpoint["document_hashes"] = nextHashes
			return stats, checkpoint, nil
		}
		return stats, checkpoint, errors.New("connector sync produced no non-empty content")
	}
	checkpoint = nonNilMap(checkpoint)
	checkpoint["document_hashes"] = nextHashes
	return map[string]any{
		"uploaded_documents": uploaded,
		"fetched_documents":  len(documents),
		"skipped_unchanged":  skippedUnchanged,
		"document_source":    documentSource,
	}, checkpoint, nil
}

type connectorDocument struct {
	FileName  string
	MimeType  string
	Content   string
	SourceID  string
	SourceURI string
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
	var b strings.Builder
	latestTS := strings.TrimSpace(stringFromAny(nonNilMap(source.Checkpoint)["slack_latest_ts"]))
	for _, msg := range payload.Messages {
		latestTS = maxSlackTimestamp(latestTS, msg.TS)
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		_, _ = fmt.Fprintf(&b, "[%s %s] %s\n", msg.TS, msg.User, text)
	}
	checkpoint := map[string]any{}
	if latestTS != "" {
		checkpoint["slack_latest_ts"] = latestTS
	}
	content := strings.TrimSpace(b.String())
	if content == "" {
		return nil, checkpoint, nil
	}
	return []connectorDocument{{
		FileName:  "slack-" + channelID + ".txt",
		MimeType:  "text/plain",
		Content:   content,
		SourceID:  "slack:" + channelID,
		SourceURI: "slack://channel/" + channelID,
	}}, checkpoint, nil
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

func boolFromAny(v any, fallback bool) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(b))
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func stringMapFromAny(v any) map[string]string {
	out := map[string]string{}
	switch raw := v.(type) {
	case map[string]string:
		for key, value := range raw {
			out[key] = value
		}
	case map[string]any:
		for key, value := range raw {
			if s := strings.TrimSpace(stringFromAny(value)); s != "" {
				out[key] = s
			}
		}
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func contentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum[:])
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

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
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
	case SourceTypeSlack, SourceTypeNotion:
		return true
	default:
		return false
	}
}

var connectorCredentialRefPattern = regexp.MustCompile(`^CONNECTOR_(GITHUB|SLACK|TEAMS|NOTION|GOOGLE_DRIVE)_[A-Z0-9_]+$`)

func refuseConnectorRedirect(*http.Request, []*http.Request) error {
	return errors.New("connector redirects are not followed")
}

func (s *Service) validateConnectorSource(ctx context.Context, source Source) error {
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
		return fmt.Errorf("credential_ref %q is not allowed", ref)
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
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: refuseConnectorRedirect,
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
