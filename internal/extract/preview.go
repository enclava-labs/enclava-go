package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

const OperationPreview = "document.preview"

// Non-terminal previews are deliberately capped per tenant; concurrent submissions may race this best-effort check.
const previewAdmissionLimit = 25

var ErrPreviewAdmissionLimited = errors.New("extract: preview admission limit reached")

type PreviewMaintenance interface {
	CountActivePreviews(ctx context.Context, tenantID string) (int, error)
	ExpiredPreviewJobs(ctx context.Context, before time.Time, limit int) ([]ExpiredPreview, error)
	DeleteExpiredPreviewJobs(ctx context.Context, ids []string) error
}

// ChunkProfiles reads immutable profile versions in the caller's tenant.
type ChunkProfiles interface {
	Get(ctx context.Context, tenantID, versionID string) (source.ChunkProfile, error)
	List(ctx context.Context, tenantID string) ([]source.ChunkProfile, error)
}

type PreviewSource interface {
	source.SourceRepository
	GetObjectVersionByArtifact(ctx context.Context, tenantID, artifactRef string) (source.Object, source.ObjectVersion, error)
}

type PreviewService struct {
	profiles    ChunkProfiles
	jobRows     JobReader
	maintenance PreviewMaintenance
	jobs        *jobs.Service
	artifacts   *artifacts.Store
	parser      *Parser
	source      PreviewSource
	authorizer  *source.Authorizer
	maxBytes    int64
}

func NewPreviewService(profiles ChunkProfiles, jobRows JobReader, jobSvc *jobs.Service, store *artifacts.Store, sourceRepo PreviewSource, maxUploadMB int64) *PreviewService {
	if maxUploadMB <= 0 {
		maxUploadMB = 32
	}
	return &PreviewService{profiles: profiles, jobRows: jobRows, maintenance: jobRows.(PreviewMaintenance), jobs: jobSvc, artifacts: store, parser: NewParser(store), source: sourceRepo, authorizer: source.NewAuthorizer(sourceRepo), maxBytes: maxUploadMB * 1024 * 1024}
}

func (s *PreviewService) ListProfiles(ctx context.Context, p auth.Principal) ([]source.ChunkProfile, error) {
	if p.TenantID == "" || s.profiles == nil {
		return nil, ErrForbidden
	}
	return s.profiles.List(ctx, p.TenantID)
}

func (s *PreviewService) GetProfile(ctx context.Context, p auth.Principal, id string) (source.ChunkProfile, error) {
	if p.TenantID == "" || s.profiles == nil {
		return source.ChunkProfile{}, ErrNotFound
	}
	profile, err := s.profiles.Get(ctx, p.TenantID, id)
	if err != nil {
		return source.ChunkProfile{}, ErrNotFound
	}
	return profile, nil
}

// Submit streams an upload directly into durable artifact storage before
// enqueueing a job that freezes the artifact and immutable profile version.
func (s *PreviewService) Submit(ctx context.Context, p auth.Principal, filename, mimeType, profileID string, body io.Reader) (jobs.Job, error) {
	if p.TenantID == "" || p.ActorID == "" || s.jobs == nil || s.artifacts == nil || body == nil {
		return jobs.Job{}, ErrForbidden
	}
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "." || filename == "" || formatFor(filename, mimeType) == "" {
		return jobs.Job{}, ErrUnsupportedFormat
	}
	profile, err := s.profiles.Get(ctx, p.TenantID, profileID)
	if err != nil {
		return jobs.Job{}, ErrNotFound
	}
	if err := ValidateChunkProfile(profile); err != nil {
		return jobs.Job{}, err
	}
	writer, _, err := s.artifacts.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: p.TenantID, Kind: artifacts.KindOriginal}, MimeType: mimeType}, 10*time.Minute, s.maxBytes)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("begin preview input artifact: %w", err)
	}
	if _, err := io.Copy(writer, body); err != nil {
		_ = writer.Abort(ctx)
		return jobs.Job{}, fmt.Errorf("write preview input artifact: %w", err)
	}
	if err := writer.Finalize(ctx); err != nil {
		return jobs.Job{}, fmt.Errorf("finalize preview input artifact: %w", err)
	}
	ref := writer.Ref()
	job, err := s.enqueue(ctx, p, ref, filename, mimeType, profile, source.ObjectRef{TenantID: p.TenantID, ObjectID: "preview-" + ref.ID, ObservedSeq: 1}, true)
	if err != nil {
		_ = s.artifacts.Delete(ctx, ref)
		return jobs.Job{}, err
	}
	return job, nil
}

func (s *PreviewService) SubmitOwnedArtifact(ctx context.Context, p auth.Principal, artifactID, profileID string) (jobs.Job, error) {
	if p.TenantID == "" || p.ActorID == "" || s.source == nil || s.authorizer == nil {
		return jobs.Job{}, ErrForbidden
	}
	profile, err := s.profiles.Get(ctx, p.TenantID, profileID)
	if err != nil {
		return jobs.Job{}, ErrNotFound
	}
	if err := ValidateChunkProfile(profile); err != nil {
		return jobs.Job{}, err
	}
	object, version, err := s.source.GetObjectVersionByArtifact(ctx, p.TenantID, artifactID)
	if err != nil || s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}) != nil {
		return jobs.Job{}, ErrNotFound
	}
	filename := filepath.Base(version.OriginalFilename)
	if formatFor(filename, version.MimeType) == "" {
		return jobs.Job{}, ErrUnsupportedFormat
	}
	ref := artifacts.Ref{TenantID: p.TenantID, ID: artifactID, Kind: artifacts.KindOriginal}
	rc, err := s.artifacts.Read(ctx, ref)
	if err != nil {
		return jobs.Job{}, ErrNotFound
	}
	_ = rc.Close()
	return s.enqueue(ctx, p, ref, filename, version.MimeType, profile, source.ObjectRef{TenantID: p.TenantID, ObjectID: object.ID, ObservedSeq: version.ObservedSeq}, false)
}

func (s *PreviewService) enqueue(ctx context.Context, p auth.Principal, ref artifacts.Ref, filename, mimeType string, profile source.ChunkProfile, objectRef source.ObjectRef, uploadOwned bool) (jobs.Job, error) {
	active, err := s.maintenance.CountActivePreviews(ctx, p.TenantID)
	if err != nil {
		return jobs.Job{}, fmt.Errorf("count active previews: %w", err)
	}
	if active >= previewAdmissionLimit {
		return jobs.Job{}, ErrPreviewAdmissionLimited
	}
	job, _, err := s.jobs.Enqueue(ctx, p, jobs.EnqueueRequest{
		TenantID: p.TenantID, Operation: OperationPreview,
		IdempotencyKey: "preview:" + ref.ID + ":" + profile.ID,
		Subject:        jobs.Subject{ObjectID: objectRef.ObjectID, ObservedRevision: objectRef.ObservedSeq},
		InputRef:       ref.ID, InputHash: ref.SHA256,
		Policy: map[string]any{"filename": filename, "mime_type": mimeType, "artifact_version": ref.Version, "profile": profile, "object_ref": objectRef, "upload_owned": uploadOwned},
	})
	if err != nil {
		return jobs.Job{}, fmt.Errorf("enqueue preview: %w", err)
	}
	return job, nil
}

// RunRetentionSweep removes terminal preview jobs older than seven days and their owned artifacts.
func (s *PreviewService) RunRetentionSweep(ctx context.Context) {
	rows, err := s.maintenance.ExpiredPreviewJobs(ctx, time.Now().UTC().Add(-7*24*time.Hour), 200)
	if err != nil {
		log.Printf("preview retention: list expired jobs: %v", err)
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		cleaned := true
		if row.ResultRef != "" {
			if err := s.artifacts.Delete(ctx, artifacts.Ref{TenantID: row.TenantID, ID: row.ResultRef}); err != nil {
				log.Printf("preview retention: delete result artifact %s: %v", row.ResultRef, err)
				cleaned = false
			}
		}
		if row.UploadOwned && row.InputRef != "" {
			if err := s.artifacts.Delete(ctx, artifacts.Ref{TenantID: row.TenantID, ID: row.InputRef}); err != nil {
				log.Printf("preview retention: delete input artifact %s: %v", row.InputRef, err)
				cleaned = false
			}
		}
		if cleaned {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) > 0 {
		if err := s.maintenance.DeleteExpiredPreviewJobs(ctx, ids); err != nil {
			log.Printf("preview retention: delete expired jobs: %v", err)
		}
	}
	log.Printf("preview retention: processed %d terminal jobs", len(rows))
}

func (s *PreviewService) RunRetention(ctx context.Context) {
	s.RunRetentionSweep(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunRetentionSweep(ctx)
		}
	}
}

type ExpiredPreview struct {
	TenantID, ID, InputRef, ResultRef string
	UploadOwned                       bool
}

func (s *PreviewService) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, _ jobs.InputFunc) (string, error) {
		job := claim.Job()
		var p auth.Principal
		if err := json.Unmarshal(job.PrincipalSnapshot, &p); err != nil {
			return "", fmt.Errorf("decode frozen principal: %w", err)
		}
		cancelled, err := claim.CancelRequested(ctx)
		if err != nil {
			return "", err
		}
		if cancelled {
			return "", context.Canceled
		}
		profileID := profileIDFromJob(job.Policy["profile"])
		profile, err := s.profiles.Get(ctx, job.TenantID, profileID)
		if err != nil {
			return "", fmt.Errorf("load immutable chunk profile: %w", err)
		}
		var snapshot source.ChunkProfile
		encoded, _ := json.Marshal(job.Policy["profile"])
		if err := json.Unmarshal(encoded, &snapshot); err != nil || snapshot != profile {
			return "", errors.New("preview chunk profile version changed")
		}
		version := intFromPolicy(job.Policy["artifact_version"])
		ref := artifacts.Ref{TenantID: job.TenantID, ID: job.InputRef, Kind: artifacts.KindOriginal, Version: version}
		filename, _ := job.Policy["filename"].(string)
		mimeType, _ := job.Policy["mime_type"].(string)
		var objectRef source.ObjectRef
		objectBytes, _ := json.Marshal(job.Policy["object_ref"])
		if err := json.Unmarshal(objectBytes, &objectRef); err != nil || objectRef.TenantID != job.TenantID || objectRef.ObjectID == "" || objectRef.ObservedSeq < 1 {
			return "", errors.New("preview job has invalid source identity")
		}
		if !boolFromPolicy(job.Policy["upload_owned"]) {
			object, err := s.source.GetObject(ctx, job.TenantID, objectRef.ObjectID)
			if err != nil {
				return "", source.ErrDenied
			}
			versionRow, err := s.source.GetObjectVersion(ctx, job.TenantID, objectRef.ObjectID, objectRef.ObservedSeq)
			if err != nil || s.authorizer.CanReadVersion(ctx, p, object, versionRow, source.ReadOptions{}) != nil || versionRow.ArtifactRef != ref.ID {
				return "", source.ErrDenied
			}
		}
		doc, err := s.parser.Parse(ctx, ref, ParseOptions{Filename: filename, MIMEType: mimeType, ObjectRef: objectRef})
		if err != nil {
			return "", err
		}
		chunks, err := Chunk(doc, profile)
		if err != nil {
			return "", err
		}
		if len(chunks) == 0 {
			return "", errors.New("preview has no extractable chunks")
		}
		cancelled, err = claim.CancelRequested(ctx)
		if err != nil {
			return "", err
		}
		if cancelled || ctx.Err() != nil {
			return "", context.Canceled
		}
		warnings := append([]string(nil), doc.ParseWarnings...)
		for _, chunk := range chunks {
			warnings = append(warnings, chunk.Warnings...)
		}
		result := previewResult{Document: previewDocumentFrom(doc), Chunks: previewChunksFrom(chunks), Warnings: warnings, ChunkProfileVersionID: profile.ID}
		writer, _, err := s.artifacts.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: job.TenantID, Kind: artifacts.KindResult}, MimeType: "application/json"}, 10*time.Minute, 64<<20)
		if err != nil {
			return "", fmt.Errorf("begin preview result artifact: %w", err)
		}
		defer func() { _ = writer.Close() }()
		if err := json.NewEncoder(writer).Encode(result); err != nil {
			return "", fmt.Errorf("write preview result artifact: %w", err)
		}
		if err := writer.Finalize(ctx); err != nil {
			return "", fmt.Errorf("finalize preview result artifact: %w", err)
		}
		return writer.Ref().ID, nil
	}
}

type previewObjectRef struct {
	TenantID    string `json:"tenant_id"`
	ObjectID    string `json:"object_id"`
	ObservedSeq int64  `json:"observed_seq"`
}

type previewBlock struct {
	ID        string         `json:"id"`
	Ordinal   int            `json:"ordinal"`
	Kind      string         `json:"kind"`
	ParentID  string         `json:"parent_id,omitempty"`
	Text      string         `json:"text"`
	ByteStart int            `json:"byte_start"`
	ByteEnd   int            `json:"byte_end"`
	Locator   source.Locator `json:"locator"`
}

type previewDocument struct {
	Ref           previewObjectRef `json:"ref"`
	Blocks        []previewBlock   `json:"blocks"`
	ParseWarnings []string         `json:"parse_warnings,omitempty"`
}

type previewSpan struct {
	Ref       previewObjectRef `json:"ref"`
	BlockID   string           `json:"block_id"`
	ByteStart int              `json:"byte_start"`
	ByteEnd   int              `json:"byte_end"`
}

type previewChunk struct {
	ID            string           `json:"id"`
	ObjectRef     previewObjectRef `json:"object_ref"`
	ProfileID     string           `json:"profile_id"`
	Ordinal       int              `json:"ordinal"`
	Text          string           `json:"text"`
	Spans         []previewSpan    `json:"spans"`
	ParentChunkID string           `json:"parent_chunk_id,omitempty"`
	Warnings      []string         `json:"warnings,omitempty"`
}

type previewResult struct {
	Document              previewDocument `json:"document"`
	Chunks                []previewChunk  `json:"chunks"`
	Warnings              []string        `json:"warnings"`
	ChunkProfileVersionID string          `json:"chunk_profile_version_id"`
	OriginalURL           string          `json:"original_url"`
}

func previewRef(ref source.ObjectRef) previewObjectRef {
	return previewObjectRef{TenantID: ref.TenantID, ObjectID: ref.ObjectID, ObservedSeq: ref.ObservedSeq}
}

func previewDocumentFrom(document source.Document) previewDocument {
	out := previewDocument{Ref: previewRef(document.Ref), Blocks: make([]previewBlock, 0, len(document.Blocks)), ParseWarnings: document.ParseWarnings}
	for _, block := range document.Blocks {
		out.Blocks = append(out.Blocks, previewBlock{ID: block.ID, Ordinal: block.Ordinal, Kind: block.Kind, ParentID: block.ParentID, Text: block.Text, ByteStart: block.ByteStart, ByteEnd: block.ByteEnd, Locator: block.Locator})
	}
	return out
}

func previewChunksFrom(chunks []source.Chunk) []previewChunk {
	out := make([]previewChunk, 0, len(chunks))
	for _, chunk := range chunks {
		item := previewChunk{ID: chunk.ID, ObjectRef: previewRef(chunk.ObjectRef), ProfileID: chunk.ProfileID, Ordinal: chunk.Ordinal, Text: chunk.Text, ParentChunkID: chunk.ParentChunkID, Warnings: chunk.Warnings, Spans: make([]previewSpan, 0, len(chunk.Spans))}
		for _, span := range chunk.Spans {
			item.Spans = append(item.Spans, previewSpan{Ref: previewRef(span.Ref), BlockID: span.BlockID, ByteStart: span.ByteStart, ByteEnd: span.ByteEnd})
		}
		out = append(out, item)
	}
	return out
}

type PreviewStatus struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

func (s *PreviewService) Get(ctx context.Context, p auth.Principal, id string) (PreviewStatus, error) {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationPreview {
		return PreviewStatus{}, ErrNotFound
	}
	errorMessage := ""
	switch row.Status {
	case jobs.StatusFailed:
		errorMessage = "document preview failed"
	case jobs.StatusUnknown:
		errorMessage = "document preview outcome is unknown"
	}
	return PreviewStatus{ID: row.ID, Status: row.Status, ErrorMessage: errorMessage, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt}, nil
}

func (s *PreviewService) Result(ctx context.Context, p auth.Principal, id string) (previewResult, error) {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationPreview || row.Status != jobs.StatusSucceeded || row.ResultRef == "" {
		return previewResult{}, ErrNotFound
	}
	rc, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: row.ResultRef, Kind: artifacts.KindResult})
	if err != nil {
		return previewResult{}, ErrNotFound
	}
	defer func() { _ = rc.Close() }()
	var result previewResult
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&result); err != nil {
		return previewResult{}, fmt.Errorf("decode preview result: %w", err)
	}
	result.OriginalURL = "/api/v1/document-previews/" + id + "/original"
	return result, nil
}

func (s *PreviewService) Original(ctx context.Context, p auth.Principal, id string) (io.ReadCloser, string, string, error) {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationPreview {
		return nil, "", "", ErrNotFound
	}
	var objectRef source.ObjectRef
	objectBytes, _ := json.Marshal(row.Policy["object_ref"])
	if err := json.Unmarshal(objectBytes, &objectRef); err != nil || objectRef.TenantID != p.TenantID {
		return nil, "", "", ErrNotFound
	}
	if !boolFromPolicy(row.Policy["upload_owned"]) {
		object, err := s.source.GetObject(ctx, p.TenantID, objectRef.ObjectID)
		if err != nil {
			return nil, "", "", ErrNotFound
		}
		version, err := s.source.GetObjectVersion(ctx, p.TenantID, objectRef.ObjectID, objectRef.ObservedSeq)
		if err != nil || s.authorizer.CanReadVersion(ctx, p, object, version, source.ReadOptions{}) != nil || version.ArtifactRef != row.InputRef {
			return nil, "", "", ErrNotFound
		}
	}
	filename, _ := row.Policy["filename"].(string)
	mimeType, _ := row.Policy["mime_type"].(string)
	reader, err := s.artifacts.Read(ctx, artifacts.Ref{TenantID: p.TenantID, ID: row.InputRef, Kind: artifacts.KindOriginal})
	if err != nil {
		return nil, "", "", ErrNotFound
	}
	return reader, filepath.Base(filename), mimeType, nil
}

func (s *PreviewService) Cancel(ctx context.Context, p auth.Principal, id string) error {
	row, err := s.jobRows.GetOwnedJob(ctx, p.TenantID, id, jobs.SubmitterKey(p))
	if err != nil || row.Operation != OperationPreview {
		return ErrNotFound
	}
	return s.jobs.Cancel(ctx, p, id)
}

func profileIDFromJob(v any) string {
	m, _ := v.(map[string]any)
	id, _ := m["ID"].(string)
	return id
}

func intFromPolicy(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
func boolFromPolicy(v any) bool {
	b, _ := v.(bool)
	return b
}
