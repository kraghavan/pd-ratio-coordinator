// Package controller contains the main reconciler for PDRatioPolicy resources.
//
// Control loop (every 10s):
//  1. Fetch Prometheus metrics for prefill and decode pools
//  2. Analyse prefill: queue depth + velocity spike detection
//  3. Analyse decode: TPOT p95 + KV cache pressure
//  4. Compute joint scale decision (prefill + decode <= gpuBudget)
//  5. Enforce cooldown to prevent oscillation
//  6. Issue scale actions with graceful drain for decode scale-down
//  7. Optionally adjust PD_PROMPT_LEN_THRESHOLD based on ISL distribution
//  8. Update PDRatioPolicy status
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	llmdv1alpha1 "github.com/kraghavan/pd-ratio-coordinator/api/v1alpha1"
	"github.com/kraghavan/pd-ratio-coordinator/internal/metrics"
	"github.com/kraghavan/pd-ratio-coordinator/internal/scaler"
)

const (
	// ReconcileInterval is how often the control loop runs.
	ReconcileInterval = 10 * time.Second

	// PDThresholdEnvVar is the env var on decode pods that controls
	// which requests get disaggregated to the prefill pool.
	PDThresholdEnvVar = "PD_PROMPT_LEN_THRESHOLD"
)

// PDRatioPolicyReconciler reconciles a PDRatioPolicy object.
type PDRatioPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=llmd.io,resources=pdratiopolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llmd.io,resources=pdratiopolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch

// Reconcile is the main control loop entry point.
func (r *PDRatioPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PDRatioPolicy
	policy := &llmdv1alpha1.PDRatioPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	ns := policy.Spec.Namespace
	if ns == "" {
		ns = policy.Namespace
	}

	// Fetch current replica counts
	prefillDeploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: policy.Spec.Prefill.Deployment, Namespace: ns,
	}, prefillDeploy); err != nil {
		return ctrl.Result{RequeueAfter: ReconcileInterval},
			fmt.Errorf("fetching prefill deployment: %w", err)
	}

	decodeDeploy := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{
		Name: policy.Spec.Decode.Deployment, Namespace: ns,
	}, decodeDeploy); err != nil {
		return ctrl.Result{RequeueAfter: ReconcileInterval},
			fmt.Errorf("fetching decode deployment: %w", err)
	}

	currentPrefill := *prefillDeploy.Spec.Replicas
	currentDecode := *decodeDeploy.Spec.Replicas

	// Build Prometheus client
	promURL := policy.Spec.PrometheusURL
	if promURL == "" {
		promURL = "http://kube-prometheus-stack-prometheus:9090"
	}
	promClient, err := metrics.NewClient(promURL)
	if err != nil {
		return ctrl.Result{RequeueAfter: ReconcileInterval},
			fmt.Errorf("creating prometheus client: %w", err)
	}

	// Build pod selectors matching llm-d modelservice deployment labels
	prefillSelector := fmt.Sprintf(`pod=~"%s.*"`, policy.Spec.Prefill.Deployment)
	decodeSelector := fmt.Sprintf(`pod=~"%s.*"`, policy.Spec.Decode.Deployment)

	// Fetch metrics
	prefillMetrics, err := promClient.QueryPrefillMetrics(ctx, prefillSelector)
	if err != nil {
		logger.Error(err, "failed to fetch prefill metrics, skipping")
		return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
	}

	decodeMetrics, err := promClient.QueryDecodeMetrics(ctx, decodeSelector)
	if err != nil {
		logger.Error(err, "failed to fetch decode metrics, skipping")
		return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
	}

	// Fetch queue history for velocity/spike detection
	queueHistory, _ := promClient.QueryQueueHistory(
		ctx, prefillSelector, policy.Spec.Prefill.VelocityWindowSeconds)

	// Analyse each pool
	prefillDecision := scaler.AnalysePrefill(
		prefillMetrics.QueueDepth,
		queueHistory,
		policy.Spec.Prefill.QueueDepthTrigger,
		policy.Spec.Prefill.QueueVelocityTrigger,
		policy.Spec.Prefill.VelocityWindowSeconds,
	)

	decodeDecision := scaler.AnalyseDecode(
		decodeMetrics.TPOT_P95Ms,
		decodeMetrics.KVCacheUsagePct,
		policy.Spec.Decode.TpotSLOMs,
		policy.Spec.Decode.KVCacheThreshold,
	)

	logger.Info("pool metrics snapshot",
		"prefill.queue", prefillMetrics.QueueDepth,
		"prefill.ttft_p95_ms", prefillMetrics.TTFT_P95Ms,
		"prefill.under_pressure", prefillDecision.UnderPressure,
		"prefill.reason", prefillDecision.Reason,
		"decode.tpot_p95_ms", decodeMetrics.TPOT_P95Ms,
		"decode.kv_cache_pct", decodeMetrics.KVCacheUsagePct,
		"decode.under_pressure", decodeDecision.UnderPressure,
		"decode.reason", decodeDecision.Reason,
	)

	// Compute joint scale decision
	decision := scaler.ComputeScaleDecision(
		currentPrefill, currentDecode,
		policy.Spec.GPUBudget,
		policy.Spec.Prefill.Min, policy.Spec.Prefill.Max,
		policy.Spec.Decode.Min, policy.Spec.Decode.Max,
		prefillDecision, decodeDecision,
	)

	// Enforce cooldown
	cooldown := scaler.NewCooldownGuard(policy.Spec.CooldownSeconds)
	if policy.Status.LastScaleTime != nil {
		t := policy.Status.LastScaleTime.Time
		if cooldown.InCooldown(&t) {
			logger.Info("in cooldown, skipping scale",
				"remaining", cooldown.Remaining(&t).String())
			return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
		}
	}

	// Apply scale actions
	scaled := false

	if decision.DesiredPrefill != currentPrefill {
		if err := r.scalePrefill(ctx, prefillDeploy, decision.DesiredPrefill); err != nil {
			return ctrl.Result{RequeueAfter: ReconcileInterval},
				fmt.Errorf("scaling prefill: %w", err)
		}
		logger.Info("scaled prefill",
			"from", currentPrefill, "to", decision.DesiredPrefill,
			"bottleneck", decision.Bottleneck, "reason", decision.Reason)
		scaled = true
	}

	if decision.DesiredDecode != currentDecode {
		if decision.DesiredDecode < currentDecode {
			if err := r.drainAndScaleDecode(ctx, policy, decodeDeploy, decision.DesiredDecode, ns); err != nil {
				return ctrl.Result{RequeueAfter: ReconcileInterval},
					fmt.Errorf("draining decode: %w", err)
			}
		} else {
			if err := r.scaleDecode(ctx, decodeDeploy, decision.DesiredDecode); err != nil {
				return ctrl.Result{RequeueAfter: ReconcileInterval},
					fmt.Errorf("scaling decode up: %w", err)
			}
		}
		logger.Info("scaled decode",
			"from", currentDecode, "to", decision.DesiredDecode,
			"bottleneck", decision.Bottleneck)
		scaled = true
	}

	// Dynamic PD_PROMPT_LEN_THRESHOLD adjustment
	if policy.Spec.DynamicThreshold.Enabled {
		if err := r.adjustPDThreshold(ctx, policy, decodeDeploy, promClient, decodeSelector); err != nil {
			logger.Error(err, "failed to adjust PD threshold (non-fatal)")
		}
	}

	// Update status
	now := metav1.Now()
	policy.Status.CurrentPrefillReplicas = currentPrefill
	policy.Status.CurrentDecodeReplicas = currentDecode
	policy.Status.DesiredPrefillReplicas = decision.DesiredPrefill
	policy.Status.DesiredDecodeReplicas = decision.DesiredDecode
	policy.Status.LastBottleneck = decision.Bottleneck
	policy.Status.CurrentPDThreshold = policy.Spec.DynamicThreshold.PDPromptLenThreshold

	if scaled {
		policy.Status.LastScaleTime = &now
	}

	if err := r.Status().Update(ctx, policy); err != nil {
		return ctrl.Result{RequeueAfter: ReconcileInterval},
			fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: ReconcileInterval}, nil
}

func (r *PDRatioPolicyReconciler) scalePrefill(
	ctx context.Context, deploy *appsv1.Deployment, desired int32,
) error {
	patch := client.MergeFrom(deploy.DeepCopy())
	deploy.Spec.Replicas = &desired
	return r.Patch(ctx, deploy, patch)
}

func (r *PDRatioPolicyReconciler) scaleDecode(
	ctx context.Context, deploy *appsv1.Deployment, desired int32,
) error {
	patch := client.MergeFrom(deploy.DeepCopy())
	deploy.Spec.Replicas = &desired
	return r.Patch(ctx, deploy, patch)
}

func (r *PDRatioPolicyReconciler) drainAndScaleDecode(
	ctx context.Context,
	policy *llmdv1alpha1.PDRatioPolicy,
	deploy *appsv1.Deployment,
	desired int32,
	ns string,
) error {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(ns),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels),
	); err != nil {
		return fmt.Errorf("listing decode pods: %w", err)
	}

	// Pick a pod not already draining
	var target *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Labels[scaler.DrainLabel] != scaler.DrainLabelValue {
			target = pod
			break
		}
	}

	drainMgr := scaler.NewDrainManager(r.Client, policy.Spec.DrainTimeoutSeconds)

	if target != nil {
		if err := drainMgr.DrainAndWait(ctx, *target); err != nil {
			log.FromContext(ctx).Error(err, "drain timeout, force scaling down")
		}
	}

	return r.scaleDecode(ctx, deploy, desired)
}

func (r *PDRatioPolicyReconciler) adjustPDThreshold(
	ctx context.Context,
	policy *llmdv1alpha1.PDRatioPolicy,
	deploy *appsv1.Deployment,
	promClient *metrics.Client,
	decodeSelector string,
) error {
	islP50, islP90, err := promClient.QueryISLPercentiles(ctx, decodeSelector)
	if err != nil {
		return fmt.Errorf("fetching ISL percentiles: %w", err)
	}

	current := policy.Spec.DynamicThreshold.PDPromptLenThreshold
	suggested := scaler.ComputeDynamicThreshold(islP50, islP90, current)

	// Apply hysteresis: only update if change is > 20%
	diff := suggested - current
	if diff < 0 {
		diff = -diff
	}
	threshold := current / 5 // 20%
	if diff <= threshold {
		return nil // change too small, skip rolling restart
	}

	// Patch the env var on the decode Deployment
	patch := client.MergeFrom(deploy.DeepCopy())
	for i, container := range deploy.Spec.Template.Spec.Containers {
		for j, env := range container.Env {
			if env.Name == PDThresholdEnvVar {
				deploy.Spec.Template.Spec.Containers[i].Env[j].Value =
					fmt.Sprintf("%d", suggested)
				break
			}
		}
	}

	if err := r.Patch(ctx, deploy, patch); err != nil {
		return fmt.Errorf("patching PD threshold: %w", err)
	}

	policy.Status.CurrentPDThreshold = suggested
	log.FromContext(ctx).Info("adjusted PD threshold",
		"from", current, "to", suggested,
		"isl_p50", islP50, "isl_p90", islP90)

	return nil
}

// SetupWithManager registers the controller with the manager.
func (r *PDRatioPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&llmdv1alpha1.PDRatioPolicy{}).
		Complete(r)
}
