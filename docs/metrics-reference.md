# Metrics Reference — pd-ratio-coordinator

Every Prometheus metric the controller reads, with PromQL examples
you can run directly in Prometheus UI at http://localhost:9090.

---

## Prefill Pool Metrics

### Queue Depth
```promql
avg(vllm:num_requests_waiting{pod=~"ms-pd-llm-d-modelservice-prefill.*"})
```
**What it means:** Average number of requests waiting to be processed across all prefill pods.
**Trigger:** `queueDepthTrigger` (default: 5)
**Action:** Add prefill replica if exceeded.

### Queue Velocity (spike detection)
```promql
# Range query over velocityWindowSeconds — controller queries this as a time series
avg(vllm:num_requests_waiting{pod=~"ms-pd-llm-d-modelservice-prefill.*"})
```
**What it means:** Rate of change in queue depth. Computed as `queue[now] - queue[now - window]`.
**Trigger:** `queueVelocityTrigger` (default: 3 requests per window)
**Action:** Add prefill replica immediately on spike.

### TTFT p95
```promql
histogram_quantile(0.95,
  sum(rate(vllm:time_to_first_token_seconds_bucket{
    pod=~"ms-pd-llm-d-modelservice-prefill.*"}[2m])) by (le)) * 1000
```
**What it means:** 95th percentile time-to-first-token in milliseconds.
**Usage:** Informational — logged in status, not a scaling trigger (queue metrics drive prefill scaling).

---

## Decode Pool Metrics

### TPOT p95 — primary decode trigger
```promql
histogram_quantile(0.95,
  sum(rate(vllm:time_per_output_token_seconds_bucket{
    pod=~"ms-pd-llm-d-modelservice-decode.*"}[2m])) by (le)) * 1000
```
**What it means:** 95th percentile time-per-output-token in milliseconds.
**Trigger:** `tpotSLOMs` (default: 80ms)
**Action:** Add decode replica when p95 exceeds SLO.

### KV Cache Utilization — OOM prevention
```promql
avg(vllm:gpu_cache_usage_perc{pod=~"ms-pd-llm-d-modelservice-decode.*"})
```
**What it means:** Fraction of GPU KV cache used (0.0–1.0) averaged across decode pods.
**Trigger:** `kvCacheThreshold` (default: 0.85)
**Action:** Add decode replica before OOM. At 0.85, there's ~15% headroom — enough for ~1 more request batch.

---

## Both Pools

### Token Throughput
```promql
sum(rate(vllm:generation_tokens_total{
  pod=~"ms-pd-llm-d-modelservice-.*"}[1m]))
```
**What it means:** Output tokens generated per second across all pods.
**Usage:** Informational — shows overall system health.

---

## Dynamic Threshold Metrics (optional)

### Input Sequence Length p50
```promql
histogram_quantile(0.50,
  sum(rate(vllm:request_prompt_tokens_bucket[5m])) by (le))
```

### Input Sequence Length p90
```promql
histogram_quantile(0.90,
  sum(rate(vllm:request_prompt_tokens_bucket[5m])) by (le))
```
**Usage:** When `dynamicThreshold.enabled=true`, controller adjusts `PD_PROMPT_LEN_THRESHOLD`
so ~20-40% of traffic (the long-tail above p50+0.5*(p90-p50)) uses disaggregation.

---

## Controller Health Metrics

The controller exposes its own Prometheus metrics at `:8080/metrics`:

```promql
# Scale events counter
pd_ratio_coordinator_scale_events_total{pool="prefill",reason="queue_velocity_spike"}
pd_ratio_coordinator_scale_events_total{pool="decode",reason="tpot_slo_breach"}

# Current replica counts (mirrors PDRatioPolicy status)
pd_ratio_coordinator_current_replicas{pool="prefill"}
pd_ratio_coordinator_current_replicas{pool="decode"}

# Cooldown state (1=in cooldown, 0=ready to scale)
pd_ratio_coordinator_cooldown_active
```

> **Note:** Controller metrics require adding a ServiceMonitor after deployment.
> See `config/rbac/role.yaml` for the metrics scraping RBAC.

---

## Debugging Queries

```promql
# Is vLLM being scraped?
up{job="vllm"}

# Which pods are being scraped?
vllm:num_requests_waiting

# Is the prefill queue building up?
avg_over_time(vllm:num_requests_waiting{
  pod=~"ms-pd-llm-d-modelservice-prefill.*"}[5m])

# Is decode TPOT trending toward SLO breach?
histogram_quantile(0.95,
  sum(rate(vllm:time_per_output_token_seconds_bucket{
    pod=~"ms-pd-llm-d-modelservice-decode.*"}[1m])) by (le)) * 1000

# Is KV cache trending toward OOM?
max(vllm:gpu_cache_usage_perc{
  pod=~"ms-pd-llm-d-modelservice-decode.*"})
```

---

## Metric Name Notes

vLLM uses a colon namespace: `vllm:metric_name` not `vllm_metric_name`.

If Grafana panels show no data, verify the metric name in Prometheus:
```
http://localhost:9090 → query: vllm
```
All vLLM metrics should appear. If not, the PodMonitor label is likely missing.
See Week 2 lab doc: patch PodMonitor with `release=kube-prometheus-stack`.
