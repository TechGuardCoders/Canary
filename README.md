# Canary

Gradual rollout with automatic rollback. Routes a percentage of traffic to a
candidate backend, watches error rate and latency against the incumbent, ramps
1% → 5% → 25% → 50% → 100%, and snaps back to the incumbent the moment the
candidate crosses a failure threshold.

## Why

Deploying a new backend all-at-once turns every regression into a full outage.
Canary makes the blast radius proportional to confidence: a bad candidate is
caught at 1% of traffic, not 100%. Rollback is automatic — no human in the
loop, no 3am page.

## Architecture

```
route(key) ──► hash(key) vs current pct ──► incumbent_fn or candidate_fn
                                                │
                                        record_result(ok, latency)
                                                │
                              compare candidate vs incumbent stats
                                    │                     │
                                 healthy              breach
                                    ▼                     ▼
                              maybe_ramp()            rollback()
                             1→5→25→50→100%         pct=0, status=rolled_back
```

- **Deterministic routing** — `route(key)` hashes the key, so the same key
  always hits the same backend at a given percentage. Sticky clients, reproducible tests.
- **Thresholds** — candidate error rate vs `max_error_rate` (default 5%) and
  candidate p50 latency vs `max_latency_ratio` × incumbent p50 (default 1.5×),
  judged only after `min_samples` (default 20) so noise doesn't trigger rollback.
- **Terminal states** — `rolled_back` and `complete` are sticky; a rolled-back
  release cannot silently re-ramp.

## Usage

```python
from canary import Canary

c = Canary(incumbent_fn=serve_v1, candidate_fn=serve_v2)

# per request
result = c.route(key=user_id)

# or instrument externally
c.on_result("candidate", ok=False, latency=0.42)

print(c.state())
# {'pct': 5, 'status': 'ramping', 'candidate': {...}, 'incumbent': {...}, ...}
```

Custom thresholds and ramp curve:

```python
c = Canary(serve_v1, serve_v2,
           thresholds={"max_error_rate": 0.02, "max_latency_ratio": 1.2, "min_samples": 50},
           ramp_steps=[10, 50])
```

## Results

- 16 tests, all passing: routing determinism, percentage accuracy (~25% ± 5pts
  over 10k keys), ramp progression, error-rate and latency rollback, terminal
  state stickiness, sample recording on success and exception.
- Pure Python, zero dependencies, no network in tests.

## CI

GitHub Actions runs `pytest -q` on every push.
