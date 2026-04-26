package scaler

import (
	"fmt"
	"strconv"
)

// DecodeDecision is the output of the decode bottleneck analysis.
type DecodeDecision struct {
	// UnderPressure is true when the decode pool needs more capacity.
	UnderPressure bool

	// Reason describes what triggered the pressure signal.
	// One of: "tpot_slo_breach", "kv_cache_pressure", "none".
	Reason string

	// TpotP95Ms is the observed TPOT p95 in milliseconds.
	TpotP95Ms float64

	// KVCachePct is the observed KV cache utilization (0.0-1.0).
	KVCachePct float64
}

// AnalyseDecode determines whether the decode pool is a bottleneck.
//
// Two signals are checked:
//  1. TPOT p95 breach: decode is too slow → user-visible token generation lag
//  2. KV cache pressure: decode pods near OOM → risk of request drops
func AnalyseDecode(
	tpotP95Ms float64,
	kvCachePct float64,
	tpotSLOMs int32,
	kvCacheThresholdStr string,
) DecodeDecision {
	kvCacheThreshold := 0.85 // sensible default
	if kvCacheThresholdStr != "" {
		if parsed, err := strconv.ParseFloat(kvCacheThresholdStr, 64); err == nil {
			kvCacheThreshold = parsed
		}
	}

	// Signal 1: TPOT p95 exceeds SLO
	if tpotP95Ms > float64(tpotSLOMs) {
		return DecodeDecision{
			UnderPressure: true,
			Reason:        fmt.Sprintf("tpot_slo_breach(%.0fms>%dms)", tpotP95Ms, tpotSLOMs),
			TpotP95Ms:     tpotP95Ms,
			KVCachePct:    kvCachePct,
		}
	}

	// Signal 2: KV cache approaching OOM
	if kvCachePct > kvCacheThreshold {
		return DecodeDecision{
			UnderPressure: true,
			Reason:        fmt.Sprintf("kv_cache_pressure(%.2f>%.2f)", kvCachePct, kvCacheThreshold),
			TpotP95Ms:     tpotP95Ms,
			KVCachePct:    kvCachePct,
		}
	}

	return DecodeDecision{
		UnderPressure: false,
		Reason:        "none",
		TpotP95Ms:     tpotP95Ms,
		KVCachePct:    kvCachePct,
	}
}

// ScaleDecision is the joint output of the P/D ratio optimisation.
type ScaleDecision struct {
	DesiredPrefill int32
	DesiredDecode  int32
	Bottleneck     string
	Reason         string
}

// ComputeScaleDecision runs the joint P/D ratio optimisation.
//
// This is what makes pd-ratio-coordinator different from WVA:
// WVA scales each pool independently. We solve them jointly
// subject to: prefill.replicas + decode.replicas <= gpuBudget.
//
// Priority when both are under pressure: decode first
// (user-visible TPOT > TTFT for UX — waiting for tokens is worse than waiting for first token).
func ComputeScaleDecision(
	currentPrefill int32,
	currentDecode int32,
	gpuBudget int32,
	minPrefill int32,
	maxPrefill int32,
	minDecode int32,
	maxDecode int32,
	prefill PrefillDecision,
	decode DecodeDecision,
) ScaleDecision {
	desired := ScaleDecision{
		DesiredPrefill: currentPrefill,
		DesiredDecode:  currentDecode,
		Bottleneck:     BottleneckNone,
	}

	switch {
	case prefill.UnderPressure && !decode.UnderPressure:
		desired.Bottleneck = BottleneckPrefill
		desired.Reason = prefill.Reason
		if desired.DesiredPrefill < maxPrefill &&
			desired.DesiredPrefill+desired.DesiredDecode < gpuBudget {
			desired.DesiredPrefill++
		}

	case decode.UnderPressure && !prefill.UnderPressure:
		desired.Bottleneck = BottleneckDecode
		desired.Reason = decode.Reason
		if desired.DesiredDecode < maxDecode &&
			desired.DesiredPrefill+desired.DesiredDecode < gpuBudget {
			desired.DesiredDecode++
		}

	case prefill.UnderPressure && decode.UnderPressure:
		desired.Bottleneck = BottleneckBoth
		desired.Reason = fmt.Sprintf("prefill:%s decode:%s", prefill.Reason, decode.Reason)
		// Prioritise decode — TPOT is more user-visible than TTFT
		if desired.DesiredDecode < maxDecode &&
			desired.DesiredPrefill+desired.DesiredDecode < gpuBudget {
			desired.DesiredDecode++
		} else if desired.DesiredPrefill < maxPrefill &&
			desired.DesiredPrefill+desired.DesiredDecode < gpuBudget {
			desired.DesiredPrefill++
		}

	default:
		desired.Bottleneck = BottleneckNone
		desired.Reason = "none"
		// No pressure — enforce budget if over it (shouldn't happen, defensive)
	}

	// Hard constraints: clamp to min/max
	desired.DesiredPrefill = clamp(desired.DesiredPrefill, minPrefill, maxPrefill)
	desired.DesiredDecode = clamp(desired.DesiredDecode, minDecode, maxDecode)

	// Enforce GPU budget: reduce decode first (prefill is stateless, decode has KV cache)
	for desired.DesiredPrefill+desired.DesiredDecode > gpuBudget {
		if desired.DesiredDecode > minDecode {
			desired.DesiredDecode--
		} else if desired.DesiredPrefill > minPrefill {
			desired.DesiredPrefill--
		} else {
			break
		}
	}

	return desired
}

// ComputeDynamicThreshold suggests a new PD_PROMPT_LEN_THRESHOLD based on
// the observed input sequence length (ISL) distribution.
//
// Target: threshold sits between p50 and p90 of ISL so that ~20-40% of
// traffic (the long-tail) uses disaggregation.
//
// islP50, islP90: observed ISL percentiles from vllm:prompt_tokens histogram.
// currentThreshold: the value currently set on decode pods.
func ComputeDynamicThreshold(islP50, islP90 float64, currentThreshold int32) int32 {
	if islP50 == 0 || islP90 == 0 {
		return currentThreshold
	}
	// Midpoint heuristic: threshold = p50 + 0.5*(p90-p50)
	suggested := islP50 + 0.5*(islP90-islP50)
	return clamp(int32(suggested), 200, 8192)
}

func clamp(val, min, max int32) int32 {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}
