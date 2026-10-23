package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"time"

	"enclava-go/internal/artifacts"
	"enclava-go/internal/auth"
	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

const connectorPageSize = 100
const connectorPageLimit = 1000
const connectorMaxArtifactBytes = 32 << 20

func (s *Service) runDurableStream(ctx context.Context, claim *jobs.Claim, principal auth.Principal, conn source.Connection, adapter Adapter, stream StreamKind) error {
	job := claim.Job()
	state, err := s.observations.StartRun(ctx, conn.TenantID, conn.ID, string(stream), job.ID)
	if err != nil {
		return err
	}
	switch stream {
	case StreamContent:
		return s.runContentStream(ctx, claim, principal, conn, adapter, state)
	case StreamAccess:
		return s.runAccessStream(ctx, claim, principal, conn, adapter, state)
	case StreamMembership:
		return s.runMembershipStream(ctx, claim, principal, conn, adapter, state)
	default:
		return fmt.Errorf("unsupported connector stream %q", stream)
	}
}

func (s *Service) runContentStream(ctx context.Context, claim *jobs.Claim, principal auth.Principal, conn source.Connection, adapter Adapter, state StreamState) error {
	if strings.TrimSpace(conn.TargetCollectionID) == "" {
		return errors.New("target_collection_id is required for connector sync")
	}
	// Re-enqueue ledgered versions first; jobs.Enqueue deduplicates the crash window after persist-before-enqueue.
	pending, err := s.observations.ListObservations(ctx, state)
	if err != nil {
		return err
	}
	if err := s.enqueueContent(ctx, principal, conn, pending); err != nil {
		return err
	}
	cursor := Cursor(state.Cursor)
	snapshotListing := true
	for pageNo := 0; pageNo < connectorPageLimit; pageNo++ {
		if cancelled, err := claim.CancelRequested(ctx); err != nil {
			return err
		} else if cancelled {
			return context.Canceled
		}
		page, pageErr := adapter.Content(ctx, conn, cursor, connectorPageSize)
		if pageErr != nil {
			_, _ = s.observations.PersistPage(ctx, state, nil, string(cursor), false, true, pageErr.Error())
			return fmt.Errorf("connector content page: %w", pageErr)
		}
		if len(page.Items) > connectorPageSize {
			_, _ = s.observations.PersistPage(ctx, state, nil, string(cursor), false, true, "adapter exceeded page bound")
			return errors.New("connector adapter exceeded content page bound")
		}
		snapshotListing = snapshotListing && page.Snapshot
		observations := make([]Observation, 0, len(page.Items))
		var jobsToQueue []Observation
		if err := s.observations.WithFence(ctx, state, func(source.Tx) error {
			for _, item := range page.Items {
				observation, err := s.observeContentItem(ctx, conn, item)
				if err != nil {
					return err
				}
				observations = append(observations, observation)
				if observation.ProcessingError == "" && observation.ArtifactRef.ID != "" {
					jobsToQueue = append(jobsToQueue, observation)
				}
			}
			return nil
		}); err != nil {
			_, _ = s.observations.PersistPage(ctx, state, observations, string(cursor), false, true, err.Error())
			return err
		}
		state, err = s.observations.PersistPage(ctx, state, observations, string(cursor), false, true, "")
		if err != nil {
			return err
		}
		if err := s.enqueueContent(ctx, principal, conn, jobsToQueue); err != nil {
			return err
		}
		terminal := page.NextCursor == "" && page.Completeness == CompletenessComplete
		if page.Completeness != CompletenessComplete && page.Completeness != CompletenessIncomplete {
			return errors.New("adapter returned invalid content completeness")
		}
		state, err = s.observations.PersistPage(ctx, state, nil, string(page.NextCursor), terminal, !terminal, "")
		if err != nil {
			return err
		}
		if terminal {
			if snapshotListing {
				return s.sweepMissingContent(ctx, state)
			}
			return nil
		}
		if page.NextCursor == "" {
			return errors.New("content listing ended incomplete")
		}
		cursor = page.NextCursor
	}
	_, _ = s.observations.PersistPage(ctx, state, nil, string(cursor), false, true, "page limit exceeded")
	return errors.New("connector content listing exceeded page limit")
}

func (s *Service) observeContentItem(ctx context.Context, conn source.Connection, item ContentItem) (Observation, error) {
	if item.NativeID == "" {
		return Observation{}, errors.New("connector content item native id is required")
	}
	kind := firstNonEmpty(item.Kind, conn.Kind)
	var upstream *string
	if item.NativeRevision != "" {
		v := item.NativeRevision
		upstream = &v
	}
	var parentID string
	if item.ParentNativeID != "" {
		if parent, err := s.canonical.ObjectForNativeID(ctx, conn.TenantID, conn.ID, kind, item.ParentNativeID); err == nil {
			parentID = parent.ID
		}
	}
	objectID, err := connectorObjectID()
	if err != nil {
		return Observation{}, err
	}
	object, err := s.canonical.UpsertObject(ctx, source.Object{
		TenantID: conn.TenantID, ConnectionID: conn.ID, ID: objectID, Kind: kind,
		NativeID: item.NativeID, ParentObjectID: parentID,
		AuthorizationMode: source.AuthorizationSourceACL, UpstreamRevision: upstream, ObservedSeq: 1,
	})
	if err != nil {
		return Observation{}, err
	}
	latest, err := s.canonical.LatestObjectVersion(ctx, conn.TenantID, object.ID)
	seq := int64(1)
	if err == nil {
		seq = latest.ObservedSeq + 1
	} else if !errors.Is(err, source.ErrDenied) {
		return Observation{}, err
	}
	observation := Observation{
		TenantID: conn.TenantID, ConnectionID: conn.ID, Stream: string(StreamContent), NativeID: item.NativeID,
		ObjectRef: source.ObjectRef{TenantID: conn.TenantID, ObjectID: object.ID, ObservedSeq: seq},
	}
	content := item.Content
	if content == nil && item.Text != "" {
		content = []byte(item.Text)
	}
	if content == nil {
		observation.ProcessingError = "content item has no bytes or inline text"
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	filename := strings.TrimSpace(item.Filename)
	if filename == "" {
		filename = filepath.Base(item.NativeID)
	}
	if filename == "." || filename == "" {
		filename = "source-content.txt"
	}
	mimeType := strings.TrimSpace(item.MIMEType)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(filename))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	writer, ref, err := s.artifacts.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: conn.TenantID, Kind: artifacts.KindOriginal, Version: 1}, MimeType: mimeType}, time.Hour, connectorMaxArtifactBytes)
	if err != nil {
		observation.ProcessingError = err.Error()
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	if _, err := io.Copy(writer, bytes.NewReader(content)); err != nil {
		_ = writer.Abort(ctx)
		observation.ProcessingError = err.Error()
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	if err := writer.Finalize(ctx); err != nil {
		observation.ProcessingError = err.Error()
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	ref = writer.Ref()
	if err := s.canonical.InsertObjectVersion(ctx, conn.TenantID, source.ObjectVersion{
		ObjectID: object.ID, ObservedSeq: seq, ArtifactRef: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize,
		MimeType: mimeType, OriginalFilename: filename, UpstreamRevision: upstream,
		Provenance: connectorProvenance(conn, item),
	}); err != nil {
		observation.ProcessingError = err.Error()
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	observation.ArtifactRef = ref
	observation.Payload = connectorPayload(item, filename, mimeType)
	return observation, nil
}

func connectorPayload(item ContentItem, filename, mimeType string) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{"native_id": item.NativeID, "parent_native_id": item.ParentNativeID, "kind": item.Kind, "native_revision": item.NativeRevision, "filename": filename, "mime_type": mimeType, "url": item.URL, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "locator": item.Locator})
	return payload
}

func connectorProvenance(conn source.Connection, item ContentItem) string {
	payload, _ := json.Marshal(map[string]any{"connection_id": conn.ID, "source_kind": conn.Kind, "native_id": item.NativeID, "url": item.URL, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "locator": item.Locator})
	return string(payload)
}

func (s *Service) enqueueContent(ctx context.Context, principal auth.Principal, conn source.Connection, observations []Observation) error {
	for _, observation := range observations {
		if observation.ArtifactRef.ID == "" || observation.ProcessingError != "" || observation.ObjectRef.ObjectID == "" || observation.ObjectRef.ObservedSeq <= 0 {
			continue
		}
		key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", conn.ID, observation.ObjectRef.ObjectID, observation.ObjectRef.ObservedSeq)))
		_, _, err := s.jobs.Enqueue(ctx, principal, jobs.EnqueueRequest{
			TenantID: principal.TenantID, Operation: OperationIngest,
			IdempotencyKey: OperationIngest + ":" + hex.EncodeToString(key[:]),
			Subject:        jobs.Subject{ConnectionID: conn.ID, ObjectID: observation.ObjectRef.ObjectID, ObservedRevision: observation.ObjectRef.ObservedSeq},
			InputRef:       observation.ArtifactRef.ID, InputHash: observation.ArtifactRef.SHA256,
			Policy: map[string]any{"collection_id": conn.TargetCollectionID, "source_ref": observation.ObjectRef, "artifact_ref": observation.ArtifactRef, "source_kind": conn.Kind},
		})
		if err != nil {
			return fmt.Errorf("enqueue rag.ingest: %w", err)
		}
	}
	return nil
}

func (s *Service) runAccessStream(ctx context.Context, claim *jobs.Claim, principal auth.Principal, conn source.Connection, adapter Adapter, state StreamState) error {
	return s.runGrantStream(ctx, claim, principal, conn, adapter, state, false)
}

func (s *Service) runMembershipStream(ctx context.Context, claim *jobs.Claim, principal auth.Principal, conn source.Connection, adapter Adapter, state StreamState) error {
	return s.runGrantStream(ctx, claim, principal, conn, adapter, state, true)
}

func (s *Service) runGrantStream(ctx context.Context, claim *jobs.Claim, principal auth.Principal, conn source.Connection, adapter Adapter, state StreamState, membership bool) error {
	kind := source.SnapshotGrants
	if membership {
		kind = source.SnapshotMembership
	}
	revision := state.Fence
	if err := s.canonical.BeginSnapshot(ctx, conn.TenantID, conn.ID, kind, revision); err != nil {
		return err
	}
	cursor := Cursor(state.Cursor)
	for pageNo := 0; pageNo < connectorPageLimit; pageNo++ {
		if cancelled, err := claim.CancelRequested(ctx); err != nil {
			return err
		} else if cancelled {
			return context.Canceled
		}
		var grants []AccessGrant
		var next Cursor
		var completeness Completeness
		var snapshot bool
		var err error
		if membership {
			page, e := adapter.Membership(ctx, conn, cursor, connectorPageSize)
			err, grants, next, completeness, snapshot = e, page.Memberships, page.NextCursor, page.Completeness, page.Snapshot
		} else {
			page, e := adapter.Access(ctx, conn, cursor, connectorPageSize)
			err, grants, next, completeness, snapshot = e, page.Grants, page.NextCursor, page.Completeness, page.Snapshot
		}
		if err != nil {
			_, _ = s.observations.PersistPage(ctx, state, nil, string(cursor), false, true, err.Error())
			return fmt.Errorf("connector %s page: %w", state.Stream, err)
		}
		if len(grants) > connectorPageSize {
			_, _ = s.observations.PersistPage(ctx, state, nil, string(cursor), false, true, "adapter exceeded page bound")
			return errors.New("connector adapter exceeded authorization page bound")
		}
		observations := make([]Observation, 0, len(grants))
		for _, grant := range grants {
			payload, e := json.Marshal(grant)
			if e != nil {
				return e
			}
			native := base64.RawURLEncoding.EncodeToString(payload)
			observations = append(observations, Observation{TenantID: conn.TenantID, ConnectionID: conn.ID, Stream: string(state.Stream), NativeID: native, Payload: payload})
		}
		state, err = s.observations.PersistPage(ctx, state, observations, string(cursor), false, true, "")
		if err != nil {
			return err
		}
		terminal := next == "" && completeness == CompletenessComplete
		if !snapshot {
			terminal = false
		}
		state, err = s.observations.PersistPage(ctx, state, nil, string(next), terminal, !terminal, "")
		if err != nil {
			return err
		}
		if terminal {
			return s.applyGrantSnapshot(ctx, principal, conn, state, revision, membership)
		}
		if next == "" {
			return errors.New("authorization listing ended incomplete or was not a snapshot")
		}
		cursor = next
	}
	return errors.New("connector authorization listing exceeded page limit")
}

func (s *Service) applyGrantSnapshot(ctx context.Context, principal auth.Principal, conn source.Connection, state StreamState, revision int64, membership bool) error {
	observations, err := s.observations.ListObservations(ctx, state)
	if err != nil {
		return err
	}
	var grants []source.SourceGrant
	var memberships []source.SourceGroupMembership
	for _, observation := range observations {
		if observation.ProcessingError != "" {
			return fmt.Errorf("authorization observation %q failed: %s", observation.NativeID, observation.ProcessingError)
		}
		var grant AccessGrant
		if err := json.Unmarshal(observation.Payload, &grant); err != nil {
			return fmt.Errorf("decode authorization observation: %w", err)
		}
		if membership {
			group, err := s.resolveOrCreateGroup(ctx, conn, grant.NativeGroupID)
			if err != nil {
				return err
			}
			identity, err := s.canonical.SourceIdentityForNativeID(ctx, conn.TenantID, conn.ID, grant.NativePrincipalID)
			if err != nil {
				return err
			}
			memberships = append(memberships, source.SourceGroupMembership{TenantID: conn.TenantID, ConnectionID: conn.ID, GroupID: group.ID, MemberID: identity.ID, AccessRevision: revision})
			continue
		}
		if grant.Scope != "" && grant.Scope != "read" && grant.Scope != "view" {
			return fmt.Errorf("unsupported source access scope %q", grant.Scope)
		}
		object, err := s.canonical.ObjectForNativeID(ctx, conn.TenantID, conn.ID, firstNonEmpty(grant.NativeObjectKind, conn.Kind), grant.NativeObjectID)
		if err != nil {
			return err
		}
		g := source.SourceGrant{TenantID: conn.TenantID, ConnectionID: conn.ID, ObjectID: object.ID, Effect: grant.Effect, AccessRevision: revision}
		switch grant.Effect {
		case source.GrantEffectAllow, source.GrantEffectDeny:
		default:
			return fmt.Errorf("unsupported source grant effect %q", grant.Effect)
		}
		if grant.NativePrincipalID != "" {
			identity, err := s.canonical.SourceIdentityForNativeID(ctx, conn.TenantID, conn.ID, grant.NativePrincipalID)
			if err != nil {
				return err
			}
			g.SubjectKind, g.SubjectID = source.GrantSubjectUser, identity.ID
		} else if grant.NativeGroupID != "" {
			group, err := s.resolveOrCreateGroup(ctx, conn, grant.NativeGroupID)
			if err != nil {
				return err
			}
			g.SubjectKind, g.SubjectID = source.GrantSubjectGroup, group.ID
		} else {
			return errors.New("access grant has no native principal or group")
		}
		grants = append(grants, g)
	}
	kind := source.SnapshotGrants
	if membership {
		kind = source.SnapshotMembership
	}
	if err := s.observations.WithFence(ctx, state, func(tx source.Tx) error {
		if membership {
			return s.canonical.ApplyMembershipSnapshot(ctx, tx, conn.TenantID, conn.ID, revision, memberships)
		}
		return s.canonical.ApplyAccessSnapshot(ctx, tx, conn.TenantID, conn.ID, revision, grants)
	}); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s:%s:%d", OperationAccess, conn.ID, kind, revision)
	_, _, err = s.jobs.Enqueue(ctx, principal, jobs.EnqueueRequest{TenantID: principal.TenantID, Operation: OperationAccess, IdempotencyKey: key, Subject: jobs.Subject{ConnectionID: conn.ID, ObservedRevision: revision}, InputRef: conn.ID, InputHash: fmt.Sprint(revision), Policy: map[string]any{"connection_id": conn.ID, "snapshot_kind": kind, "revision": revision}})
	if err != nil {
		return fmt.Errorf("enqueue rag.access: %w", err)
	}
	return nil
}

func (s *Service) resolveOrCreateGroup(ctx context.Context, conn source.Connection, nativeID string) (source.SourceGroup, error) {
	if nativeID == "" {
		return source.SourceGroup{}, errors.New("native source group id is required")
	}
	group, err := s.canonical.SourceGroupForNativeID(ctx, conn.TenantID, conn.ID, nativeID)
	if err == nil {
		return group, nil
	}
	if !errors.Is(err, source.ErrDenied) {
		return source.SourceGroup{}, err
	}
	id, err := connectorObjectID()
	if err != nil {
		return source.SourceGroup{}, err
	}
	if err := s.canonical.InsertSourceGroup(ctx, source.SourceGroup{TenantID: conn.TenantID, ConnectionID: conn.ID, SourceKind: conn.Kind, ID: id, NativeID: nativeID, Name: nativeID}); err != nil {
		return source.SourceGroup{}, err
	}
	return s.canonical.SourceGroupForNativeID(ctx, conn.TenantID, conn.ID, nativeID)
}

func connectorObjectID() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate connector object id: %w", err)
	}
	return "src_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Service) sweepMissingContent(ctx context.Context, state StreamState) error {
	observations, err := s.observations.ListObservations(ctx, state)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		if observation.ObjectRef.ObjectID != "" {
			seen[observation.ObjectRef.ObjectID] = struct{}{}
		}
	}
	objects, err := s.canonical.ListObjectsByConnection(ctx, state.TenantID, state.ConnectionID)
	if err != nil {
		return err
	}
	return s.observations.WithFence(ctx, state, func(source.Tx) error {
		for _, object := range objects {
			if _, ok := seen[object.ID]; ok || object.DeletedAt != nil || object.TombstonedAt != nil {
				continue
			}
			if err := s.canonical.MarkObjectDeleted(ctx, state.TenantID, object.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
