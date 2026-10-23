package rag

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"enclava-go/internal/auth"
)

type ContextAuthorizer interface {
	CanReadObject(context.Context, auth.Principal, string) bool
}

type ContextResult struct {
	Text             string `json:"text"`
	CharacterLimit   int    `json:"character_limit"`
	Incomplete       bool   `json:"incomplete"`
	NeighborWithheld bool   `json:"neighbor_withheld"`
}

// BuildContext preserves distinct clauses and counts Unicode code points, not bytes or tokens.
func BuildContext(ctx context.Context, p auth.Principal, candidates []SearchResult, limit int, authz ContextAuthorizer) ContextResult {
	out := ContextResult{CharacterLimit: limit}
	if limit <= 0 { out.Incomplete = len(candidates) > 0; return out }
	seen := map[string]struct{}{}
	var parts []string
	count := 0
	for _, hit := range candidates {
		if authz == nil || !authz.CanReadObject(ctx, p, hit.DocumentID) {
			out.NeighborWithheld = true
			continue
		}
		if len(hit.Spans) != 0 {
			span := hit.Spans[0]
			key := span.Ref.TenantID + "\x00" + span.Ref.ObjectID + "\x00" + itoa64(span.Ref.ObservedSeq) + "\x00" + span.BlockID + "\x00" + itoa64(hit.Generation) + "\x00" + itoa(span.ByteStart) + ":" + itoa(span.ByteEnd)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		text := strings.TrimSpace(hit.Content)
		if text == "" {
			continue
		}
		label := hit.EvidenceID
		if label == "" {
			label = hit.ChunkID
		}
		piece := "[" + label + "] " + text
		separator := 0
		if len(parts) > 0 { separator = 1 }
		remaining := limit - count - separator
		runes := []rune(piece)
		if remaining <= 0 { out.Incomplete = true; break }
		if len(runes) > remaining { piece = string(runes[:remaining]); out.Incomplete = true }
		if separator > 0 { parts = append(parts, "\n"); count++ }
		parts = append(parts, piece)
		count += utf8.RuneCountInString(piece)
		if out.Incomplete { break }
	}
	out.Text = strings.Join(parts, "")
	if out.NeighborWithheld { marker := "[neighbor withheld]"; if utf8.RuneCountInString(out.Text)+1+utf8.RuneCountInString(marker) <= limit { if out.Text != "" { out.Text += "\n" }; out.Text += marker } else { out.Incomplete = true } }
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
