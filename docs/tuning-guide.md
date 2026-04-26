# Tuning Guide — pd-ratio-coordinator

How to calibrate the thresholds for your specific cluster and workload.

---

## Start With These Defaults

```yaml
prefill:
  queueDepthTrigger: 5
  queueVelocityTrigger: 3
  velocityWindowSeconds: 30
decode:
  tpotSLOMs: 80
  kvCacheThreshold: "0.85"
cooldownSeconds: 120
drainTimeoutSeconds: 30
```

These are conservative — they won't over-scale but may react slower than ideal.
Run for 24 hours, then tune based on what you observe.

---

## How to Set tpotSLOMs

Your TPOT SLO should be based on your application's UX requirements:

```
Streaming chat UI:    users notice lag > 100ms between tokens
                      → tpotSLOMs: 80  (20% headroom)

Background jobs:      latency tolerant
                      → tpotSLOMs: 300

Real-time voice:      very sensitive
                      → tpotSLOMs: 30
```

**How to measure your baseline TPOT:**

```bash
# Query Prometheus while under normal load
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=histogram_quantile(0.95,
    sum(rate(vllm:time_per_output_token_seconds_bucket[5m])) by (le)) * 1000' \
  | jq '.data.result[0].value[1]'
```

Set `tpotSLOMs` to ~120% of your observed p95 under normal load.
This gives you a buffer before the controller intervenes.

---

## How to Set queueDepthTrigger

```
Low traffic cluster (< 10 req/s):  queueDepthTrigger: 3
Medium traffic (10-100 req/s):     queueDepthTrigger: 5   ← default
High traffic (> 100 req/s):        queueDepthTrigger: 10
```

Too low → frequent unnecessary scale-ups (thrashing even with cooldown).
Too high → queue saturates before you scale (users wait).

**Rule of thumb:** set to ~2x your average burst size in requests.

---

## How to Set queueVelocityTrigger

This is the spike detection sensitivity. Lower = more sensitive.

```bash
# Observe typical queue fluctuation during normal operation
curl -s 'http://localhost:9090/api/v1/query_range' \
  --data-urlencode 'query=avg(vllm:num_requests_waiting{pod=~".*prefill.*"})' \
  --data-urlencode 'start=-30m' \
  --data-urlencode 'end=now' \
  --data-urlencode 'step=5s' \
  | jq '[.data.result[0].values[][1] | tonumber] | max - min'
```

Set `queueVelocityTrigger` to 2x the normal fluctuation range.
If queue normally swings ±2, set trigger to 4-5.

---

## How to Set cooldownSeconds

```
Pod startup time for your model:
  Qwen3-0.6B:    ~15-20s
  Llama-3.2-3B:  ~30-45s
  Llama-3.1-8B:  ~60-90s
  Mistral-7B:    ~45-60s

cooldownSeconds = 2 * pod_startup_time + 30s_prometheus_lag

Examples:
  Qwen3-0.6B:   cooldownSeconds: 60
  Llama-3.1-8B: cooldownSeconds: 210
  Mistral-7B:   cooldownSeconds: 150
```

Default 120s covers most 3-7B models.

---

## How to Set drainTimeoutSeconds

```
Average request duration at p99:
  Short prompts (<100 tokens out):  drainTimeoutSeconds: 15
  Medium (100-500 tokens out):      drainTimeoutSeconds: 30  ← default
  Long (500+ tokens out):           drainTimeoutSeconds: 60
```

**Measure your p99 E2E latency:**

```bash
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=histogram_quantile(0.99,
    sum(rate(vllm:e2e_request_latency_seconds_bucket[10m])) by (le))' \
  | jq '.data.result[0].value[1]'
```

Set `drainTimeoutSeconds` to ~2x your p99 E2E latency.

---

## Signs of Misconfiguration

### Over-scaling (too aggressive)
```
Symptom: frequent scale-up/down in logs, replica counts oscillating
Fix:
  - Increase cooldownSeconds
  - Increase queueDepthTrigger
  - Increase queueVelocityTrigger
```

### Under-scaling (too conservative)
```
Symptom: TPOT consistently above SLO, queue staying high
Fix:
  - Decrease tpotSLOMs
  - Decrease queueDepthTrigger
  - Decrease queueVelocityTrigger
```

### Budget exhaustion
```
Symptom: "GPU budget exhausted" in logs, both pools at max
Fix:
  - Increase gpuBudget (requires more GPUs)
  - Decrease max replicas for one pool
  - Use a smaller/quantized model
```

### Drain timeouts
```
Symptom: "drain timeout exceeded" warnings in logs
Fix:
  - Increase drainTimeoutSeconds
  - Decrease max request duration (lower max_tokens in vLLM)
```

---

## Calibration Workflow

```
Week 1: Deploy with defaults, observe
  kubectl logs -n pd-ratio-coordinator-system \
    -l control-plane=controller-manager | grep "pool metrics snapshot"
  
  Record: typical TPOT, queue depth, scale frequency

Week 2: Tune thresholds
  Based on observations:
  - Set tpotSLOMs to 120% of observed p95 under normal load
  - Set cooldownSeconds to 2x pod startup time
  - Set queueDepthTrigger to 2x normal burst size

Week 3: Stress test
  Locust 50 users → watch controller decisions
  Verify no missed SLO breaches
  Verify no oscillation
```
