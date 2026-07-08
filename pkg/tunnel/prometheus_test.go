package tunnel

import (
	"strings"
	"testing"
)

func TestParsePrometheus_Simple(t *testing.T) {
	input := `
# HELP test_gauge A test gauge
# TYPE test_gauge gauge
test_gauge 42.5
`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["test_gauge"]; v != 42.5 {
		t.Errorf("want 42.5, got %v", v)
	}
}

func TestParsePrometheus_WithLabels(t *testing.T) {
	input := `vllm:num_requests_waiting{model_name="qwen2.5-1.5b"} 3.0
vllm:num_requests_running{model_name="qwen2.5-1.5b"} 2.0`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["vllm:num_requests_waiting"]; v != 3.0 {
		t.Errorf("waiting: want 3.0, got %v", v)
	}
	if v := m["vllm:num_requests_running"]; v != 2.0 {
		t.Errorf("running: want 2.0, got %v", v)
	}
}

func TestParsePrometheus_CounterSumsAcrossLabels(t *testing.T) {
	// Counters split by label must be SUMMED.
	input := `# TYPE foo counter
foo{a="1"} 10
foo{a="2"} 20
foo{a="3"} 30`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["foo"]; v != 60 {
		t.Errorf("counter should sum: want 60, got %v", v)
	}
}

func TestParsePrometheus_GaugeLastWins(t *testing.T) {
	// Gauges represent instantaneous values — when the same metric appears
	// under multiple label sets, last wins (summing would be nonsense).
	input := `# TYPE bar gauge
bar{a="1"} 10
bar{a="2"} 20
bar{a="3"} 30`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["bar"]; v != 30 {
		t.Errorf("gauge last-wins: want 30, got %v", v)
	}
}

func TestParsePrometheus_VLLMSuccessTotalSplitByReason(t *testing.T) {
	// Real-world vLLM exposes request_success_total split by finished_reason.
	// Total jobs done = sum across all reasons.
	input := `# TYPE vllm:request_success_total counter
vllm:request_success_total{engine="0",finished_reason="stop",model_name="qwen2.5-1.5b"} 5.0
vllm:request_success_total{engine="0",finished_reason="length",model_name="qwen2.5-1.5b"} 2.0
vllm:request_success_total{engine="0",finished_reason="abort",model_name="qwen2.5-1.5b"} 1.0
vllm:request_success_total{engine="0",finished_reason="error",model_name="qwen2.5-1.5b"} 0.0
vllm:request_success_total{engine="0",finished_reason="repetition",model_name="qwen2.5-1.5b"} 0.0`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["vllm:request_success_total"]; v != 8.0 {
		t.Errorf("want 8.0, got %v", v)
	}
}

func TestParsePrometheus_Histogram(t *testing.T) {
	input := `
# HELP vllm:e2e_request_latency_seconds E2E latency
# TYPE vllm:e2e_request_latency_seconds histogram
vllm:e2e_request_latency_seconds_bucket{le="0.5"} 5
vllm:e2e_request_latency_seconds_bucket{le="1.0"} 12
vllm:e2e_request_latency_seconds_bucket{le="+Inf"} 20
vllm:e2e_request_latency_seconds_sum 23.4
vllm:e2e_request_latency_seconds_count 20
`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["vllm:e2e_request_latency_seconds_sum"]; v != 23.4 {
		t.Errorf("sum: want 23.4, got %v", v)
	}
	if v := m["vllm:e2e_request_latency_seconds_count"]; v != 20 {
		t.Errorf("count: want 20, got %v", v)
	}
	// Bucket lines are accumulated under one key (summed across le="..."
	// labels). We don't use bucket totals — callers access sum/count only.
	if _, ok := m["vllm:e2e_request_latency_seconds_bucket"]; !ok {
		t.Error("bucket key should be present")
	}
}

func TestParsePrometheus_EmptyAndComments(t *testing.T) {
	input := `
# HELP just_a_comment
# TYPE foo counter


`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("want empty map, got %v", m)
	}
}

func TestParsePrometheus_MalformedLineSkipped(t *testing.T) {
	input := `good_metric 1.0
bad_metric not_a_number
another_good 2.5`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["good_metric"]; v != 1.0 {
		t.Errorf("good_metric: %v", v)
	}
	if v := m["another_good"]; v != 2.5 {
		t.Errorf("another_good: %v", v)
	}
	if _, ok := m["bad_metric"]; ok {
		t.Error("bad_metric should have been skipped")
	}
}

func TestParsePrometheus_QuotedBraceInLabel(t *testing.T) {
	// Label value contains "}" — make sure we don't split on it.
	input := `tricky{key="val}ue"} 99`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["tricky"]; v != 99 {
		t.Errorf("want 99, got %v", v)
	}
}

func TestParsePrometheus_TimestampIgnored(t *testing.T) {
	// Prometheus text may have "value timestamp". We only want the value.
	input := `metric_with_ts 42 1700000000000`
	m, err := ParsePrometheus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if v := m["metric_with_ts"]; v != 42 {
		t.Errorf("want 42, got %v", v)
	}
}

func TestParsePrometheus_VLLMRealSample(t *testing.T) {
	// Trimmed but realistic vLLM /metrics output.
	sample := `# HELP vllm:num_requests_running Number of requests currently running on GPU.
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 2.0
# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 3.0
# HELP vllm:num_requests_swapped Number of requests swapped to CPU.
# TYPE vllm:num_requests_swapped gauge
vllm:num_requests_swapped{model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 0.0
# HELP vllm:request_success_total Count of successfully processed requests.
# TYPE vllm:request_success_total counter
vllm:request_success_total{finished_reason="stop",model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 47.0
# HELP vllm:e2e_request_latency_seconds End to end request latency in seconds.
# TYPE vllm:e2e_request_latency_seconds histogram
vllm:e2e_request_latency_seconds_sum{model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 234.5
vllm:e2e_request_latency_seconds_count{model_name="Qwen/Qwen2.5-1.5B-Instruct-AWQ"} 47
`
	m, err := ParsePrometheus(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}

	checks := map[string]float64{
		"vllm:num_requests_running":              2.0,
		"vllm:num_requests_waiting":              3.0,
		"vllm:num_requests_swapped":              0.0,
		"vllm:request_success_total":             47.0,
		"vllm:e2e_request_latency_seconds_sum":   234.5,
		"vllm:e2e_request_latency_seconds_count": 47,
	}
	for k, want := range checks {
		if got := m[k]; got != want {
			t.Errorf("%s: want %v, got %v", k, want, got)
		}
	}

	// Derived avg — sanity.
	avg := m["vllm:e2e_request_latency_seconds_sum"] / m["vllm:e2e_request_latency_seconds_count"]
	if avg < 4.9 || avg > 5.1 {
		t.Errorf("avg derived: want ~5.0, got %v", avg)
	}
}
