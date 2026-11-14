package extract

import (
	"errors"
	"fmt"
	"strings"

	"enclava-go/internal/source"
)

var ErrInvalidChunkProfile = errors.New("extract: invalid chunk profile")

var boundaryStrategies = map[string]bool{
	"auto": true, "fixed": true, "heading": true, "clause": true, "paragraph": true,
	"message": true, "thread": true, "code_lines": true,
}

var tableHandlings = map[string]bool{"atomic": true, "row-wise": true, "skip": true}

// ValidateChunkProfile checks the controls that determine a reproducible chunk set.
func ValidateChunkProfile(profile source.ChunkProfile) error {
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidChunkProfile, reason) }
	if strings.TrimSpace(profile.ID) == "" || strings.TrimSpace(profile.ProfileID) == "" || profile.Version < 1 {
		return invalid("ID, ProfileID and positive Version are required")
	}
	if profile.SizeCodePoints < 1 {
		return invalid("size must be positive Unicode code points")
	}
	if profile.OverlapCodePoints < 0 || profile.OverlapCodePoints >= profile.SizeCodePoints {
		return invalid("overlap must be non-negative and less than size")
	}
	if !boundaryStrategies[profile.BoundaryStrategy] {
		return invalid("unsupported boundary strategy")
	}
	if !tableHandlings[profile.TableHandling] {
		return invalid("unsupported table handling")
	}
	return nil
}
