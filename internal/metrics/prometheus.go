// Package metrics provides a thin Prometheus query client used by the
// pd-ratio-coordinator control loop.
package metrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// PoolMetrics holds a snapshot of the key inference metrics
// for a single pool (prefill or decode) at a point in time.
type PoolMetrics struct {
	// QueueDepth is vllm:num_requests_waiting averaged across all pods in the pool.
	QueueDepth float64

	// KVCacheUsagePct is vllm:gpu_cache_usage_perc averaged across pods (0.0-1.0).
	KVCacheUsagePct float64

	// TPOT_P95Ms is the p95 time-per-output-token in milliseconds.
	// Meaningful for decode pools.
	TPOT_P95Ms float64

	// TTFT_P95Ms is the p95 time-to-first-token in milliseconds.
	// Meaningful for prefill pools.
	TTFT_P95Ms float64

	// GenerationTokensPerSec is the output token throughput.
	GenerationTokensPerSec float64

	// SampledAt is when this snapshot was taken.
	SampledAt time.Time
}

// Client wraps the Prometheus API for inference metric queries.
type Client struct {
	api v1.API
	url string
}

// NewClient creates a Prometheus client targeting the given URL.
func NewClient(prometheusURL string) (*Client, error) {
	cfg := api.Config{Address: prometheusURL}
	c, err := api.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client: %w", err)
	}
	return &Client{api: v1.NewAPI(c), url: prometheusURL}, nil
}

// QueryPrefillMetrics fetches all relevant metrics for the prefill pool.
// podSelector example: `pod=~"ms-pd-llm-d-modelservice-prefill.*"`
func (c *Client) QueryPrefillMetrics(ctx context.Context, podSelector string) (*PoolMetrics, error) {
	return c.queryPool(ctx, podSelector, true)
}

// QueryDecodeMetrics fetches all relevant metrics for the decode pool.
// podSelector example: `pod=~"ms-pd-llm-d-modelservice-decode.*"`
func (c *Client) QueryDecodeMetrics(ctx context.Context, podSelector string) (*PoolMetrics, error) {
	return c.queryPool(ctx, podSelector, false)
}

func (c *Client) queryPool(ctx context.Context, podSelector string, isPrefill bool) (*PoolMetrics, error) {
	now := time.Now()
	m := &PoolMetrics{SampledAt: now}
	var err error

	// Queue depth — avg across all matching pods
	m.QueueDepth, err = c.scalar(ctx,
		fmt.Sprintf(`avg(vllm:num_requests_waiting{%s})`, podSelector))
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}

	// KV cache utilization
	// Metric name verified against a real v0.30.0 server (2026-10-01):
	// vllm:gpu_cache_usage_perc was renamed to vllm:kv_cache_usage_perc.
	// Same rename already documented as a gotcha in Part 2 of the blog
	// series, on a different vLLM version -- this has been drifting for a
	// while, not a one-off.
	m.KVCacheUsagePct, err = c.scalar(ctx,
		fmt.Sprintf(`avg(vllm:kv_cache_usage_perc{%s})`, podSelector))
	if err != nil {
		return nil, fmt.Errorf("kv cache: %w", err)
	}

	// TPOT p95 (decode signal)
	// Metric name verified against a real v0.30.0 server (2026-10-01):
	// vllm:time_per_output_token_seconds was renamed to
	// vllm:request_time_per_output_token_seconds.
	m.TPOT_P95Ms, err = c.scalar(ctx, fmt.Sprintf(
		`histogram_quantile(0.95, sum(rate(vllm:request_time_per_output_token_seconds_bucket{%s}[2m])) by (le)) * 1000`,
		podSelector))
	if err != nil {
		return nil, fmt.Errorf("tpot p95: %w", err)
	}

	// TTFT p95 (prefill signal)
	m.TTFT_P95Ms, err = c.scalar(ctx, fmt.Sprintf(
		`histogram_quantile(0.95, sum(rate(vllm:time_to_first_token_seconds_bucket{%s}[2m])) by (le)) * 1000`,
		podSelector))
	if err != nil {
		return nil, fmt.Errorf("ttft p95: %w", err)
	}

	// Token throughput
	m.GenerationTokensPerSec, err = c.scalar(ctx,
		fmt.Sprintf(`sum(rate(vllm:generation_tokens_total{%s}[1m]))`, podSelector))
	if err != nil {
		return nil, fmt.Errorf("throughput: %w", err)
	}

	return m, nil
}

// QueryQueueHistory returns queue depth values over the last windowSeconds.
// Used to compute velocity (rate of change) for spike detection.
func (c *Client) QueryQueueHistory(ctx context.Context, podSelector string, windowSeconds int32) ([]float64, error) {
	r := v1.Range{
		Start: time.Now().Add(-time.Duration(windowSeconds) * time.Second),
		End:   time.Now(),
		Step:  5 * time.Second,
	}

	query := fmt.Sprintf(`avg(vllm:num_requests_waiting{%s})`, podSelector)
	result, _, err := c.api.QueryRange(ctx, query, r)
	if err != nil {
		return nil, fmt.Errorf("queue history: %w", err)
	}

	matrix, ok := result.(model.Matrix)
	if !ok || len(matrix) == 0 {
		return nil, nil
	}

	values := make([]float64, 0, len(matrix[0].Values))
	for _, v := range matrix[0].Values {
		f, err := strconv.ParseFloat(v.Value.String(), 64)
		if err == nil {
			values = append(values, f)
		}
	}
	return values, nil
}

// QueryISLPercentiles fetches input sequence length percentiles from the
// vllm:prompt_tokens histogram, used for dynamic threshold adjustment.
func (c *Client) QueryISLPercentiles(ctx context.Context, podSelector string) (p50, p90 float64, err error) {
	p50, err = c.scalar(ctx, fmt.Sprintf(
		`histogram_quantile(0.50, sum(rate(vllm:request_prompt_tokens_bucket{%s}[5m])) by (le))`,
		podSelector))
	if err != nil {
		return 0, 0, fmt.Errorf("isl p50: %w", err)
	}

	p90, err = c.scalar(ctx, fmt.Sprintf(
		`histogram_quantile(0.90, sum(rate(vllm:request_prompt_tokens_bucket{%s}[5m])) by (le))`,
		podSelector))
	if err != nil {
		return 0, 0, fmt.Errorf("isl p90: %w", err)
	}

	return p50, p90, nil
}

// scalar executes an instant PromQL query and returns the scalar result.
// Returns 0 with no error when no data is available (no pods scraped yet).
func (c *Client) scalar(ctx context.Context, query string) (float64, error) {
	result, _, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, err
	}

	switch v := result.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, nil
		}
		return float64(v[0].Value), nil
	case *model.Scalar:
		return float64(v.Value), nil
	default:
		return 0, nil
	}
}
