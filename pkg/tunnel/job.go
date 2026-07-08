package tunnel

// JobProgress is a single progress update pushed from Provider to Broker.
type JobProgress struct {
	JobID    string `json:"job_id"`
	Status   string `json:"status"`              // "preparing" | "running" | "done" | "failed"
	Progress int    `json:"progress"`            // 0-100
	URL      string `json:"url,omitempty"`       // done 시 결과 이미지 URL
	B64Image string `json:"b64_image,omitempty"` // done 시 base64 이미지
	Token    string `json:"token,omitempty"`
	Seed     int64  `json:"actual_seed,omitempty"`
	Error    string `json:"error,omitempty"` // failed 시 에러 메시지
}
