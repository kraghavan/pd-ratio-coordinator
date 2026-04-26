// Package scaler contains the core P/D ratio coordination logic.
// Split into three files:
//   prefill.go   — prefill bottleneck detection
//   decode.go    — decode bottleneck detection
//   cooldown.go  — oscillation prevention
package scaler

// VelocityResult holds the output of the prefill spike detector.
type VelocityResult struct {
	// Delta is the change in queue depth over the observation window.
	Delta float64

	// RatePerSecond is the average growth rate per second.
	RatePerSecond float64

	// IsSpiking is true when delta exceeds the configured trigger.
	IsSpiking bool

	// Trend is "rising", "falling", or "stable".
	Trend string
}

// PrefillDecision is the output of the prefill bottleneck analysis.
type PrefillDecision struct {
	// UnderPressure is true when the prefill pool needs more capacity.
	UnderPressure bool

	// Reason describes what triggered the pressure signal.
	Reason string

	// Velocity is the queue velocity analysis.
	Velocity VelocityResult
}

// AnalysePrefill determines whether the prefill pool is a bottleneck.
//
// Two signals are checked:
//  1. QueueDepth: current requests waiting exceeds queueDepthTrigger
//  2. QueueVelocity: rate of queue growth exceeds queueVelocityTrigger
//     within velocityWindowSeconds (catches spikes before saturation)
//
// WVA only uses signal 1. Signal 2 is what makes this controller faster.
func AnalysePrefill(
	queueDepth float64,
	queueHistory []float64,
	queueDepthTrigger int32,
	queueVelocityTrigger int32,
	velocityWindowSeconds int32,
) PrefillDecision {
	velocity := computeVelocity(queueHistory, velocityWindowSeconds, queueVelocityTrigger)

	// Signal 1: absolute queue depth
	if queueDepth >= float64(queueDepthTrigger) {
		return PrefillDecision{
			UnderPressure: true,
			Reason:        "queue_depth_exceeded",
			Velocity:      velocity,
		}
	}

	// Signal 2: velocity spike — queue growing fast even if not yet deep
	if velocity.IsSpiking {
		return PrefillDecision{
			UnderPressure: true,
			Reason:        "queue_velocity_spike",
			Velocity:      velocity,
		}
	}

	return PrefillDecision{
		UnderPressure: false,
		Reason:        "none",
		Velocity:      velocity,
	}
}

// computeVelocity analyses a time-series of queue depth samples
// and determines whether a spike is occurring.
//
// samples: queue depth readings, oldest first, newest last.
// windowSeconds: total duration covered by the samples.
// triggerDelta: how many requests the queue must grow to be a spike.
func computeVelocity(samples []float64, windowSeconds int32, triggerDelta int32) VelocityResult {
	if len(samples) < 2 {
		return VelocityResult{Trend: "stable"}
	}

	first := samples[0]
	last := samples[len(samples)-1]
	delta := last - first

	rate := delta / float64(windowSeconds)

	trend := "stable"
	switch {
	case delta > 1.0:
		trend = "rising"
	case delta < -1.0:
		trend = "falling"
	}

	return VelocityResult{
		Delta:         delta,
		RatePerSecond: rate,
		IsSpiking:     delta >= float64(triggerDelta),
		Trend:         trend,
	}
}
