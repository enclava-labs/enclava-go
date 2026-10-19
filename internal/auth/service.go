package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	HashAlgoSHA256     = "sha256"
	HashAlgoHMACSHA256 = "hmac_sha256"
)

type APIKeyRecord struct {
	ID                      int64
	Name                    string
	KeyPrefix               string
	KeyHash                 string
	KeyHashAlgo             string
	IsActive                bool
	ExpiresAt               *time.Time
	Scopes                  []string
	AllowedModels           []string
	AllowedExtractTemplates []string
	AllowedChatbots         []string
	AllowedAgents           []string
	AllowedEndpoints        []string
	AllowedIPs              []string
	RateLimitPerMinute      *int
	RateLimitPerHour        *int
	RateLimitPerDay         *int
	IsUnlimited             bool
	BudgetLimitTokens       *int64
	BudgetLimitCents        *int64
	BudgetPeriod            string
}

type APIKeyRepository interface {
	FindCandidatesByPrefix(ctx context.Context, prefix string) ([]APIKeyRecord, error)
	TouchUsage(ctx context.Context, id int64) error
}

type Service struct {
	repo   APIKeyRepository
	pepper []byte
}

func NewService(repo APIKeyRepository, pepper string) *Service {
	return &Service{repo: repo, pepper: []byte(pepper)}
}

func ParseAPIKey(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		candidate := strings.TrimSpace(authHeader[7:])
		if candidate != "" {
			return candidate
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func (s *Service) Authenticate(ctx context.Context, plaintext string) (Principal, error) {
	if plaintext == "" {
		return Principal{}, errors.New("missing API key")
	}
	if len(plaintext) < 8 {
		return Principal{}, errors.New("invalid API key")
	}
	prefix := plaintext[:8]
	candidates, err := s.repo.FindCandidatesByPrefix(ctx, prefix)
	if err != nil {
		return Principal{}, fmt.Errorf("lookup API key: %w", err)
	}
	if len(candidates) == 0 {
		return Principal{}, errors.New("invalid API key")
	}

	hashes := make(map[string]string, 2)
	hashFor := func(algo string) (string, error) {
		if h, ok := hashes[algo]; ok {
			return h, nil
		}
		h, err := s.hashAPIKey(plaintext, algo)
		if err != nil {
			return "", err
		}
		hashes[algo] = h
		return h, nil
	}

	for _, candidate := range candidates {
		algo := strings.TrimSpace(candidate.KeyHashAlgo)
		if algo == "" {
			algo = HashAlgoSHA256
		}
		hash, err := hashFor(algo)
		if err != nil {
			// Treat unknown/unsupported hash schemes as non-matching.
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(candidate.KeyHash)) != 1 {
			continue
		}
		if !candidate.IsActive {
			return Principal{}, errors.New("API key is inactive")
		}
		if candidate.ExpiresAt != nil && time.Now().UTC().After(*candidate.ExpiresAt) {
			return Principal{}, errors.New("API key is expired")
		}

		models, modelsAll := allowlistSet(candidate.AllowedModels)
		templates, templatesAll := allowlistSet(candidate.AllowedExtractTemplates)
		chatbots, chatbotsAll := allowlistSet(candidate.AllowedChatbots)
		agents, agentsAll := allowlistSet(candidate.AllowedAgents)
		endpoints, endpointsAll := allowlistSet(candidate.AllowedEndpoints)
		ips, ipsAll := allowlistSet(candidate.AllowedIPs)
		principal := Principal{
			APIKeyID:                   candidate.ID,
			Name:                       candidate.Name,
			Scopes:                     listToSet(candidate.Scopes),
			AllowedModels:              models,
			AllowedModelsAll:           modelsAll,
			AllowedExtractTemplates:    templates,
			AllowedExtractTemplatesAll: templatesAll,
			AllowedChatbots:            chatbots,
			AllowedChatbotsAll:         chatbotsAll,
			AllowedAgents:              agents,
			AllowedAgentsAll:           agentsAll,
			AllowedEndpoints:           endpoints,
			AllowedEndpointsAll:        endpointsAll,
			AllowedIPs:                 ips,
			AllowedIPsAll:              ipsAll,
			RateLimitPerMinute:         candidate.RateLimitPerMinute,
			RateLimitPerHour:           candidate.RateLimitPerHour,
			RateLimitPerDay:            candidate.RateLimitPerDay,
			IsUnlimited:                candidate.IsUnlimited,
			BudgetLimitTokens:          candidate.BudgetLimitTokens,
			BudgetLimitCents:           candidate.BudgetLimitCents,
			BudgetPeriod:               candidate.BudgetPeriod,
			RawAPIKeyPrefix:            candidate.KeyPrefix,
		}
		_ = s.repo.TouchUsage(ctx, candidate.ID)
		return principal, nil
	}

	return Principal{}, errors.New("invalid API key")
}

func HashForStorage(plaintext, algo, pepper string) (prefix string, hash string, usedAlgo string, err error) {
	if len(plaintext) >= 8 {
		prefix = plaintext[:8]
	}
	usedAlgo = strings.TrimSpace(algo)
	if usedAlgo == "" {
		usedAlgo = HashAlgoSHA256
	}
	hash, err = hashAPIKeyWithAlgo(plaintext, usedAlgo, []byte(pepper))
	return prefix, hash, usedAlgo, err
}

func (s *Service) hashAPIKey(key, algo string) (string, error) {
	return hashAPIKeyWithAlgo(key, algo, s.pepper)
}

func hashAPIKeyWithAlgo(key, algo string, pepper []byte) (string, error) {
	switch algo {
	case "", HashAlgoSHA256:
		sum := sha256.Sum256([]byte(key))
		return hex.EncodeToString(sum[:]), nil
	case HashAlgoHMACSHA256:
		if len(pepper) == 0 {
			return "", errors.New("API key pepper not configured")
		}
		mac := hmac.New(sha256.New, pepper)
		_, _ = mac.Write([]byte(key))
		return hex.EncodeToString(mac.Sum(nil)), nil
	default:
		return "", fmt.Errorf("unknown API key hash algo: %s", algo)
	}
}

func listToSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}
