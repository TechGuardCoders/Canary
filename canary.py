"""Canary — gradual rollout with automatic rollback.

Routes a percentage of traffic to a candidate backend, watches error rate
and latency against the incumbent, ramps 1% -> 5% -> 25% -> 50% -> 100%,
and snaps back to the incumbent when the candidate crosses a failure
threshold.
"""

from __future__ import annotations

import hashlib
import time
from dataclasses import dataclass, field
from typing import Callable, Dict, List, Optional

RAMP_STEPS = (1, 5, 25, 50, 100)

DEFAULT_THRESHOLDS = {
    "max_error_rate": 0.05,       # candidate error rate vs incumbent
    "max_latency_ratio": 1.5,     # candidate p50 latency / incumbent p50
    "min_samples": 20,            # samples per backend before judging
}


@dataclass
class Sample:
    ok: bool
    latency: float
    backend: str
    ts: float = field(default_factory=time.time)


class Canary:
    """Gradual rollout controller with deterministic routing and auto-rollback."""

    def __init__(
        self,
        incumbent_fn: Callable[[], object],
        candidate_fn: Callable[[], object],
        thresholds: Optional[Dict[str, float]] = None,
        ramp_steps: Optional[List[int]] = None,
    ):
        self.incumbent_fn = incumbent_fn
        self.candidate_fn = candidate_fn
        self.thresholds = dict(DEFAULT_THRESHOLDS)
        if thresholds:
            self.thresholds.update(thresholds)
        self.ramp_steps = list(ramp_steps) if ramp_steps else list(RAMP_STEPS)

        self.pct = 0
        self.status = "idle"  # idle | ramping | healthy | rolled_back | complete
        self.samples: List[Sample] = []
        self.rollbacks = 0
        self.last_reason = ""
        self._samples_at_last_ramp = 0

    # ------------------------------------------------------------------ route

    def route(self, key: str = "") -> object:
        """Execute the chosen backend for this request.

        Deterministic: the same key always maps to the same backend at a
        given percentage, so tests (and sticky clients) are reproducible.
        """
        use_candidate = self._pick_candidate(key)
        backend = "candidate" if use_candidate else "incumbent"
        start = time.perf_counter()
        try:
            fn = self.candidate_fn if use_candidate else self.incumbent_fn
            result = fn()
            ok = True
        except Exception:
            result = None
            ok = False
            raise
        finally:
            latency = time.perf_counter() - start
            self.record_result(backend, ok, latency)
        return result

    def _pick_candidate(self, key: str) -> bool:
        if self.pct >= 100:
            return True
        if self.pct <= 0:
            return False
        h = int(hashlib.sha256(key.encode("utf-8")).hexdigest(), 16)
        return (h % 10_000) < self.pct * 100

    # ----------------------------------------------------------------- record

    def record_result(self, backend: str, ok: bool, latency: float) -> None:
        self.samples.append(Sample(ok=ok, latency=latency, backend=backend))
        self._evaluate()

    def on_result(self, backend: str, ok: bool, latency: float) -> None:
        """Alias for record_result (external instrumentation)."""
        self.record_result(backend, ok, latency)

    # ----------------------------------------------------------------- stats

    def _stats(self, backend: str) -> Dict[str, float]:
        s = [x for x in self.samples if x.backend == backend]
        n = len(s)
        if n == 0:
            return {"n": 0, "error_rate": 0.0, "p50": 0.0}
        lats = sorted(x.latency for x in s)
        return {
            "n": n,
            "error_rate": sum(0 if x.ok else 1 for x in s) / n,
            "p50": lats[n // 2],
        }

    def _evaluate(self) -> None:
        if self.status in ("rolled_back", "complete"):
            return
        cand = self._stats("candidate")
        inc = self._stats("incumbent")
        if cand["n"] < self.thresholds["min_samples"]:
            return
        if inc["n"] > 0 and inc["p50"] > 0:
            ratio = cand["p50"] / inc["p50"]
        else:
            ratio = 1.0
        if cand["error_rate"] > self.thresholds["max_error_rate"] or (
            ratio > self.thresholds["max_latency_ratio"]
        ):
            self.rollback("threshold breach")
            return
        # Ramp at most once per min_samples window so a stream of healthy
        # results doesn't blow through the whole curve in one burst.
        if len(self.samples) - self._samples_at_last_ramp >= self.thresholds["min_samples"]:
            if self.maybe_ramp():
                self._samples_at_last_ramp = len(self.samples)

    # ------------------------------------------------------------------ ramp

    def maybe_ramp(self) -> bool:
        """Advance to the next ramp step if the candidate is healthy."""
        if self.status in ("rolled_back", "complete"):
            return False
        idx = self.ramp_steps.index(self.pct) if self.pct in self.ramp_steps else -1
        if idx + 1 < len(self.ramp_steps):
            self.pct = self.ramp_steps[idx + 1]
            self.status = "ramping" if self.pct < self.ramp_steps[-1] else "complete"
            return True
        return False

    def rollback(self, reason: str = "") -> None:
        """Snap back to the incumbent immediately."""
        self.pct = 0
        self.status = "rolled_back"
        self.rollbacks += 1
        self.last_reason = reason

    def promote(self) -> None:
        """Manually send 100% to the candidate."""
        self.pct = 100
        self.status = "complete"

    # ----------------------------------------------------------------- state

    def state(self) -> Dict[str, object]:
        return {
            "pct": self.pct,
            "status": self.status,
            "candidate": self._stats("candidate"),
            "incumbent": self._stats("incumbent"),
            "rollbacks": self.rollbacks,
            "thresholds": dict(self.thresholds),
        }
