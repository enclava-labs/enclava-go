package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"enclava-go/internal/artifacts"
)

type ConnectorObservation struct {
	NativeID        string
	ObjectID        string
	Payload         json.RawMessage
	ArtifactRef     artifacts.Ref
	ProcessingError string
}

type ConnectorStream struct {
	TenantID, ConnectionID string
	Stream                 string
	RunID                  string
	Fence                  int64
	Cursor                 string
	ObservedSeq            int64
	Complete, Incomplete   bool
	LastError              string
}

type ConnectorObservationsRepo struct{ db *DB }

func NewConnectorObservationsRepo(db *DB) *ConnectorObservationsRepo {
	return &ConnectorObservationsRepo{db: db}
}

func (r *ConnectorObservationsRepo) StartRun(ctx context.Context, tenantID, connectionID, stream, runID string) (ConnectorStream, error) {
	var out ConnectorStream
	err := r.db.SQL.QueryRowContext(ctx, `
		INSERT INTO connector_streams (tenant_id, connection_id, stream, run_id, fence, cursor, observed_seq, complete, incomplete, last_error)
		VALUES ($1, $2, $3, $4, 1, '', 0, false, false, '')
		ON CONFLICT (tenant_id, connection_id, stream) DO UPDATE SET
			run_id = EXCLUDED.run_id, fence = connector_streams.fence + 1,
			cursor = CASE WHEN connector_streams.complete THEN '' ELSE connector_streams.cursor END,
			observed_seq = connector_streams.observed_seq, complete = false,
			incomplete = false, last_error = '', updated_at = now()
		RETURNING tenant_id::text, connection_id::text, stream, run_id::text, fence, cursor, observed_seq, complete, incomplete, last_error`,
		tenantID, connectionID, stream, runID).Scan(&out.TenantID, &out.ConnectionID, &out.Stream, &out.RunID, &out.Fence, &out.Cursor, &out.ObservedSeq, &out.Complete, &out.Incomplete, &out.LastError)
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
		return ConnectorStream{}, errors.New("connector stream fence lost")
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
		_, err = tx.ExecContext(ctx, `INSERT INTO connector_observations (tenant_id,connection_id,stream,run_id,observed_seq,native_id,object_id,payload,artifact_ref,processing_error) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9,$10)`,
			state.TenantID, state.ConnectionID, state.Stream, state.RunID, current, observation.NativeID, observation.ObjectID, []byte(payload), artifactRef, observation.ProcessingError)
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
		return ConnectorStream{}, errors.New("connector stream fence lost")
	}
	if err := tx.Commit(); err != nil {
		return ConnectorStream{}, err
	}
	state.Cursor, state.ObservedSeq, state.Complete, state.Incomplete, state.LastError = next, current, complete, incomplete, pageErr
	return state, nil
}

func (r *ConnectorObservationsRepo) GetStream(ctx context.Context, tenantID, connectionID, stream string) (ConnectorStream, error) {
	var out ConnectorStream
	err := r.db.SQL.QueryRowContext(ctx, `SELECT tenant_id::text,connection_id::text,stream,run_id::text,fence,cursor,observed_seq,complete,incomplete,last_error FROM connector_streams WHERE tenant_id=$1 AND connection_id=$2 AND stream=$3`, tenantID, connectionID, string(stream)).Scan(&out.TenantID, &out.ConnectionID, &out.Stream, &out.RunID, &out.Fence, &out.Cursor, &out.ObservedSeq, &out.Complete, &out.Incomplete, &out.LastError)
	if err != nil {
		return ConnectorStream{}, err
	}
	return out, nil
}
