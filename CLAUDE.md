# CLAUDE.md — pd-ratio-coordinator

This file gives Claude Code the context needed to work effectively on this project.

## What This Project Does

`pd-ratio-coordinator` is a Kubernetes operator that autonomously rebalances
prefill and decode replica counts in an llm-d P/D disaggregated inference cluster.

It fills the gap left by WVA (Workload Vertical Autoscaler):
- WVA scales each pool independently based on current queue depth
- This operator reacts to **rate-of-change** (velocity spikes) before queues saturate
- This operator enforces a **joint GPU budget** across both pools
- This operator handles **graceful decode drain** before scale-down
- This operator adjusts **PD_PROMPT_LEN_THRESHOLD** based on live traffic

## Key Files

```
api/v1alpha1/pdratiopolicy_types.go     CRD definition — start here to understand the API
internal/scaler/prefill.go              Prefill bottleneck detection (queue depth + velocity)
internal/scaler/decode.go               Decode bottleneck detection (TPOT + KV cache)
internal/scaler/cooldown.go             Oscillation prevention
internal/scaler/drain.go                Graceful decode pod drain before scale-down
internal/metrics/prometheus.go          PromQL queries against vLLM metrics
internal/controller/pdratiopolicy_controller.go  Main reconciler (10s loop)
config/samples/pdratiopolicy-sample.yaml         Example CR
```

## CRD Structure (key fields)

```yaml
spec:
  gpuBudget: 8          # total GPUs — hard constraint
  prefill:
    deployment: "..."   # Deployment name
    min: 1  max: 8
    queueDepthTrigger: 5      # absolute queue threshold
    queueVelocityTrigger: 3   # spike detection threshold
    velocityWindowSeconds: 30
  decode:
    deployment: "..."
    min: 1  max: 4
    tpotSLOMs: 80             # TPOT p95 SLO in ms
    kvCacheThreshold: "0.85"  # OOM prevention
  cooldownSeconds: 120
  drainTimeoutSeconds: 30
  prometheusURL: "http://..."
```

## Control Loop Logic

```
Every 10 seconds:
  1. Query Prometheus for prefill + decode metrics
  2. AnalysePrefill():  queue depth + velocity spike → PrefillDecision
  3. AnalyseDecode():   TPOT p95 + KV cache pct   → DecodeDecision
  4. ComputeScaleDecision(): joint optimisation subject to gpuBudget
  5. Cooldown check: skip if last scale < cooldownSeconds ago
  6. Scale prefill (stateless → no drain needed)
  7. Scale decode down → drain first; scale up → no drain
  8. Update status (bottleneck, lastScaleTime, currentPDThreshold)
```

## Prometheus Metrics Used

| Metric | Pool | Purpose |
|--------|------|---------|
| `vllm:num_requests_waiting` | prefill | Queue depth |
| `vllm:time_to_first_token_seconds_bucket` | prefill | TTFT p95 |
| `vllm:time_per_output_token_seconds_bucket` | decode | TPOT p95 |
| `vllm:gpu_cache_usage_perc` | decode | KV cache pressure |
| `vllm:generation_tokens_total` | both | Throughput |
| `vllm:request_prompt_tokens_bucket` | both | ISL distribution (dynamic threshold) |

## Development Commands

```bash
# Build
make build

# Test
make test

# Run locally against current kubeconfig
make run

# Deploy to cluster
make install-crds
make deploy
make sample

# Watch decisions in real time
make watch
make logs
```

## Key Design Decisions

**Why velocity not just queue depth?**
WVA reacts to `queue_depth > threshold`. By the time this fires, the queue has
already saturated and users are waiting. Velocity detects the _rate of growth_,
catching spikes 1-2 reconcile cycles earlier.

**Why joint budget enforcement?**
If prefill and decode scale independently, they can fight for the same GPU pool.
This operator treats `prefill.replicas + decode.replicas <= gpuBudget` as a hard
constraint, solving them jointly.

**Why drain before decode scale-down?**
Decode pods hold active KV cache sequences. Terminating mid-sequence drops the
user's request. The drain label stops EPP routing new requests to the pod while
we wait for in-flight sequences to finish.

**Why decode priority when both are under pressure?**
TPOT (inter-token latency) is more user-visible than TTFT (first token). A user
waiting 5 seconds between tokens is worse UX than waiting 5 seconds for the
first token.

## Testing Against Real GPU

See `docs/testing.md` for the full GPU validation plan.

Minimum: Lambda Labs GH200 + H100 ($2.29/hr)
- prefill pod on GH200 GPU 0
- decode pod on H100 GPU 1
- This operator deployed in same K3s cluster
- Locust generating load → watch operator decisions in `make watch`

## Module

```
github.com/kraghavan/pd-ratio-coordinator
```
