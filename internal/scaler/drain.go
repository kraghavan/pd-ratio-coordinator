// Package scaler - drain.go handles graceful scale-down of decode pods.
//
// Problem: terminating a decode pod mid-sequence drops all active KV cache
// sequences on that pod → user-visible request failures.
//
// Solution: before reducing decode replicas, label the target pod as draining
// (EPP stops routing new requests to it), wait for in-flight sequences to
// complete, then allow Kubernetes to terminate it.
package scaler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DrainLabel is added to pods being drained.
	// The EPP InferencePool selector excludes pods with this label,
	// stopping new requests from being routed to a draining pod.
	DrainLabel = "llmd.io/draining"

	// DrainLabelValue is the value set on draining pods.
	DrainLabelValue = "true"

	// vLLM metrics port
	vllmMetricsPort = "8000"
)

// DrainManager coordinates graceful decode pod scale-down.
type DrainManager struct {
	client       client.Client
	drainTimeout time.Duration
	pollInterval time.Duration
	httpClient   *http.Client
}

// NewDrainManager creates a DrainManager.
func NewDrainManager(c client.Client, drainTimeoutSeconds int32) *DrainManager {
	return &DrainManager{
		client:       c,
		drainTimeout: time.Duration(drainTimeoutSeconds) * time.Second,
		pollInterval: 5 * time.Second,
		httpClient:   &http.Client{Timeout: 3 * time.Second},
	}
}

// DrainAndWait gracefully removes a decode pod from the EPP routing pool
// and waits for in-flight sequences to complete before returning.
//
// Step 1: Label the pod with DrainLabel → EPP stops routing to it.
// Step 2: Poll vllm:num_requests_running on pod IP until it reaches 0.
// Step 3: Return nil → caller issues scale-down.
//
// If drainTimeout is exceeded, returns an error — caller logs and force-terminates.
func (d *DrainManager) DrainAndWait(ctx context.Context, pod corev1.Pod) error {
	if err := d.labelPodDraining(ctx, pod); err != nil {
		return fmt.Errorf("labelling pod %s for drain: %w", pod.Name, err)
	}

	deadline := time.Now().Add(d.drainTimeout)
	for time.Now().Before(deadline) {
		running, err := d.runningRequestCount(ctx, pod)
		if err == nil && running == 0 {
			return nil // clean drain
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.pollInterval):
		}
	}

	return fmt.Errorf("drain timeout after %s for pod %s", d.drainTimeout, pod.Name)
}

// RemoveDrainLabel removes the drain label — used when a scale-down is
// cancelled (e.g. a new load spike arrives during drain).
func (d *DrainManager) RemoveDrainLabel(ctx context.Context, pod corev1.Pod) error {
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Labels, DrainLabel)
	return d.client.Patch(ctx, &pod, patch)
}

// labelPodDraining adds the drain label to a pod.
func (d *DrainManager) labelPodDraining(ctx context.Context, pod corev1.Pod) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[DrainLabel] = DrainLabelValue
	return d.client.Patch(ctx, &pod, patch)
}

// runningRequestCount queries the pod's local /metrics endpoint directly
// to get vllm:num_requests_running for that specific pod.
//
// Direct pod scrape (not via Prometheus aggregation) gives us the exact
// per-pod value without Prometheus scrape interval lag.
func (d *DrainManager) runningRequestCount(ctx context.Context, pod corev1.Pod) (int, error) {
	if pod.Status.PodIP == "" {
		return 0, fmt.Errorf("pod %s has no IP yet", pod.Name)
	}

	url := fmt.Sprintf("http://%s:%s/metrics", pod.Status.PodIP, vllmMetricsPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("scraping pod metrics: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	return parseRunningRequests(string(body)), nil
}

// parseRunningRequests extracts vllm:num_requests_running from raw metrics text.
func parseRunningRequests(metricsBody string) int {
	for _, line := range strings.Split(metricsBody, "\n") {
		if strings.HasPrefix(line, "vllm:num_requests_running{") {
			// Line format: vllm:num_requests_running{engine="0",...} 3.0
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				val, err := strconv.ParseFloat(parts[len(parts)-1], 64)
				if err == nil {
					return int(val)
				}
			}
		}
	}
	return 0
}
