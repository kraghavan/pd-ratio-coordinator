package scaler_test

import (
	"testing"
	"time"

	"github.com/kraghavan/pd-ratio-coordinator/internal/scaler"
)

// ─── Prefill Tests ────────────────────────────────────────────────────────────

func TestAnalysePrefill_QueueDepth(t *testing.T) {
	tests := []struct {
		name            string
		queueDepth      float64
		queueHistory    []float64
		depthTrigger    int32
		velocityTrigger int32
		velocityWindow  int32
		wantPressure    bool
		wantReason      string
	}{
		{
			name: "queue below trigger — no pressure",
			queueDepth: 3, queueHistory: []float64{2, 2, 3},
			depthTrigger: 5, velocityTrigger: 5, velocityWindow: 30,
			wantPressure: false, wantReason: "none",
		},
		{
			name: "queue at trigger — pressure",
			queueDepth: 5, queueHistory: []float64{4, 4, 5},
			depthTrigger: 5, velocityTrigger: 10, velocityWindow: 30,
			wantPressure: true, wantReason: "queue_depth_exceeded",
		},
		{
			name: "velocity spike with low queue — pressure",
			queueDepth: 2, queueHistory: []float64{0, 1, 2, 3, 5},
			depthTrigger: 10, velocityTrigger: 3, velocityWindow: 30,
			wantPressure: true, wantReason: "queue_velocity_spike",
		},
		{
			name: "stable queue — no pressure",
			queueDepth: 1, queueHistory: []float64{1, 1, 1, 1},
			depthTrigger: 5, velocityTrigger: 3, velocityWindow: 30,
			wantPressure: false, wantReason: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaler.AnalysePrefill(
				tt.queueDepth, tt.queueHistory,
				tt.depthTrigger, tt.velocityTrigger, tt.velocityWindow,
			)
			if got.UnderPressure != tt.wantPressure {
				t.Errorf("UnderPressure = %v, want %v", got.UnderPressure, tt.wantPressure)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// ─── Decode Tests ─────────────────────────────────────────────────────────────

func TestAnalyseDecode(t *testing.T) {
	tests := []struct {
		name         string
		tpotMs       float64
		kvCachePct   float64
		tpotSLO      int32
		kvThreshold  string
		wantPressure bool
	}{
		{
			name: "tpot within SLO KV fine — no pressure",
			tpotMs: 60, kvCachePct: 0.70, tpotSLO: 80, kvThreshold: "0.85",
			wantPressure: false,
		},
		{
			name: "tpot SLO breach — pressure",
			tpotMs: 95, kvCachePct: 0.70, tpotSLO: 80, kvThreshold: "0.85",
			wantPressure: true,
		},
		{
			name: "KV cache near OOM — pressure",
			tpotMs: 60, kvCachePct: 0.92, tpotSLO: 80, kvThreshold: "0.85",
			wantPressure: true,
		},
		{
			name: "tpot exactly at SLO — no pressure",
			tpotMs: 80, kvCachePct: 0.70, tpotSLO: 80, kvThreshold: "0.85",
			wantPressure: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaler.AnalyseDecode(tt.tpotMs, tt.kvCachePct, tt.tpotSLO, tt.kvThreshold)
			if got.UnderPressure != tt.wantPressure {
				t.Errorf("UnderPressure = %v, want %v (reason: %s)",
					got.UnderPressure, tt.wantPressure, got.Reason)
			}
		})
	}
}

// ─── Scale Decision Tests ─────────────────────────────────────────────────────

func TestComputeScaleDecision(t *testing.T) {
	pNone := scaler.PrefillDecision{UnderPressure: false, Reason: "none"}
	dNone := scaler.DecodeDecision{UnderPressure: false, Reason: "none"}
	pSpike := scaler.PrefillDecision{UnderPressure: true, Reason: "queue_velocity_spike"}
	dBreach := scaler.DecodeDecision{UnderPressure: true, Reason: "tpot_slo_breach"}

	tests := []struct {
		name           string
		curP, curD     int32
		budget         int32
		minP, maxP     int32
		minD, maxD     int32
		prefill        scaler.PrefillDecision
		decode         scaler.DecodeDecision
		wantP, wantD   int32
		wantBottleneck string
	}{
		{
			name: "no pressure — hold steady",
			curP: 2, curD: 2, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pNone, decode: dNone,
			wantP: 2, wantD: 2, wantBottleneck: "none",
		},
		{
			name: "prefill spike — add prefill",
			curP: 2, curD: 2, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pSpike, decode: dNone,
			wantP: 3, wantD: 2, wantBottleneck: "prefill",
		},
		{
			name: "decode breach — add decode",
			curP: 2, curD: 2, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pNone, decode: dBreach,
			wantP: 2, wantD: 3, wantBottleneck: "decode",
		},
		{
			name: "both pressure — prioritise decode",
			curP: 2, curD: 2, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pSpike, decode: dBreach,
			wantP: 2, wantD: 3, wantBottleneck: "both",
		},
		{
			name: "budget exhausted — no scale",
			curP: 4, curD: 4, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pSpike, decode: dNone,
			wantP: 4, wantD: 4, wantBottleneck: "prefill",
		},
		{
			name: "over budget — reduce decode",
			curP: 5, curD: 5, budget: 8, minP: 1, maxP: 6, minD: 1, maxD: 6,
			prefill: pNone, decode: dNone,
			wantP: 5, wantD: 3, wantBottleneck: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaler.ComputeScaleDecision(
				tt.curP, tt.curD, tt.budget,
				tt.minP, tt.maxP, tt.minD, tt.maxD,
				tt.prefill, tt.decode,
			)
			if got.DesiredPrefill != tt.wantP {
				t.Errorf("DesiredPrefill = %d, want %d", got.DesiredPrefill, tt.wantP)
			}
			if got.DesiredDecode != tt.wantD {
				t.Errorf("DesiredDecode = %d, want %d", got.DesiredDecode, tt.wantD)
			}
			if got.Bottleneck != tt.wantBottleneck {
				t.Errorf("Bottleneck = %q, want %q", got.Bottleneck, tt.wantBottleneck)
			}
		})
	}
}

// ─── Dynamic Threshold Tests ──────────────────────────────────────────────────

func TestComputeDynamicThreshold(t *testing.T) {
	tests := []struct {
		name    string
		islP50  float64
		islP90  float64
		current int32
		wantMin int32
		wantMax int32
	}{
		{
			name: "short prompt workload — low threshold",
			islP50: 100, islP90: 300, current: 1000,
			wantMin: 200, wantMax: 300,
		},
		{
			name: "long prompt workload — high threshold",
			islP50: 2000, islP90: 6000, current: 1000,
			wantMin: 2000, wantMax: 4500,
		},
		{
			name: "no data — keep current",
			islP50: 0, islP90: 0, current: 1000,
			wantMin: 1000, wantMax: 1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaler.ComputeDynamicThreshold(tt.islP50, tt.islP90, tt.current)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("threshold %d outside range [%d, %d]", got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// ─── Cooldown Tests ───────────────────────────────────────────────────────────

func TestCooldownGuard(t *testing.T) {
	guard := scaler.NewCooldownGuard(120)

	if guard.InCooldown(nil) {
		t.Error("InCooldown(nil) should return false")
	}

	recent := time.Now().Add(-10 * time.Second)
	if !guard.InCooldown(&recent) {
		t.Error("expected cooldown for recent scale (10s ago, cooldown=120s)")
	}

	old := time.Now().Add(-200 * time.Second)
	if guard.InCooldown(&old) {
		t.Error("expected no cooldown for old scale (200s ago, cooldown=120s)")
	}
}
