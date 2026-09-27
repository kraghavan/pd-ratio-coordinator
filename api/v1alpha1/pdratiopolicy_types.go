package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PDRatioPolicySpec defines the desired configuration for P/D ratio coordination.
type PDRatioPolicySpec struct {
	// GPUBudget is the total number of GPUs available across both pools.
	// The coordinator enforces: prefill.replicas + decode.replicas <= GPUBudget.
	// +kubebuilder:validation:Minimum=2
	GPUBudget int32 `json:"gpuBudget"`

	// Namespace is the Kubernetes namespace where the prefill and decode
	// Deployments live. Defaults to the PDRatioPolicy namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Prefill holds configuration for the prefill worker pool.
	Prefill PrefillConfig `json:"prefill"`

	// Decode holds configuration for the decode worker pool.
	Decode DecodeConfig `json:"decode"`

	// CooldownSeconds is the minimum time between consecutive scale actions.
	// Prevents oscillation. Default: 120.
	// +kubebuilder:default=120
	// +optional
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty"`

	// DrainTimeoutSeconds is how long to wait for in-flight decode sequences
	// to complete before terminating a decode pod during scale-down.
	// Default: 30.
	// +kubebuilder:default=30
	// +optional
	DrainTimeoutSeconds int32 `json:"drainTimeoutSeconds,omitempty"`

	// PrometheusURL is the address of the Prometheus instance to query.
	// +kubebuilder:default="http://kube-prometheus-stack-prometheus:9090"
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`

	// DynamicThreshold controls automatic PD_PROMPT_LEN_THRESHOLD adjustment.
	// +optional
	DynamicThreshold DynamicThresholdConfig `json:"dynamicThreshold,omitempty"`
}

// PrefillConfig holds scaling configuration for the prefill worker pool.
// Prefill workers handle the compute-intensive first-token generation.
type PrefillConfig struct {
	// Deployment is the name of the prefill Deployment managed by llm-d.
	// Example: ms-pd-llm-d-modelservice-prefill
	Deployment string `json:"deployment"`

	// Min is the minimum replica count. Must be >= 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Min int32 `json:"min"`

	// Max is the maximum replica count.
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// QueueDepthTrigger: if vllm:num_requests_waiting for prefill pods
	// exceeds this value, add a prefill replica.
	// +kubebuilder:default=5
	// +optional
	QueueDepthTrigger int32 `json:"queueDepthTrigger,omitempty"`

	// QueueVelocityTrigger: if the prefill queue grows by this many requests
	// within VelocityWindowSeconds, add a prefill replica immediately.
	// This catches sudden spikes before the queue saturates.
	// +kubebuilder:default=3
	// +optional
	QueueVelocityTrigger int32 `json:"queueVelocityTrigger,omitempty"`

	// VelocityWindowSeconds is the observation window for spike detection.
	// +kubebuilder:default=30
	// +optional
	VelocityWindowSeconds int32 `json:"velocityWindowSeconds,omitempty"`
}

// DecodeConfig holds scaling configuration for the decode worker pool.
// Decode workers handle token generation after the first token.
type DecodeConfig struct {
	// Deployment is the name of the decode Deployment managed by llm-d.
	// Example: ms-pd-llm-d-modelservice-decode
	Deployment string `json:"deployment"`

	// Min is the minimum replica count. Must be >= 1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Min int32 `json:"min"`

	// Max is the maximum replica count.
	// +kubebuilder:validation:Minimum=1
	Max int32 `json:"max"`

	// TpotSLOMs is the TPOT p95 SLO in milliseconds.
	// When TPOT p95 exceeds this, decode is the bottleneck → add decode replica.
	// Default: 80ms.
	// +kubebuilder:default=80
	// +optional
	TpotSLOMs int32 `json:"tpotSLOMs,omitempty"`

	// KVCacheThreshold is the KV cache utilization threshold (0.0-1.0).
	// When decode pods exceed this, they risk OOM → add decode replica.
	// Default: 0.85.
	// +kubebuilder:default="0.85"
	// +optional
	KVCacheThreshold string `json:"kvCacheThreshold,omitempty"`
}

// DynamicThresholdConfig controls automatic adjustment of PD_PROMPT_LEN_THRESHOLD.
// When enabled, the controller adjusts the threshold based on the observed
// input sequence length (ISL) distribution of live traffic.
type DynamicThresholdConfig struct {
	// Enabled turns on automatic threshold adjustment.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// PDPromptLenThreshold is the initial token count above which a request
	// is routed to the prefill pool. Maps to PD_PROMPT_LEN_THRESHOLD env var.
	// +kubebuilder:default=1000
	PDPromptLenThreshold int32 `json:"pdPromptLenThreshold,omitempty"`
}

// PDRatioPolicyStatus reflects the observed state of the controller.
type PDRatioPolicyStatus struct {
	// CurrentPrefillReplicas is the observed prefill replica count.
	CurrentPrefillReplicas int32 `json:"currentPrefillReplicas,omitempty"`

	// CurrentDecodeReplicas is the observed decode replica count.
	CurrentDecodeReplicas int32 `json:"currentDecodeReplicas,omitempty"`

	// DesiredPrefillReplicas is what the controller wants to scale to.
	DesiredPrefillReplicas int32 `json:"desiredPrefillReplicas,omitempty"`

	// DesiredDecodeReplicas is what the controller wants to scale to.
	DesiredDecodeReplicas int32 `json:"desiredDecodeReplicas,omitempty"`

	// LastScaleTime is when the last scale action was taken.
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// LastBottleneck describes which pool was detected as the bottleneck.
	// One of: "prefill", "decode", "none", "both".
	LastBottleneck string `json:"lastBottleneck,omitempty"`

	// CurrentPDThreshold is the live value of PD_PROMPT_LEN_THRESHOLD.
	CurrentPDThreshold int32 `json:"currentPDThreshold,omitempty"`

	// Conditions holds standard Kubernetes condition types.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Bottleneck constants used in status reporting.
const (
	BottleneckNone    = "none"
	BottleneckPrefill = "prefill"
	BottleneckDecode  = "decode"
	BottleneckBoth    = "both"
)

// Condition type constants.
const (
	ConditionReady          = "Ready"
	ConditionScaling        = "Scaling"
	ConditionBudgetExceeded = "BudgetExceeded"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pdrp
// +kubebuilder:printcolumn:name="GPU Budget",type=integer,JSONPath=`.spec.gpuBudget`
// +kubebuilder:printcolumn:name="Prefill",type=integer,JSONPath=`.status.currentPrefillReplicas`
// +kubebuilder:printcolumn:name="Decode",type=integer,JSONPath=`.status.currentDecodeReplicas`
// +kubebuilder:printcolumn:name="Bottleneck",type=string,JSONPath=`.status.lastBottleneck`
// +kubebuilder:printcolumn:name="Last Scale",type=date,JSONPath=`.status.lastScaleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PDRatioPolicy is the Schema for the pdratiopolicies API.
// It coordinates prefill and decode replica counts within a fixed GPU budget,
// reacting to queue velocity spikes, TPOT SLO breaches, and KV cache pressure.
type PDRatioPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PDRatioPolicySpec   `json:"spec,omitempty"`
	Status PDRatioPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PDRatioPolicyList contains a list of PDRatioPolicy.
type PDRatioPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PDRatioPolicy `json:"items"`
}
