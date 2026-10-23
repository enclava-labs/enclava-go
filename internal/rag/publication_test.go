package rag

import (
	"context"
	"testing"
)

type reconciliationPoint struct {
	version    string
	generation int64
}

type reconciliationStore struct {
	points []reconciliationPoint
}

func (s *reconciliationStore) DeleteGenerationPoints(_ context.Context, _ string, version string, generation int64) error {
	kept := s.points[:0]
	for _, point := range s.points {
		if point.version != version || point.generation != generation {
			kept = append(kept, point)
		}
	}
	s.points = kept
	return nil
}

func TestDeleteReconciledGenerationPreservesCurrentPoints(t *testing.T) {
	store := &reconciliationStore{points: []reconciliationPoint{
		{version: "doc#1", generation: 1},
		{version: "doc#1", generation: 1},
		{version: "doc#1", generation: 2},
	}}
	orphan := IndexReconciliation{QdrantCollection: "q", ObjectVersionID: "doc#1", Generation: 1}
	if err := deleteReconciledGeneration(context.Background(), store, orphan, false); err != nil {
		t.Fatal(err)
	}
	if len(store.points) != 1 || store.points[0].generation != 2 {
		t.Fatalf("orphan cleanup left %#v", store.points)
	}
	current := orphan
	current.Generation = 2
	if err := deleteReconciledGeneration(context.Background(), store, current, true); err == nil {
		t.Fatal("published generation deletion was allowed")
	}
	if len(store.points) != 1 || store.points[0].generation != 2 {
		t.Fatalf("published points were removed: %#v", store.points)
	}
}
