// Package canary — gradual rollout with automatic rollback.
//
// Go port of canary.py (the Python implementation stays as the reference).
// Routes a percentage of traffic to a candidate backend, watches error rate
// and latency against the incumbent, ramps 1% -> 5% -> 25% -> 50% -> 100%,
// and snaps back to the incumbent when the candidate crosses a failure
// threshold.
package canary

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// RampSteps is the default rollout curve.
var RampSteps = []int{1, 5, 25, 50, 100}

// Thresholds gate rollback decisions.
type Thresholds struct {
	MaxErrorRate    float64 // candidate error rate vs incumbent
	MaxLatencyRatio float64 // candidate p50 latency / incumbent p50
	MinSamples      int     // samples per backend before judging
}

// DefaultThresholds mirrors DEFAULT_THRESHOLDS in canary.py.
var DefaultThresholds = Thresholds{
	MaxErrorRate:    0.05,
	MaxLatencyRatio: 1.5,
	MinSamples:      20,
}

// Sample is one observed request result.
type Sample struct {
	OK      bool
	Latency float64 // seconds
	Backend string  // "incumbent" | "candidate"
	TS      time.Time
}

// Stats summarizes one backend's samples.
type Stats struct {
	N         int     `json:"n"`
	ErrorRate float64 `json:"error_rate"`
	P50       float64 `json:"p50"`
}

// State is a point-in-time snapshot of the controller.
type State struct {
	Pct        int         `json:"pct"`
	Status     string      `json:"status"`
	Candidate  Stats       `json:"candidate"`
	Incumbent  Stats       `json:"incumbent"`
	Rollbacks  int         `json:"rollbacks"`
	Thresholds Thresholds  `json:"thresholds"`
}

// Canary is a gradual rollout controller with deterministic routing and
// auto-rollback. Safe for concurrent use.
type Canary struct {
	mu          sync.Mutex
	incumbentFn func() (interface{}, error)
	candidateFn func() (interface{}, error)
	thresholds  Thresholds
	rampSteps   []int

	pct                 int
	status              string // idle | ramping | healthy | rolled_back | complete
	samples             []Sample
	rollbacks           int
	lastReason          string
	samplesAtLastRamp   int
}

// New creates a Canary. Pass nil for thresholds or rampSteps to use defaults.
func New(incumbentFn, candidateFn func() (interface{}, error), thresholds *Thresholds, rampSteps []int) *Canary {
	c := &Canary{
		incumbentFn: incumbentFn,
		candidateFn: candidateFn,
		thresholds:  DefaultThresholds,
		rampSteps:   append([]int(nil), RampSteps...),
		status:      "idle",
	}
	if thresholds != nil {
		c.thresholds = *thresholds
	}
	if len(rampSteps) > 0 {
		c.rampSteps = append([]int(nil), rampSteps...)
	}
	return c
}

// ---------------------------------------------------------------- routing

// PickCandidate reports whether this key should go to the candidate.
// Deterministic: the same key always maps to the same backend at a given
// percentage, so tests (and sticky clients) are reproducible.
func (c *Canary) PickCandidate(key string) bool {
	c.mu.Lock()
	pct := c.pct
	c.mu.Unlock()
	if pct >= 100 {
		return true
	}
	if pct <= 0 {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	h := binary.BigEndian.Uint64(sum[:8])
	return h%10000 < uint64(pct)*100
}

// Route executes the chosen backend for this request and records the sample.
// The backend's error is returned after being recorded.
func (c *Canary) Route(key string) (interface{}, error) {
	useCandidate := c.PickCandidate(key)
	backend := "incumbent"
	if useCandidate {
		backend = "candidate"
	}
	fn := c.incumbentFn
	if useCandidate {
		fn = c.candidateFn
	}
	start := time.Now()
	result, err := fn()
	latency := time.Since(start).Seconds()
	c.RecordResult(backend, err == nil, latency)
	return result, err
}

// ----------------------------------------------------------------- record

// RecordResult records one sample and re-evaluates thresholds.
func (c *Canary) RecordResult(backend string, ok bool, latency float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, Sample{OK: ok, Latency: latency, Backend: backend, TS: time.Now()})
	c.evaluate()
}

// OnResult is an alias for RecordResult (external instrumentation).
func (c *Canary) OnResult(backend string, ok bool, latency float64) {
	c.RecordResult(backend, ok, latency)
}

// ----------------------------------------------------------------- stats

func (c *Canary) stats(backend string) Stats {
	var s []Sample
	for _, x := range c.samples {
		if x.Backend == backend {
			s = append(s, x)
		}
	}
	n := len(s)
	if n == 0 {
		return Stats{}
	}
	lats := make([]float64, n)
	errs := 0
	for i, x := range s {
		lats[i] = x.Latency
		if !x.OK {
			errs++
		}
	}
	sortFloats(lats)
	return Stats{N: n, ErrorRate: float64(errs) / float64(n), P50: lats[n/2]}
}

func sortFloats(a []float64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (c *Canary) evaluate() {
	if c.status == "rolled_back" || c.status == "complete" {
		return
	}
	cand := c.stats("candidate")
	inc := c.stats("incumbent")
	if cand.N < c.thresholds.MinSamples {
		return
	}
	ratio := 1.0
	if inc.N > 0 && inc.P50 > 0 {
		ratio = cand.P50 / inc.P50
	}
	if cand.ErrorRate > c.thresholds.MaxErrorRate || ratio > c.thresholds.MaxLatencyRatio {
		c.rollback("threshold breach")
		return
	}
	// Ramp at most once per min_samples window so a stream of healthy
	// results doesn't blow through the whole curve in one burst.
	if len(c.samples)-c.samplesAtLastRamp >= c.thresholds.MinSamples {
		if c.maybeRamp() {
			c.samplesAtLastRamp = len(c.samples)
		}
	}
}

// ------------------------------------------------------------------ ramp

// MaybeRamp advances to the next ramp step if the candidate is healthy.
func (c *Canary) MaybeRamp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maybeRamp()
}

func (c *Canary) maybeRamp() bool {
	if c.status == "rolled_back" || c.status == "complete" {
		return false
	}
	idx := -1
	for i, s := range c.rampSteps {
		if s == c.pct {
			idx = i
			break
		}
	}
	if idx+1 < len(c.rampSteps) {
		c.pct = c.rampSteps[idx+1]
		if c.pct < c.rampSteps[len(c.rampSteps)-1] {
			c.status = "ramping"
		} else {
			c.status = "complete"
		}
		return true
	}
	return false
}

// Rollback snaps back to the incumbent immediately.
func (c *Canary) Rollback(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollback(reason)
}

func (c *Canary) rollback(reason string) {
	c.pct = 0
	c.status = "rolled_back"
	c.rollbacks++
	c.lastReason = reason
}

// Promote manually sends 100% to the candidate.
func (c *Canary) Promote() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pct = 100
	c.status = "complete"
}

// ----------------------------------------------------------------- state

// State returns a point-in-time snapshot.
func (c *Canary) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return State{
		Pct:        c.pct,
		Status:     c.status,
		Candidate:  c.stats("candidate"),
		Incumbent:  c.stats("incumbent"),
		Rollbacks:  c.rollbacks,
		Thresholds: c.thresholds,
	}
}

// Pct returns the current rollout percentage.
func (c *Canary) Pct() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pct
}

// SetPct forces the rollout percentage (tests, manual control).
func (c *Canary) SetPct(p int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pct = p
}

// Status returns the current status string.
func (c *Canary) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Rollbacks returns the rollback count.
func (c *Canary) Rollbacks() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rollbacks
}

// ErrNoSteps is returned by helpers that expect a ramp curve.
var ErrNoSteps = errors.New("no ramp steps configured")
