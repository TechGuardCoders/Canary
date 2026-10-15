import pytest

from canary import Canary


def make_canary(**kw):
    return Canary(lambda: "inc", lambda: "cand", **kw)


def feed(canary, backend, ok, latency, n=1):
    for _ in range(n):
        canary.record_result(backend, ok, latency)


# ---------------------------------------------------------------- routing


def test_pct_zero_never_routes_candidate():
    c = make_canary()
    for i in range(200):
        assert c._pick_candidate(f"key-{i}") is False


def test_pct_hundred_always_routes_candidate():
    c = make_canary()
    c.promote()
    for i in range(200):
        assert c._pick_candidate(f"key-{i}") is True


def test_routing_deterministic_per_key():
    c = make_canary()
    c.pct = 50
    assert c._pick_candidate("user-42") == c._pick_candidate("user-42")


def test_routing_approximates_percentage():
    c = make_canary()
    c.pct = 25
    hits = sum(c._pick_candidate(f"key-{i}") for i in range(10_000))
    assert 2_000 < hits < 3_000  # ~25% ± 5pts


def test_route_executes_chosen_backend():
    c = make_canary()
    assert c.route("k") == "inc"  # pct 0 -> incumbent
    c.promote()
    assert c.route("k") == "cand"


def test_route_records_sample():
    c = make_canary()
    c.route("k")
    assert len(c.samples) == 1
    assert c.samples[0].backend == "incumbent"
    assert c.samples[0].ok is True


def test_route_records_failure_and_reraises():
    def boom():
        raise RuntimeError("x")

    c = Canary(lambda: "inc", boom)
    c.pct = 100  # force candidate routing
    with pytest.raises(RuntimeError):
        c.route("k")
    assert c.samples[0].ok is False
    assert c.samples[0].backend == "candidate"


# ------------------------------------------------------------------ ramping


def test_ramp_steps_default():
    c = make_canary()
    assert c.pct == 0
    assert c.maybe_ramp()
    assert c.pct == 1
    for _ in range(4):
        c.maybe_ramp()
    assert c.pct == 100
    assert c.status == "complete"
    assert not c.maybe_ramp()  # no further steps


def test_custom_ramp_steps():
    c = make_canary(ramp_steps=[10, 50])
    c.maybe_ramp()
    assert c.pct == 10
    c.maybe_ramp()
    assert c.pct == 50
    assert c.status == "complete"


# ----------------------------------------------------------------- rollback


def test_rollback_on_error_rate_breach():
    c = make_canary()
    c.pct = 25
    feed(c, "incumbent", True, 0.01, n=30)
    # 5 errors out of 20 candidate samples = 25% > 5% threshold
    feed(c, "candidate", True, 0.01, n=15)
    feed(c, "candidate", False, 0.01, n=5)
    assert c.status == "rolled_back"
    assert c.pct == 0
    assert c.rollbacks == 1


def test_rollback_on_latency_breach():
    c = make_canary()
    feed(c, "incumbent", True, 0.01, n=30)
    feed(c, "candidate", True, 0.10, n=25)  # 10x incumbent p50 > 1.5x
    assert c.status == "rolled_back"


def test_healthy_candidate_ramps_not_rolls_back():
    c = make_canary()
    feed(c, "incumbent", True, 0.01, n=30)
    feed(c, "candidate", True, 0.012, n=25)
    assert c.status == "ramping"
    assert c.pct == 1


def test_no_rollback_below_min_samples():
    c = make_canary()
    feed(c, "candidate", False, 0.01, n=10)  # all errors but < 20 samples
    assert c.status == "idle"


def test_rolled_back_is_terminal():
    c = make_canary()
    c.rollback("test")
    assert not c.maybe_ramp()
    feed(c, "candidate", True, 0.01, n=100)
    assert c.status == "rolled_back"


# -------------------------------------------------------------------- state


def test_state_shape():
    c = make_canary()
    s = c.state()
    assert s["pct"] == 0
    assert s["status"] == "idle"
    assert s["rollbacks"] == 0
    assert "max_error_rate" in s["thresholds"]
    assert s["candidate"]["n"] == 0


def test_on_result_alias():
    c = make_canary()
    c.on_result("incumbent", True, 0.01)
    assert c._stats("incumbent")["n"] == 1
