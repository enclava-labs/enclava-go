package llm

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"
)

type ReadinessSnapshot struct {
	Ready               bool      `json:"ready"`
	LastProbeAt         time.Time `json:"last_probe_at"`
	LastSuccessAt       time.Time `json:"last_success_at"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
}

// Prober continuously checks whether the configured provider is usable.
// In Kubernetes, use this for readiness (`/readyz`) rather than blocking startup.
type Prober struct {
	provider Provider
	interval time.Duration
	timeout  time.Duration

	mu   sync.RWMutex
	snap ReadinessSnapshot
}

func NewProber(provider Provider, interval, timeout time.Duration) *Prober {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Prober{
		provider: provider,
		interval: interval,
		timeout:  timeout,
		snap: ReadinessSnapshot{
			Ready:               false,
			ConsecutiveFailures: 0,
		},
	}
}

func (p *Prober) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snap.Ready
}

func (p *Prober) Snapshot() ReadinessSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snap
}

func (p *Prober) Start(ctx context.Context) {
	// Probe immediately, then on an interval with small jitter to avoid stampedes.
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("prober: recovered from panic: %v", r)
				p.mu.Lock()
				p.snap.Ready = false
				p.snap.LastError = fmt.Sprintf("panic: %v", r)
				p.mu.Unlock()
			}
		}()
		p.probeOnce(ctx)
	}()

	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("prober: recovered from panic: %v", r)
						p.mu.Lock()
						p.snap.Ready = false
						p.snap.LastError = fmt.Sprintf("panic: %v", r)
						p.mu.Unlock()
					}
				}()
				p.probeOnce(ctx)
			}()
		}
	}
}

func (p *Prober) probeOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()

	err := ProbeProvider(ctx, p.provider)
	now := time.Now().UTC()

	if err != nil {
		// Small jitter so repeated failures don't synchronize across replicas.
		delay, err := rand.Int(rand.Reader, big.NewInt(250))
		if err != nil {
			delay = big.NewInt(0)
		}
		select {
		case <-time.After(time.Duration(delay.Int64()) * time.Millisecond):
		case <-parent.Done():
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.snap.LastProbeAt = now
	if err == nil {
		p.snap.Ready = true
		p.snap.LastSuccessAt = now
		p.snap.ConsecutiveFailures = 0
		p.snap.LastError = ""
		return
	}

	p.snap.Ready = false
	p.snap.ConsecutiveFailures++
	p.snap.LastError = err.Error()
}

// ProbeProvider determines if the provider is usable right now.
// For attested providers (tinfoil), it verifies attestation and then checks list-models.
func ProbeProvider(ctx context.Context, provider Provider) error {
	if attested, ok := provider.(AttestationProvider); ok {
		if _, err := attested.VerifyAttestation(ctx); err != nil {
			return err
		}
	}
	if health, ok := provider.(HealthProvider); ok {
		return health.Health(ctx)
	}
	_, err := provider.ListModels(ctx)
	return err
}
