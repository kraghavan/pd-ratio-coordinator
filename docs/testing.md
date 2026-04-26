# Testing pd-ratio-coordinator on Real GPU

## What You're Proving

| Test | Proves |
|---|---|
| TPOT SLO breach → scale decode | Decode bottleneck detection works |
| Queue velocity spike → scale prefill | Spike detection faster than WVA |
| GPU budget respected | Joint constraint enforced |
| Scale-down drain | No mid-sequence request drops |
| Cooldown | No oscillation under sustained load |

---

## Minimum Hardware — Lambda Labs GH200 + H100

```
Instance: 1x GH200 (96GB) + H100
Provider: Lambda Labs
Cost:     $2.29/hr
GPUs:     GH200 (GPU 0) + H100 (GPU 1)
Budget:   gpuBudget: 2
Duration: ~3 hours = ~$7
```

**Why this works:**
- 2 real GPUs — prefill on GPU 0, decode on GPU 1
- NVLink between them — NIXL KV transfer works
- No time-slicing needed

---

## At Scale — Vultr 4x L40S

```
Instance: 4x L40S 48GB (from $250 Vultr credit)
Cost:     ~$3.34/hr from credit
GPUs:     4
Budget:   gpuBudget: 4 (e.g. 2 prefill + 2 decode)
Duration: ~5 hours = ~$17
```

**What this adds:**
- Independent scaling experiments: scale prefill 1→2, watch TTFT improve
- Scale decode 1→2 while Locust runs, watch TPOT recover
- Grafana screenshots showing controller making real decisions

---

## Step-by-Step Test Plan

### Prerequisites

```bash
# Deploy llm-d P/D disaggregation (Week 2 Day 5 guide)
# All three pods must be Running:
kubectl get pods -n llm-d
# gaie-pd-epp-xxx          1/1 Running
# infra-pd-gateway-xxx     1/1 Running
# ms-pd-...-decode-xxx     2/2 Running  (vLLM + NIXL sidecar)
# ms-pd-...-prefill-xxx    1/1 Running

# Prometheus scraping vLLM — verify:
curl http://localhost:9090/api/v1/query \
  '?query=vllm:num_requests_waiting' | jq .
# Should return data (even 0 is fine)
```

### Deploy the Controller

```bash
# Install CRD
kubectl apply -f config/crd/

# Deploy RBAC + controller
kubectl apply -f config/rbac/
kubectl apply -f config/manager/

# Verify controller running
kubectl get pods -n pd-ratio-coordinator-system

# Apply the sample CR (pointing to your llm-d deployments)
kubectl apply -f config/samples/pdratiopolicy-sample.yaml

# Watch decisions in real time
kubectl get pdratiopolicy -n llm-d -w
```

### Test 1 — TPOT SLO Breach (decode bottleneck)

```bash
# Set a very aggressive TPOT SLO to trigger easily
kubectl patch pdratiopolicy qwen-scaler -n llm-d \
  --type=merge \
  -p '{"spec":{"decode":{"tpotSLOMs":20}}}'

# Generate sustained load with Locust
locust -f locustfile.py \
  --host http://localhost:8080 \
  --headless --users 30 --spawn-rate 3 --run-time 90s

# Expected: controller adds decode replica within 1-2 reconcile cycles
kubectl get pdratiopolicy -n llm-d -w
# STATUS: bottleneck=decode, desiredDecode=2

# Check controller logs
kubectl logs -n pd-ratio-coordinator-system \
  -l control-plane=controller-manager -f | grep "scaled decode"
```

**Expected log:**
```
scaled decode from=1 to=2 bottleneck=decode reason=tpot_slo_breach(45ms>20ms)
```

**Grafana:** TPOT p95 panel shows breach → recovery after scale

### Test 2 — Queue Velocity Spike (prefill bottleneck)

```bash
# Reset TPOT SLO to normal
kubectl patch pdratiopolicy qwen-scaler -n llm-d \
  --type=merge \
  -p '{"spec":{"decode":{"tpotSLOMs":80}}}'

# Set a low velocity trigger to make it easy to observe
kubectl patch pdratiopolicy qwen-scaler -n llm-d \
  --type=merge \
  -p '{"spec":{"prefill":{"queueVelocityTrigger":2}}}'

# Generate a sudden burst of long-prompt requests
for i in $(seq 1 20); do
  curl -s http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"Qwen/Qwen3-0.6B",
         "messages":[{"role":"user","content":"'"$(python3 -c "print('Analyze: ' + 'word ' * 200)")"'"}],
         "max_tokens":50}' &
done
wait

# Expected: controller detects velocity spike, scales prefill
kubectl get pdratiopolicy -n llm-d -w
# STATUS: bottleneck=prefill, desiredPrefill=2
```

### Test 3 — GPU Budget Enforcement

```bash
# Set budget = current total (no headroom)
kubectl patch pdratiopolicy qwen-scaler -n llm-d \
  --type=merge \
  -p '{"spec":{"gpuBudget":2}}'

# Trigger both prefill and decode pressure
# (20 long-prompt concurrent requests)
locust -f locustfile.py --headless --users 20 --run-time 60s

# Expected: controller does NOT exceed budget
kubectl get pdratiopolicy -n llm-d
# PREFILL + DECODE should always be <= 2
```

### Test 4 — Drain Before Scale-Down

```bash
# Start requests, then trigger scale-down
locust -f locustfile.py --headless --users 5 --run-time 120s &

# After 30s, manually reduce budget to force scale-down
sleep 30
kubectl patch pdratiopolicy qwen-scaler -n llm-d \
  --type=merge \
  -p '{"spec":{"gpuBudget":1}}'

# Check drain label applied before scale
kubectl get pods -n llm-d --show-labels | grep draining

# Check no request failures in Locust output
# Expected: 0 failures (drain completed before pod terminated)
```

---

## Grafana Panels to Watch

| Panel | Expected during test |
|---|---|
| TPOT p95 | Breaches SLO → recovers after scale |
| TTFT p95 | Improves when prefill scales up |
| Queue depth | Drops after scale |
| Token throughput | Increases after scale |
| Scheduler state | num_waiting drops post-scale |

---

## Key Commands During Testing

```bash
# Watch PDRatioPolicy decisions in real time
kubectl get pdratiopolicy -n llm-d -w

# Stream controller logs (scale events)
kubectl logs -n pd-ratio-coordinator-system \
  -l control-plane=controller-manager -f

# Current status
kubectl describe pdratiopolicy qwen-scaler -n llm-d

# Check drain labels
kubectl get pods -n llm-d --show-labels | grep -E "draining|prefill|decode"

# Current replica counts
kubectl get deployment -n llm-d | grep ms-pd
```

---

## Success Criteria

| Criteria | How to verify |
|---|---|
| Controller detects TPOT breach | Log: `scaled decode ... reason=tpot_slo_breach` |
| Controller detects queue spike | Log: `scaled prefill ... reason=queue_velocity_spike` |
| GPU budget respected | `prefill + decode <= gpuBudget` always true |
| Drain prevents failures | Locust: 0 failures during scale-down test |
| Cooldown prevents oscillation | No scale events < 120s apart in logs |
| Status updated correctly | `kubectl get pdrp` shows correct bottleneck + replicas |
