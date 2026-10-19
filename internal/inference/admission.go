// admission.go bounds the executor's process-local concurrency (CON-01).
// Upstream dispatch capacity, lifecycle management capacity and internal
// fan-out are three separate pools so that a saturated request queue can
// never starve cancellation/read/delete calls, and nested/background model
// work can never deadlock an outer permit. Queues are FIFO-bounded: overload
// rejects explicitly rather than queueing forever.
package inference

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// ErrOverloaded is the explicit overload outcome: every admission queue is
// bounded, so excess callers reject immediately instead of waiting forever
// or being retried elsewhere.
var ErrOverloaded = errors.New("inference: admission queue full")

// AdmissionClass selects the bounded pool an Acquire call draws from.
type AdmissionClass int

const (
	// AdmissionExecute bounds priced upstream dispatch — one slot per
	// dispatched attempt, held until the response/stream body closes.
	AdmissionExecute AdmissionClass = iota
	// AdmissionManage bounds unpriced lifecycle follow-ups (retrieve,
	// cancel, delete, input_items, stream-resume). It is a separate pool so
	// control operations progress while every execute slot is saturated.
	AdmissionManage
	// AdmissionFanOut bounds internal/background model calls (job-runner
	// attempts). Keeping them out of the request pool means nested work
	// can never deadlock an outer dispatch permit.
	AdmissionFanOut
)

// ClassLimit configures one admission pool. PerTenant <= 0 means unlimited;
// QueueTimeout <= 0 means waiters are bounded only by the caller context.
type ClassLimit struct {
	MaxActive    int
	MaxQueued    int
	PerTenant    int
	QueueTimeout time.Duration
}

// AdmissionConfig wires the three pools. A zero AdmissionConfig takes
// DefaultAdmissionConfig.
type AdmissionConfig struct {
	Execute ClassLimit
	Manage  ClassLimit
	FanOut  ClassLimit
}

// DefaultAdmissionConfig is the production bound set: explicit rejection
// under overload instead of resource exhaustion.
var DefaultAdmissionConfig = AdmissionConfig{
	Execute: ClassLimit{MaxActive: 32, MaxQueued: 128},
	Manage:  ClassLimit{MaxActive: 8, MaxQueued: 64},
	FanOut:  ClassLimit{MaxActive: 8, MaxQueued: 256},
}

// waiter is one queued Acquire caller. granted is set under the gate mutex
// when the slot is handed off, so a racing cancellation can tell "abandoned
// while queued" from "granted, must release".
type waiter struct {
	tenant  string
	granted bool
	wake    chan struct{}
}

type admissionPool struct {
	owner   *Admission
	limit   ClassLimit
	active  int
	queued  int
	tenants map[string]int // active + queued occupancy per tenant
	waiters *list.List     // FIFO of *waiter
}

// Admission is the per-process bounded-concurrency gate. Its limits are
// local to this process only — shared tenant/accounting admission is
// enforced authoritatively by PostgreSQL reservations (CON-02); summing
// per-process limits across processes is not a shared cap.
type Admission struct {
	mu    sync.Mutex
	pools [3]*admissionPool
}

// NewAdmission builds the gate; a zero AdmissionConfig takes the defaults.
func NewAdmission(cfg AdmissionConfig) *Admission {
	if cfg == (AdmissionConfig{}) {
		cfg = DefaultAdmissionConfig
	}
	a := &Admission{}
	cfgOf := [3]ClassLimit{cfg.Execute, cfg.Manage, cfg.FanOut}
	for i := range a.pools {
		l := cfgOf[i]
		if l.MaxActive <= 0 {
			l.MaxActive = 1
		}
		a.pools[i] = &admissionPool{owner: a, limit: l, tenants: map[string]int{}, waiters: list.New()}
	}
	return a
}

// Permit is a held admission slot. Release is idempotent and must run
// exactly once per successful Acquire — for streams it runs on Body.Close,
// so a permit lives until the body actually closes, not when headers return.
type Permit struct {
	pool   *admissionPool
	tenant string
	once   sync.Once
}

// Release frees the slot and grants waiting callers their turn.
func (p *Permit) Release() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		a := p.pool.owner
		a.mu.Lock()
		pool := p.pool
		pool.active--
		a.grantLocked(pool)
		a.mu.Unlock()
	})
}

func (p *Permit) decTenantLocked(pool *admissionPool) {
	pool.tenants[p.tenant]--
	if pool.tenants[p.tenant] <= 0 {
		delete(pool.tenants, p.tenant)
	}
}

// Acquire takes one slot for tenant in class cls, waiting FIFO while the
// pool is full. Cancellation abandons the queue entry and frees its tenant
// occupancy. Returns ErrOverloaded when the queue is full or the tenant
// sublimit is reached.
func (a *Admission) Acquire(ctx context.Context, cls AdmissionClass, tenant string) (*Permit, error) {
	pool := a.pools[cls]
	if pool.limit.QueueTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pool.limit.QueueTimeout)
		defer cancel()
	}

	a.mu.Lock()
	tenantFull := pool.limit.PerTenant > 0 && pool.tenants[tenant] >= pool.limit.PerTenant
	if pool.active < pool.limit.MaxActive && pool.waiters.Len() == 0 && !tenantFull {
		pool.active++
		pool.tenants[tenant]++
		a.mu.Unlock()
		return &Permit{pool: pool, tenant: tenant}, nil
	}
	atQueue := pool.limit.MaxQueued <= 0 || pool.queued >= pool.limit.MaxQueued
	if tenantFull || atQueue {
		a.mu.Unlock()
		return nil, ErrOverloaded
	}
	pool.queued++
	pool.tenants[tenant]++
	w := &waiter{tenant: tenant, wake: make(chan struct{}, 1)}
	el := pool.waiters.PushBack(w)
	a.mu.Unlock()

	select {
	case <-w.wake:
		return &Permit{pool: pool, tenant: tenant}, nil
	case <-ctx.Done():
		a.mu.Lock()
		if w.granted {
			// The slot was handed off before the cancellation was
			// observed: drop it and serve the next waiter.
			pool.active--
		} else {
			pool.waiters.Remove(el)
			pool.queued--
		}
		pool.tenants[tenant]--
		if pool.tenants[tenant] <= 0 {
			delete(pool.tenants, tenant)
		}
		a.grantLocked(pool)
		a.mu.Unlock()
		return nil, ctx.Err()
	}
}

// grantLocked hands freed capacity to head-of-line waiters for pool. Called
// with a.mu held. The granted waiter converts queued occupancy into the
// active slot; its wake is buffered so the send never blocks.
func (a *Admission) grantLocked(pool *admissionPool) {
	for pool.active < pool.limit.MaxActive && pool.waiters.Len() > 0 {
		el := pool.waiters.Front()
		w := el.Value.(*waiter)
		pool.waiters.Remove(el)
		pool.queued--
		pool.active++
		w.granted = true
		w.wake <- struct{}{}
	}
}

// Stats returns the pool's live counters — tests observe bounds through
// these; it is not a metrics surface.
func (a *Admission) Stats(cls AdmissionClass) (active, queued, tenants int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pools[cls]
	return p.active, p.queued, len(p.tenants)
}
