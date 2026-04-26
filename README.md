# pd-ratio-coordinator

A Kubernetes operator that autonomously rebalances prefill and decode replica
counts in an [llm-d](https://github.com/llm-d/llm-d) P/D disaggregated LLM
inference cluster.

---

## The Problem

llm-d separates LLM inference into two pools:
- **Prefill workers** — compute the first token (compute-intensive)
- **Decode workers** — generate subsequent tokens (memory-bandwidth-intensive)

WVA (the existing autoscaler) scales each pool independently, reacting only to
current queue depth. This misses two failure modes:

**1. Spike blindness** — WVA reacts after queues saturate. `pd-ratio-coordinator`
detects queue *velocity* (rate of growth), scaling before saturation hits.

**2. Pool competition** — Without a joint constraint, prefill and decode can
independently request more GPUs than exist, leaving both pools Pending.
`pd-ratio-coordinator` enforces `prefill + decode ≤ gpuBudget` at all times.

---

## How It Works

```
Every 10 seconds:
  1. Query Prometheus for prefill + decode metrics
  2. Detect prefill pressure: queue depth OR velocity spike
  3. Detect decode pressure: TPOT p95 SLO breach OR KV cache near OOM
  4. Compute optimal replica counts subject to GPU budget
  5. Enforce cooldown (prevent oscillation)
  6. Scale prefill directly (stateless — no drain needed)
  7. Scale decode down → drain in-flight requests first
  8. Update PDRatioPolicy status
```

See [docs/architecture.md](docs/architecture.md) for detailed design.

---

## Quick Start

### Prerequisites

- Kubernetes cluster with llm-d P/D disaggregation deployed
- Prometheus scraping vLLM metrics (kube-prometheus-stack)
- Two Deployments: prefill pool + decode pool
- `kubectl` configured for the cluster

### Install

```bash
# 1. Install CRD
kubectl apply -f config/crd/

# 2. Deploy RBAC + controller
kubectl apply -f config/rbac/
kubectl apply -f config/manager/

# 3. Verify controller is running
kubectl get pods -n pd-ratio-coordinator-system

# 4. Apply a PDRatioPolicy CR
kubectl apply -f config/samples/pdratiopolicy-sample.yaml

# 5. Watch decisions in real time
kubectl get pdratiopolicy -n llm-d -w
```

### Configure

Edit the sample CR to match your cluster:

```yaml
apiVersion: llmd.io/v1alpha1
kind: PDRatioPolicy
metadata:
  name: qwen-scaler
  namespace: llm-d
spec:
  gpuBudget: 8          # total GPUs — hard constraint

  prefill:
    deployment: ms-pd-llm-d-modelservice-prefill
    min: 1
    max: 8
    queueDepthTrigger: 5      # scale when queue > 5 requests
    queueVelocityTrigger: 3   # scale when queue grows by 3 in 30s
    velocityWindowSeconds: 30

  decode:
    deployment: ms-pd-llm-d-modelservice-decode
    min: 1
    max: 4
    tpotSLOMs: 80             # scale when TPOT p95 > 80ms
    kvCacheThreshold: "0.85"  # scale when KV cache > 85%

  cooldownSeconds: 120
  drainTimeoutSeconds: 30
  prometheusURL: http://kube-prometheus-stack-prometheus.llm-d-monitoring:9090
```

See [docs/tuning-guide.md](docs/tuning-guide.md) for threshold calibration.

---

## Observing Decisions

```bash
# Current state — replica counts and bottleneck
kubectl get pdratiopolicy -n llm-d
# NAME          GPU BUDGET   PREFILL   DECODE   BOTTLENECK   LAST SCALE
# qwen-scaler   8            2         3        decode       2m ago

# Detailed status
kubectl describe pdratiopolicy qwen-scaler -n llm-d

# Controller logs — scale events
kubectl logs -n pd-ratio-coordinator-system \
  -l control-plane=controller-manager -f

# Example log output:
# pool metrics snapshot prefill.queue=0 decode.tpot_p95_ms=94.3 decode.kv_cache_pct=0.71
# scaled decode from=2 to=3 bottleneck=decode reason=tpot_slo_breach(94.3ms>80ms)
```

---

## How This Differs From WVA

| Feature | WVA | pd-ratio-coordinator |
|---|---|---|
| Trigger signal | Queue depth | Queue depth + **velocity** |
| Pool coordination | Independent | **Joint** (gpuBudget) |
| GPU budget | Not enforced | **Hard constraint** |
| Decode drain | Not handled | **Graceful drain** |
| PD threshold | Static | **Dynamic** (optional) |
| Oscillation | Basic | **Cooldown + hysteresis** |

These tools are **complementary**. WVA handles vertical config. This operator
handles horizontal replica counts. Deploy both.

---

## Development

```bash
# Build
make build

# Test
make test

# Run locally (uses current kubeconfig)
make run

# Build and push Docker image
make docker-build docker-push IMG=ghcr.io/<youruser>/pd-ratio-coordinator:latest
```

### Project Structure

```
pd-ratio-coordinator/
├── main.go                         entrypoint
├── api/v1alpha1/
│   ├── pdratiopolicy_types.go      CRD definition
│   └── register.go                 scheme registration
├── internal/
│   ├── controller/
│   │   └── pdratiopolicy_controller.go   main reconciler
│   ├── metrics/
│   │   └── prometheus.go           PromQL client
│   └── scaler/
│       ├── prefill.go              prefill bottleneck detection
│       ├── decode.go               decode bottleneck detection + joint optimisation
│       ├── cooldown.go             oscillation prevention
│       └── drain.go                graceful decode pod drain
├── config/
│   ├── crd/                        CRD YAML (generated)
│   ├── rbac/                       RBAC manifests
│   ├── manager/                    Deployment YAML
│   └── samples/                    example PDRatioPolicy CR
├── docs/
│   ├── architecture.md             control loop design
│   ├── tuning-guide.md             threshold calibration
│   ├── metrics-reference.md        every PromQL query used
│   └── testing.md                  GPU validation plan
└── tests/
    ├── unit/                       unit tests for scaler logic
    └── e2e/                        end-to-end tests against real cluster
```

---

## GPU Requirements for Testing

**Minimum (routing validation):**
- Lambda Labs GH200 + H100 — $2.29/hr
- 2 GPUs, no time-slicing, NIXL over NVLink

**At scale (performance validation):**
- Vultr 4x L40S — ~$3.34/hr from credit
- 2 prefill + 2 decode, independent scaling experiments

See [docs/testing.md](docs/testing.md) for step-by-step test plan.

---

## Related Projects

- [llm-d](https://github.com/llm-d/llm-d) — the inference platform this extends
- [vLLM](https://github.com/vllm-project/vllm) — the serving engine
- [WVA](https://github.com/llm-d/llm-d/tree/main/guides) — complementary vertical autoscaler

---

## Author

Karthika Raghavan — [@kraghavan](https://github.com/kraghavan)

Built as part of an LLM Infrastructure Engineer learning journey.
Week 2 lab data: [gpu-labs](https://github.com/kraghavan/gpu-labs)
