# Architecture — pd-ratio-coordinator

## Problem Statement

llm-d supports P/D disaggregation: prefill workers handle the compute-intensive
first-token generation, decode workers handle the memory-bandwidth-intensive
token generation loop. This separation allows each pool to scale independently.

**The gap:** WVA (the existing autoscaler) scales each pool independently, reacting
to current queue depth. This misses two failure modes:

```
Failure 1: Spike blindness
  t=0:  queue=0, scale action: none
  t=5:  queue=3, below threshold, scale action: none
  t=10: queue=47, threshold breached → scale action: +1 prefill
  t=25: new pod ready, but we lost 25 seconds of requests
  
  pd-ratio-coordinator: at t=5, velocity = +3 in 5s = spike detected → scale now

Failure 2: Pool competition
  WVA scales prefill to 6 GPUs + decode to 4 GPUs = 10 GPUs
  But cluster only has 8 GPUs → both Deployments Pending
  
  pd-ratio-coordinator: gpuBudget=8 → joint constraint, always enforced
```

---

## System Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                         llm-d Cluster                            │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │              pd-ratio-coordinator (this)                 │    │
│  │                                                          │    │
│  │  Reconcile() every 10s:                                  │    │
│  │                                                          │    │
│  │  Prometheus ──metrics──► AnalysePrefill()  ─┐           │    │
│  │                          AnalyseDecode()   ─┤           │    │
│  │                          ComputeScaleDecision() ────┐   │    │
│  │                          CooldownGuard.Check() ─────┤   │    │
│  │                                               scale │   │    │
│  └───────────────────────────────────────────────┬─────┘   │    │
│                                                  │          │    │
│  ┌───────────────────────────────────────────────▼──────┐  │    │
│  │  prefill Deployment        decode Deployment          │  │    │
│  │  ┌────────────────────┐   ┌─────────────────────────┐│  │    │
│  │  │ prefill pod 0      │   │ decode pod 0             ││  │    │
│  │  │ GPU 0              │   │ GPU 1  KV cache          ││  │    │
│  │  │ vllm serve         │   │ vllm serve               ││  │    │
│  │  │ kv_role=kv_both    │   │ kv_role=kv_both          ││  │    │
│  │  └────────────────────┘   └─────────────────────────┘│  │    │
│  └───────────────────────────────────────────────────────┘  │    │
│                                                              │    │
│  ┌─────────────────────────────────────────────────────┐    │    │
│  │  Prometheus (kube-prometheus-stack)                  │    │    │
│  │  scrapes vllm:/metrics every 5s                      │    │    │
│  └─────────────────────────────────────────────────────┘    │    │
└──────────────────────────────────────────────────────────────────┘
```

---

## Control Loop Design

### Phase 1: Metric Collection (every 10s)

Two independent Prometheus queries per pool:

```
Prefill pool:
  - avg(vllm:num_requests_waiting) → current queue depth
  - range query over velocityWindowSeconds → queue history for spike detection
  - histogram_quantile(0.95, vllm:time_to_first_token_seconds_bucket) → TTFT p95

Decode pool:
  - histogram_quantile(0.95, vllm:time_per_output_token_seconds_bucket) → TPOT p95
  - avg(vllm:gpu_cache_usage_perc) → KV cache utilization
  - sum(rate(vllm:generation_tokens_total)) → throughput
```

### Phase 2: Bottleneck Analysis

**Prefill analysis (prefill.go):**

```
Signal 1: Queue depth
  queueDepth >= queueDepthTrigger → under pressure
  
Signal 2: Velocity spike (the WVA blind spot)
  delta = queue[now] - queue[now - velocityWindow]
  delta >= queueVelocityTrigger → under pressure
  
  Why velocity? If queue grows from 0→3 in 10 seconds on a prefill-heavy
  workload, we'll hit 15 in 60 seconds. Scale now, not at saturation.
```

**Decode analysis (decode.go):**

```
Signal 1: TPOT SLO breach
  tpotP95Ms > tpotSLOMs → under pressure
  User-visible: slow token generation
  
Signal 2: KV cache pressure  
  kvCachePct > kvCacheThreshold → under pressure
  Preemptive: prevent OOM before it drops requests
```

### Phase 3: Joint Optimisation

```go
// Pseudo-code for ComputeScaleDecision
switch {
case prefill.UnderPressure && !decode.UnderPressure:
    if prefill.replicas < max && total < gpuBudget:
        prefill.replicas++

case decode.UnderPressure && !prefill.UnderPressure:
    if decode.replicas < max && total < gpuBudget:
        decode.replicas++

case both.UnderPressure:
    // Prioritise decode: TPOT is more user-visible than TTFT
    if decode.replicas < max && total < gpuBudget:
        decode.replicas++
    elif prefill.replicas < max && total < gpuBudget:
        prefill.replicas++
}

// Hard constraint — always enforced
while prefill.replicas + decode.replicas > gpuBudget:
    reduce decode first (prefill is stateless)
```

### Phase 4: Cooldown

```
if time.Since(lastScaleTime) < cooldownSeconds:
    skip this reconcile cycle

Why 120s default?
  - K8s pod startup: ~30-60s for vLLM to load model
  - Prometheus scrape lag: 5-10s
  - Metric stabilization: 2-3 scrape intervals
  - Safety margin: 2x pod startup = 120s
```

### Phase 5: Scale Actions

**Prefill scale-up/down:** direct `kubectl scale` — prefill is stateless per-request.

**Decode scale-down:** graceful drain first:
```
1. Add label llmd.io/draining=true to target pod
   → EPP InferencePool selector excludes it → no new requests routed
2. Poll pod's /metrics directly: vllm:num_requests_running
3. Wait until = 0 (or drainTimeout)
4. Then scale down Deployment
```

**Decode scale-up:** direct `kubectl scale` — no drain needed.

---

## Key Metrics Reference

See `docs/metrics-reference.md` for full list with PromQL examples.

---

## Why Not Just Use HPA?

| | HPA | pd-ratio-coordinator |
|---|---|---|
| Metric source | K8s metrics server | Prometheus (raw vLLM) |
| Cross-pool awareness | No | Yes (joint GPU budget) |
| Velocity detection | No | Yes |
| Graceful drain | No | Yes |
| Dynamic PD threshold | No | Yes |
| GPU budget constraint | No | Yes |

HPA can scale one pool. It cannot coordinate two pools with a shared resource constraint.

---

## Comparison to WVA

| Feature | WVA | pd-ratio-coordinator |
|---|---|---|
| Trigger signal | Queue depth | Queue depth + velocity |
| Pool coordination | Independent | Joint (gpuBudget) |
| GPU budget | Not enforced | Hard constraint |
| Decode drain | Not handled | Graceful drain |
| PD threshold | Static | Dynamic (optional) |
| Oscillation prevention | Basic | Cooldown + hysteresis |

These tools are complementary. WVA handles vertical scaling (model/worker config).
This operator handles horizontal scaling (replica counts). Deploy both.
