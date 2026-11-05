package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/jobs"
	"enclava-go/internal/source"
)

type ConnectorObservation = jobs.ConnectorObservation

type ConnectorStream = jobs.ConnectorStreamCheckpoint

type ConnectorObservationsRepo struct{ db *DB }

func NewConnectorObservationsRepo(db *DB) *ConnectorObservationsRepo {
	return &ConnectorObservationsRepo{db: db}
}

func (r *ConnectorObservationsRepo) StartRun(ctx context.Context, tenantID, connectionID, stream, runID, configHash string) (ConnectorStream, error) {
	var out ConnectorStream
	err := r.db.SQL.QueryRowContext(ctx, `
		INSERT INTO connector_streams (tenant_id, connection_id, stream, run_id, fence, cursor, observed_seq, run_start_seq, complete, incomplete, last_error, config_hash)
		VALUES ($1, $2, $3, $4, 1, '', 0, 0, false, false, '', $5)
		ON CONFLICT (tenant_id, connection_id, stream) DO UPDATE SET
			run_id = EXCLUDED.run_id, fence = connector_streams.fence + 1,
			cursor = CASE WHEN connector_streams.config_hash <> EXCLUDED.config_hash THEN '' ELSE connector_streams.cursor END,
			run_start_seq = CASE WHEN connector_streams.complete OR connector_streams.config_hash <> EXCLUDED.config_hash THEN connector_streams.observed_seq ELSE connector_streams.run_start_seq END,
			observed_seq = connector_streams.observed_seq, complete = false,
			incomplete = false, last_error = '', config_hash = EXCLUDED.config_hash, updated_at = now()
		RETURNING tenant_id::text, connection_id::text, stream, run_id::text, fence, cursor, observed_seq, run_start_seq, complete, incomplete, last_error`,
		tenantID, connectionID, stream, runID, configHash).Scan(&out.TenantID, &out.ConnectionID, &out.Stream, &out.RunID, &out.Fence, &out.Cursor, &out.ObservedSeq, &out.RunStartSeq, &out.Complete, &out.Incomplete, &out.LastError)
	if err != nil {
		return ConnectorStream{}, fmt.Errorf("start connector stream: %w", err)
	}
	return out, nil
}

// PersistPage writes observations and the next cursor in one fenced transaction.
func (r *ConnectorObservationsRepo) PersistPage(ctx context.Context, state ConnectorStream, observations []ConnectorObservation, next string, complete, incomplete bool, pageErr string) (ConnectorStream, error) {
	tx, err := r.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return ConnectorStream{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT observed_seq FROM connector_streams WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3 AND run_id=$4 AND fence=$5 FOR UPDATE`, state.TenantID, state.ConnectionID, state.Stream, state.RunID, state.Fence).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectorStream{}, source.ErrStreamSuperseded
	}
	if err != nil {
		return ConnectorStream{}, err
	}
	for _, observation := range observations {
		if observation.NativeID == "" {
			return ConnectorStream{}, errors.New("connector observation native id required")
		}
		payload := observation.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		if !json.Valid(payload) {
			return ConnectorStream{}, errors.New("connector observation payload is invalid JSON")
		}
		var artifactRef any
		if observation.ArtifactRef.ID != "" {
			artifactRef, err = json.Marshal(observation.ArtifactRef)
			if err != nil {
				return ConnectorStream{}, fmt.Errorf("marshal connector artifact ref: %w", err)
			}
		}
		current++
		_, err = tx.ExecContext(ctx, `INSERT INTO connector_observations (tenant_id,connection_id,stream,run_id,observed_seq,native_id,object_id,source_observed_seq,payload,artifact_ref,processing_error) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10,$11)`,
			state.TenantID, state.ConnectionID, state.Stream, state.RunID, current, observation.NativeID, observation.ObjectRef.ObjectID, observation.ObjectRef.ObservedSeq, []byte(payload), artifactRef, observation.ProcessingError)
		if err != nil {
			return ConnectorStream{}, fmt.Errorf("persist connector observation: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE connector_streams SET cursor=$6, observed_seq=$7, complete=$8, incomplete=$9, last_error=$10, updated_at=now() WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3 AND run_id=$4 AND fence=$5`, state.TenantID, state.ConnectionID, state.Stream, state.RunID, state.Fence, next, current, complete, incomplete, pageErr)
	if err != nil {
		return ConnectorStream{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ConnectorStream{}, err
	}
	if n != 1 {
		return ConnectorStream{}, source.ErrStreamSuperseded
	}
	if err := tx.Commit(); err != nil {
		return ConnectorStream{}, err
	}
	state.Cursor, state.ObservedSeq, state.Complete, state.Incomplete, state.LastError = next, current, complete, incomplete, pageErr
	return state, nil
}

func (r *ConnectorObservationsRepo) GetStream(ctx context.Context, tenantID, connectionID, stream string) (ConnectorStream, error) {
	var out ConnectorStream
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,connection_id::text,stream,run_id::text,fence,cursor,observed_seq,run_start_seq,complete,incomplete,last_error FROM connector_streams WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3`, tenantID, connectionID, stream).Scan(&out.TenantID, &out.ConnectionID, &out.Stream, &out.RunID, &out.Fence, &out.Cursor, &out.ObservedSeq, &out.RunStartSeq, &out.Complete, &out.Incomplete, &out.LastError)
	if err != nil {
		return ConnectorStream{}, err
	}
	return out, nil
}

func (r *ConnectorObservationsRepo) ListObservations(ctx context.Context, state ConnectorStream) ([]ConnectorObservation, error) {
	rows, err := r.db.SQL.QueryContext(ctx, `SELECT tenant_id::text,connection_id::text,stream,run_id,observed_seq,native_id,object_id,source_observed_seq,payload,artifact_ref,processing_error FROM connector_observations WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3 AND observed_seq>$4 ORDER BY observed_seq`,
		state.TenantID, state.ConnectionID, state.Stream, state.RunStartSeq)
	if err != nil {
		return nil, fmt.Errorf("list connector observations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConnectorObservation
	for rows.Next() {
		var item ConnectorObservation
		var objectID sql.NullString
		var artifactRef []byte
		if err := rows.Scan(&item.TenantID, &item.ConnectionID, &item.Stream, &item.RunID, &item.ObservedSeq, &item.NativeID, &objectID, &item.ObjectRef.ObservedSeq, &item.Payload, &artifactRef, &item.ProcessingError); err != nil {
			return nil, fmt.Errorf("scan connector observation: %w", err)
		}
		item.ObjectRef.TenantID = item.TenantID
		item.ObjectRef.ObjectID = objectID.String
		if len(artifactRef) > 0 {
			if err := json.Unmarshal(artifactRef, &item.ArtifactRef); err != nil {
				return nil, fmt.Errorf("decode connector artifact ref: %w", err)
			}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *ConnectorObservationsRepo) WithFence(ctx context.Context, state ConnectorStream, fn func(source.Tx) error) error {
	return WithTx(ctx, r.db.SQL, func(tx *sql.Tx) error {
		var current int64
		err := tx.QueryRowContext(ctx, `SELECT fence FROM connector_streams WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3 AND run_id=$4 FOR UPDATE`, state.TenantID, state.ConnectionID, state.Stream, state.RunID).Scan(&current)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return source.ErrStreamSuperseded
			}
			return err
		}
		if current != state.Fence {
			return source.ErrStreamSuperseded
		}
		return fn(tx)
	})
}
