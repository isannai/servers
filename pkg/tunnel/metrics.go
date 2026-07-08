package tunnel

// ServiceMetrics — provider/broker 가 서비스 상태 변화 시 isannd 한테 push.
// HTTP body 는 한 서비스의 한 시점 snapshot. RV 가 받아서 proxies 캐시의
// per-service metrics 영역을 patch (전체 덮어쓰기 X — Service 명 기준 patch).
type ServiceMetrics struct {
	// Service — "sd-api", "llm-api", "vllm-api" 등. 빈 값이면 node-level
	// 알림 (현재는 사용처 없음, 향후 확장 자리).
	Service       string  `json:"service"`
	Status        string  `json:"status"` // "idle" / "busy" / "loading" / "stopped"
	QueueDepth    uint32  `json:"queue_depth,omitempty"`
	RunningCount  uint32  `json:"running_count,omitempty"`
	TotalJobsDone uint64  `json:"total_jobs_done,omitempty"`
	AvgJobSec     float32 `json:"avg_job_sec,omitempty"`
	LastJobMs     int64   `json:"last_job_ms,omitempty"`
	RunningJobID  string  `json:"running_job_id,omitempty"`
	TimestampMs   int64   `json:"timestamp_ms"` // 송신 시각
}

// MetricsEvent — backend → isannd 의 /internal/rv/metrics POST body.
// node 식별 + 변경된 서비스의 새 상태.
type MetricsEvent struct {
	NodeID  string         `json:"id"`   // "P:0x..." / "B:0x..."
	Role    string         `json:"role"` // "provider" / "broker"
	Service ServiceMetrics `json:"service"`
}

// HeartbeatPing — backend → isannd 의 /internal/rv/heartbeat POST body.
// liveness ping. Signature 는 PingDigest(role, timestampMs) 에 대한 EOA
// ECDSA 서명. RV 가 ecrecover 로 EOA 복구 후 NodeID 의 EOA 와 매칭하여
// 진위 확인. NodeID 는 isannd → RV ping frame 의 ID 필드를 채우기 위해
// 함께 보내지만, 인증 자체는 서명+ecrecover 결과만 신뢰.
type HeartbeatPing struct {
	NodeID      string `json:"id"`
	Role        string `json:"role"`
	TimestampMs int64  `json:"timestamp_ms"`
	Signature   []byte `json:"signature"`
}
