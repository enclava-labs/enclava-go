package extract

import (
	"errors"
	"testing"

	"enclava-go/internal/source"
)

func TestValidateChunkProfile(t *testing.T) {
	base := source.ChunkProfile{
		ID: "cpv_1", ProfileID: "legal", Version: 1, SizeCodePoints: 100,
		BoundaryStrategy: "clause", TableHandling: "atomic",
	}
	tests := []struct {
		name    string
		change  func(*source.ChunkProfile)
		wantErr bool
	}{
		{name: "valid", change: func(*source.ChunkProfile) {}},
		{name: "zero size", change: func(p *source.ChunkProfile) { p.SizeCodePoints = 0 }, wantErr: true},
		{name: "overlap equals size", change: func(p *source.ChunkProfile) { p.OverlapCodePoints = 100 }, wantErr: true},
		{name: "negative overlap", change: func(p *source.ChunkProfile) { p.OverlapCodePoints = -1 }, wantErr: true},
		{name: "invalid boundary", change: func(p *source.ChunkProfile) { p.BoundaryStrategy = "ontology" }, wantErr: true},
		{name: "invalid table handling", change: func(p *source.ChunkProfile) { p.TableHandling = "guess" }, wantErr: true},
		{name: "version required", change: func(p *source.ChunkProfile) { p.Version = 0 }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := base
			tt.change(&profile)
			err := ValidateChunkProfile(profile)
			if tt.wantErr && !errors.Is(err, ErrInvalidChunkProfile) {
				t.Fatalf("expected profile validation error, got %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
