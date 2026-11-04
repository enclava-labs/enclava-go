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
	configHash, err := connectorConfigHash(conn)
	if err != nil {
		return err
	}
	state, err := s.observations.StartRun(ctx, conn.TenantID, conn.ID, string(stream), job.ID, configHash)
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
		nextCursor := page.NextCursor
		if terminal {
			nextCursor = page.Checkpoint
		}
		state, err = s.observations.PersistPage(ctx, state, nil, string(nextCursor), terminal, !terminal, "")
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

func readCapableScope(scope string) bool {
	switch scope {
	case "", "read", "view", "reader", "commenter", "writer", "editor", "owner", "organizer", "fileOrganizer", "member", "post", "reply":
		return true
	}
	return false
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
		parentKind := firstNonEmpty(item.ParentKind, kind)
		parent, err := s.canonical.ObjectForNativeID(ctx, conn.TenantID, conn.ID, parentKind, item.ParentNativeID)
		if errors.Is(err, source.ErrDenied) {
			parent, err = s.canonical.ObjectForNativeIDCrossKind(ctx, conn.TenantID, conn.ID, item.ParentNativeID)
		}
		if errors.Is(err, source.ErrDenied) {
			id, idErr := connectorObjectID()
			if idErr != nil {
				return Observation{}, idErr
			}
			parent, err = s.canonical.UpsertObject(ctx, source.Object{
				TenantID: conn.TenantID, ConnectionID: conn.ID, ID: id, Kind: parentKind,
				NativeID: item.ParentNativeID, AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1,
			})
		}
		if err != nil {
			return Observation{}, err
		}
		parentID = parent.ID
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
	digest := sha256.Sum256(content)
	unchanged := err == nil && object.DeletedAt == nil && object.TombstonedAt == nil && latest.SHA256 == hex.EncodeToString(digest[:])
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
	metadataChanged := err == nil && (filename != latest.OriginalFilename || provenanceValue(latest.Provenance, "title") != item.Title || provenanceValue(latest.Provenance, "source_uri") != item.URL || mimeType != latest.MimeType)
	if unchanged {
		if err := s.canonical.ObserveObject(ctx, conn.TenantID, object.ID, parentID, upstream); err != nil {
			return Observation{}, err
		}
		retry := false
		if !metadataChanged {
			// Failed indexing (for example before the creator's source identity was
			// bound) is retried as a new version so the ingest job key changes.
			failed, failErr := s.canonical.IngestFailed(ctx, conn.TenantID, conn.TargetCollectionID, object.ID, latest.ObservedSeq)
			if failErr != nil {
				return Observation{}, failErr
			}
			retry = failed
		}
		if !metadataChanged && !retry {
			handedOff, handErr := s.canonical.IngestHandoffExists(ctx, conn.TenantID, conn.ID, conn.TargetCollectionID, object.ID, latest.ObservedSeq)
			if handErr != nil {
				return Observation{}, handErr
			}
			observation.ObjectRef.ObservedSeq = latest.ObservedSeq
			if !handedOff {
				observation.ArtifactRef = artifacts.Ref{TenantID: conn.TenantID, ID: latest.ArtifactRef, Kind: artifacts.KindOriginal, Version: 1, SHA256: latest.SHA256, ByteSize: latest.ByteSize}
			}
			observation.Payload = json.RawMessage(`{}`)
			return observation, nil
		}
	}
	var ref artifacts.Ref
	if unchanged {
		ref = artifacts.Ref{TenantID: conn.TenantID, ID: latest.ArtifactRef, Kind: artifacts.KindOriginal, Version: 1, SHA256: latest.SHA256, ByteSize: latest.ByteSize}
	} else {
		writer, _, err := s.artifacts.Begin(ctx, artifacts.Meta{Ref: artifacts.Ref{TenantID: conn.TenantID, Kind: artifacts.KindOriginal, Version: 1}, MimeType: mimeType}, time.Hour, connectorMaxArtifactBytes)
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
		final := writer.Ref()
		final.SHA256, final.ByteSize = hex.EncodeToString(digest[:]), int64(len(content))
		ref = final
	}
	if err := s.canonical.InsertObjectVersion(ctx, conn.TenantID, source.ObjectVersion{
		ObjectID: object.ID, ObservedSeq: seq, ArtifactRef: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize,
		MimeType: mimeType, OriginalFilename: filename, UpstreamRevision: upstream,
		Provenance: connectorProvenance(conn, item),
	}); err != nil {
		observation.ProcessingError = err.Error()
		observation.Payload = json.RawMessage(`{}`)
		return observation, nil
	}
	if err := s.canonical.ObserveObject(ctx, conn.TenantID, object.ID, parentID, upstream); err != nil {
		return Observation{}, err
	}
	observation.ArtifactRef = ref
	observation.Payload = connectorPayload(item, filename, mimeType)
	return observation, nil
}

func provenanceValue(provenance, key string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(provenance), &payload); err != nil {
		return ""
	}
	v, _ := payload[key].(string)
	return v
}

func connectorPayload(item ContentItem, filename, mimeType string) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{"native_id": item.NativeID, "parent_native_id": item.ParentNativeID, "kind": item.Kind, "native_revision": item.NativeRevision, "title": item.Title, "filename": filename, "mime_type": mimeType, "url": item.URL, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "locator": item.Locator})
	return payload
}

func connectorProvenance(conn source.Connection, item ContentItem) string {
	payload, _ := json.Marshal(map[string]any{"connection_id": conn.ID, "source_kind": conn.Kind, "native_id": item.NativeID, "title": item.Title, "source_uri": item.URL, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "locator": item.Locator})
	return string(payload)
}

func (s *Service) enqueueContent(ctx context.Context, principal auth.Principal, conn source.Connection, observations []Observation) error {
	for _, observation := range observations {
		if observation.ArtifactRef.ID == "" || observation.ProcessingError != "" || observation.ObjectRef.ObjectID == "" || observation.ObjectRef.ObservedSeq <= 0 {
			continue
		}
		key := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", conn.ID, conn.TargetCollectionID, observation.ObjectRef.ObjectID, observation.ObjectRef.ObservedSeq)))
		_, _, err := s.jobs.Enqueue(ctx, principal, jobs.EnqueueRequest{
			TenantID: principal.TenantID, Operation: OperationIngest,
			IdempotencyKey: OperationIngest + ":connector-v3:" + hex.EncodeToString(key[:]),
			Subject:        jobs.Subject{ConnectionID: conn.ID, ObjectID: observation.ObjectRef.ObjectID, ObservedRevision: observation.ObjectRef.ObservedSeq},
			InputRef:       observation.ArtifactRef.ID, InputHash: observation.ArtifactRef.SHA256,
			Policy: map[string]any{"collection_id": conn.TargetCollectionID, "source_ref": observation.ObjectRef, "artifact_ref": observation.ArtifactRef, "source_kind": conn.Kind, "observation_payload": observation.Payload},
			Lane:   jobs.LaneBackground,
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
			observations = append(observations, Observation{TenantID: conn.TenantID, ConnectionID: conn.ID, Stream: state.Stream, NativeID: native, Payload: payload})
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
	unmapped := 0
	for _, observation := range observations {
		if observation.ProcessingError != "" {
			return fmt.Errorf("authorization observation %q failed: %s", observation.NativeID, observation.ProcessingError)
		}
		var grant AccessGrant
		if err := json.Unmarshal(observation.Payload, &grant); err != nil {
			return fmt.Errorf("decode authorization observation: %w", err)
		}
		if membership {
			if grant.NativePrincipalID == "" || grant.NativeGroupID == "" {
				return errors.New("membership requires native principal and group")
			}
			group, err := s.resolveOrCreateGroup(ctx, conn, grant.NativeGroupID)
			if err != nil {
				return err
			}
			identity, err := s.observePrincipal(ctx, conn, grant)
			if err != nil {
				return err
			}
			if identity.MappedUserID == "" {
				unmapped++
			}
			memberships = append(memberships, source.SourceGroupMembership{TenantID: conn.TenantID, ConnectionID: conn.ID, GroupID: group.ID, MemberID: identity.ID, AccessRevision: revision})
			continue
		}
		if grant.Effect == source.GrantEffectAllow && !readCapableScope(grant.Scope) {
			return fmt.Errorf("unsupported source access scope %q", grant.Scope)
		}
		if grant.NativeObjectID == "" {
			return errors.New("access grant has no native object")
		}
		object, err := s.canonical.ObjectForNativeID(ctx, conn.TenantID, conn.ID, firstNonEmpty(grant.NativeObjectKind, conn.Kind), grant.NativeObjectID)
		if errors.Is(err, source.ErrDenied) {
			id, idErr := connectorObjectID()
			if idErr != nil {
				return idErr
			}
			object, err = s.canonical.UpsertObject(ctx, source.Object{
				TenantID: conn.TenantID, ConnectionID: conn.ID, ID: id,
				Kind: firstNonEmpty(grant.NativeObjectKind, conn.Kind), NativeID: grant.NativeObjectID,
				AuthorizationMode: source.AuthorizationSourceACL, ObservedSeq: 1,
			})
		}
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
			identity, err := s.observePrincipal(ctx, conn, grant)
			if err != nil {
				return err
			}
			if identity.MappedUserID == "" {
				unmapped++
			}
			g.SubjectKind, g.SubjectID = source.GrantSubjectUser, identity.ID
		} else if grant.NativeGroupID != "" {
			group, err := s.resolveOrCreateGroup(ctx, conn, grant.NativeGroupID)
			if err != nil {
				return err
			}
			g.SubjectKind, g.SubjectID = source.GrantSubjectGroup, group.ID
		} else {
			if grant.Effect != source.GrantEffectDeny {
				return errors.New("access grant has no native principal or group")
			}
			g.SubjectKind = source.GrantSubjectAll
		}
		grants = append(grants, g)
	}
	kind := source.SnapshotGrants
	if membership {
		kind = source.SnapshotMembership
	}
	if err := s.observations.WithFence(ctx, state, func(tx source.Tx) error {
		if membership {
			return s.canonical.ApplyMembershipSnapshot(ctx, tx, conn.TenantID, conn.ID, revision, memberships, unmapped)
		}
		return s.canonical.ApplyAccessSnapshot(ctx, tx, conn.TenantID, conn.ID, revision, grants, unmapped)
	}); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:connector-v2:%s:%s:%d", OperationAccess, conn.ID, kind, revision)
	_, _, err = s.jobs.Enqueue(ctx, principal, jobs.EnqueueRequest{TenantID: principal.TenantID, Operation: OperationAccess, IdempotencyKey: key, Subject: jobs.Subject{ConnectionID: conn.ID, ObservedRevision: revision}, InputRef: conn.ID, InputHash: fmt.Sprint(revision), Policy: map[string]any{"connection_id": conn.ID, "snapshot_kind": kind, "revision": revision}, Lane: jobs.LaneBackground})
	if err != nil {
		return fmt.Errorf("enqueue rag.access: %w", err)
	}
	return nil
}

func (s *Service) observePrincipal(ctx context.Context, conn source.Connection, grant AccessGrant) (source.SourceIdentity, error) {
	id, err := connectorObjectID()
	if err != nil {
		return source.SourceIdentity{}, err
	}
	identity := source.SourceIdentity{TenantID: conn.TenantID, ConnectionID: conn.ID, SourceKind: conn.Kind, ID: id, NativeID: grant.NativePrincipalID, DisplayName: firstNonEmpty(grant.NativePrincipalName, grant.NativePrincipalID)}
	if err := s.canonical.ObserveSourceIdentity(ctx, identity); err != nil {
		return source.SourceIdentity{}, err
	}
	return s.canonical.ObservedSourceIdentity(ctx, conn.TenantID, conn.ID, grant.NativePrincipalID)
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
			if _, err := s.canonical.LatestObjectVersion(ctx, state.TenantID, object.ID); errors.Is(err, source.ErrDenied) {
				continue
			} else if err != nil {
				return err
			}
			if err := s.canonical.MarkObjectDeleted(ctx, state.TenantID, object.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
