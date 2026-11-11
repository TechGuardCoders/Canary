package canary

import (
	"errors"
	"testing"
)

func makeCanary(t *testing.T) *Canary {
	t.Helper()
	return New(
		func() (interface{}, error) { return "inc", nil },
		func() (interface{}, error) { return "cand", nil },
		nil, nil,
	)
}

func feed(c *Canary, backend string, ok bool, latency float64, n int) {
	for i := 0; i < n; i++ {
		c.RecordResult(backend, ok, latency)
	}
}

// ---------------------------------------------------------------- routing

func TestPctZeroNeverRoutesCandidate(t *testing.T) {
	c := makeCanary(t)
	for i := 0; i < 200; i++ {
		if c.PickCandidate(mapKey(i)) {
			t.Fatalf("key-%d routed to candidate at pct 0", i)
		}
	}
}

func TestPctHundredAlwaysRoutesCandidate(t *testing.T) {
	c := makeCanary(t)
	c.Promote()
	for i := 0; i < 200; i++ {
		if !c.PickCandidate(mapKey(i)) {
			t.Fatalf("key-%d not routed to candidate at pct 100", i)
		}
	}
}

func TestRoutingDeterministicPerKey(t *testing.T) {
	c := makeCanary(t)
	c.SetPct(50)
	if c.PickCandidate("user-42") != c.PickCandidate("user-42") {
		t.Fatal("same key mapped to different backends")
	}
}

func TestRoutingApproximatesPercentage(t *testing.T) {
	c := makeCanary(t)
	c.SetPct(25)
	hits := 0
	for i := 0; i < 10_000; i++ {
		if c.PickCandidate(mapKey(i)) {
			hits++
		}
	}
	if hits <= 2_000 || hits >= 3_000 { // ~25% ± 5pts
		t.Fatalf("hits = %d, want 2000 < hits < 3000", hits)
	}
}

func TestRouteExecutesChosenBackend(t *testing.T) {
	c := makeCanary(t)
	r, err := c.Route("k")
	if err != nil || r != "inc" { // pct 0 -> incumbent
		t.Fatalf("got %v, %v", r, err)
	}
	c.Promote()
	r, err = c.Route("k")
	if err != nil || r != "cand" {
		t.Fatalf("got %v, %v", r, err)
	}
}

func TestRouteRecordsSample(t *testing.T) {
	c := makeCanary(t)
	c.Route("k")
	st := c.State()
	if st.Incumbent.N != 1 {
		t.Fatalf("incumbent samples = %d, want 1", st.Incumbent.N)
	}
}

func TestRouteRecordsFailureAndReraises(t *testing.T) {
	boom := func() (interface{}, error) { return nil, errors.New("x") }
	c := New(
		func() (interface{}, error) { return "inc", nil },
		boom, nil, nil,
	)
	c.SetPct(100) // force candidate routing
	if _, err := c.Route("k"); err == nil {
		t.Fatal("expected error to be re-raised")
	}
	st := c.State()
	if st.Candidate.N != 1 {
		t.Fatalf("candidate samples = %d, want 1", st.Candidate.N)
	}
}

// ------------------------------------------------------------------ ramping

func TestRampStepsDefault(t *testing.T) {
	c := makeCanary(t)
	if c.Pct() != 0 {
		t.Fatal("initial pct != 0")
	}
	if !c.MaybeRamp() || c.Pct() != 1 {
		t.Fatalf("first ramp -> %d", c.Pct())
	}
	for i := 0; i < 4; i++ {
		c.MaybeRamp()
	}
	if c.Pct() != 100 || c.Status() != "complete" {
		t.Fatalf("pct=%d status=%s", c.Pct(), c.Status())
	}
	if c.MaybeRamp() {
		t.Fatal("ramped past final step")
	}
}

func TestCustomRampSteps(t *testing.T) {
	c := New(
		func() (interface{}, error) { return "inc", nil },
		func() (interface{}, error) { return "cand", nil },
		nil, []int{10, 50},
	)
	c.MaybeRamp()
	if c.Pct() != 10 {
		t.Fatalf("pct = %d, want 10", c.Pct())
	}
	c.MaybeRamp()
	if c.Pct() != 50 || c.Status() != "complete" {
		t.Fatalf("pct=%d status=%s", c.Pct(), c.Status())
	}
}

// ----------------------------------------------------------------- rollback

func TestRollbackOnErrorRateBreach(t *testing.T) {
	c := makeCanary(t)
	c.SetPct(25)
	feed(c, "incumbent", true, 0.01, 30)
	// 5 errors out of 20 candidate samples = 25% > 5% threshold
	feed(c, "candidate", true, 0.01, 15)
	feed(c, "candidate", false, 0.01, 5)
	if c.Status() != "rolled_back" || c.Pct() != 0 || c.Rollbacks() != 1 {
		t.Fatalf("status=%s pct=%d rollbacks=%d", c.Status(), c.Pct(), c.Rollbacks())
	}
}

func TestRollbackOnLatencyBreach(t *testing.T) {
	c := makeCanary(t)
	feed(c, "incumbent", true, 0.01, 30)
	feed(c, "candidate", true, 0.10, 25) // 10x incumbent p50 > 1.5x
	if c.Status() != "rolled_back" {
		t.Fatalf("status = %s", c.Status())
	}
}

func TestHealthyCandidateRampsNotRollsBack(t *testing.T) {
	c := makeCanary(t)
	feed(c, "incumbent", true, 0.01, 30)
	feed(c, "candidate", true, 0.012, 25)
	if c.Status() != "ramping" || c.Pct() != 1 {
		t.Fatalf("status=%s pct=%d", c.Status(), c.Pct())
	}
}

func TestNoRollbackBelowMinSamples(t *testing.T) {
	c := makeCanary(t)
	feed(c, "candidate", false, 0.01, 10) // all errors but < 20 samples
	if c.Status() != "idle" {
		t.Fatalf("status = %s", c.Status())
	}
}

func TestRolledBackIsTerminal(t *testing.T) {
	c := makeCanary(t)
	c.Rollback("test")
	if c.MaybeRamp() {
		t.Fatal("ramped after rollback")
	}
	feed(c, "candidate", true, 0.01, 100)
	if c.Status() != "rolled_back" {
		t.Fatalf("status = %s", c.Status())
	}
}

// -------------------------------------------------------------------- state

func TestStateShape(t *testing.T) {
	c := makeCanary(t)
	s := c.State()
	if s.Pct != 0 || s.Status != "idle" || s.Rollbacks != 0 {
		t.Fatalf("bad state: %+v", s)
	}
	if s.Thresholds.MaxErrorRate == 0 {
		t.Fatal("thresholds missing")
	}
	if s.Candidate.N != 0 {
		t.Fatal("candidate samples should be empty")
	}
}

func TestOnResultAlias(t *testing.T) {
	c := makeCanary(t)
	c.OnResult("incumbent", true, 0.01)
	if c.State().Incumbent.N != 1 {
		t.Fatal("OnResult did not record")
	}
}

func mapKey(i int) string {
	// matches Python f"key-{i}"
	return "key-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
