package setup

// prereq.go — OS prerequisite 검출 (read-only). Phase 2 §7 M1.
//
// `ivm check` 와 (장차) isannd docker status 가 공유하는 단일 검출기.
// **read-only**: 아무것도 설치 안 하고, 파일도 안 만들고, WSL 도 안 깨운다
// (정지된 WSL 안의 docker/toolkit 은 "unknown" 으로 보고 — cold-boot 회피).
//
// 실제 OS-특수 probe 는 build-tagged detectPrereqs():
//   - prereq_windows.go : wsl.exe 기반 (WSL2/distro/docker-in-WSL/toolkit)
//   - prereq_other.go   : Linux/mac native (docker/toolkit 직접)
//
// driverComponent 만 cross-platform (nvidia-smi via detectGPU, detect.go).
//
// 설계: docs/TODO/isann-cli-phase2-stage7-install.md

import (
	"context"
	"runtime"
	"strings"
)

// PrereqState — 한 component 의 검출 결과.
type PrereqState string

const (
	PrereqOK      PrereqState = "ok"      // 설치됨 + (해당되면) 동작 확인
	PrereqMissing PrereqState = "missing" // 빠짐 — setup 대상이거나 수동 안내
	PrereqNA      PrereqState = "n/a"     // 이 OS 에 해당 없음 (예: Linux 의 WSL)
	PrereqUnknown PrereqState = "unknown" // 확인 불가 (예: WSL 정지 → 안 깨우고 못 봄)
)

// 검출하는 component 이름 — 검출/렌더가 공유하는 안정 키.
const (
	PrereqWSL2    = "WSL2"
	PrereqDistro  = "Linux distro"
	PrereqDocker  = "Docker Engine"
	PrereqToolkit = "nvidia-container-toolkit"
	PrereqDriver  = "NVIDIA driver"
)

// PrereqComponent — 한 prereq 의 상태 한 줄.
type PrereqComponent struct {
	Name    string      `json:"name"`
	Status  PrereqState `json:"status"`
	Detail  string      `json:"detail,omitempty"`
	AutoFix bool        `json:"auto_fix"` // true = `ivm setup` 이 설치 가능. false = 수동/검출만 (드라이버 등)
}

// PrereqStatus — 전체 검출 결과.
type PrereqStatus struct {
	OS          string            `json:"os"`
	Components  []PrereqComponent `json:"components"`
	DockerReady bool              `json:"docker_ready"`              // bottom line — docker 데몬 도달 가능
	Desktop     *DesktopInfo      `json:"docker_desktop,omitempty"` // Docker Desktop 진단 (Windows 전용, 그 외 nil)
}

// DesktopInfo — Docker Desktop 진단 (Windows 전용). Docker Desktop 은 iSANN 의
// 백엔드가 아니다 (native WSL docker 만 사용) — 설치/실행/소켓점유는 잠재 충돌
// 신호일 뿐이라 `ivm check` 가 안내한다. 설계: docs/TODO/docker-warmup-wsl.md §4.
type DesktopInfo struct {
	Installed      bool `json:"installed"`       // docker-desktop distro 존재 또는 Desktop 실행파일 존재
	Running        bool `json:"running"`         // docker-desktop distro 가 Running (= 앱 실행 중)
	SocketConflict bool `json:"socket_conflict"` // /var/run/docker.sock 을 native 아닌 Desktop 이 서빙 (target distro Running 일 때만 판정)
}

// DetectPrereqs 는 현재 머신의 OS prereq 를 read-only 로 검출한다.
// 아무 부수효과 없음 (설치 X, 파일 X, WSL cold-boot X).
func DetectPrereqs(ctx context.Context) PrereqStatus {
	comps, desktop := detectPrereqs(ctx) // detectPrereqs: build-tagged
	st := newPrereqStatus(runtime.GOOS, comps)
	st.Desktop = desktop
	return st
}

// newPrereqStatus 는 component 목록에서 PrereqStatus 를 조립한다 — DockerReady
// (bottom line)는 Docker component 가 OK 일 때만 true. OS-호출과 분리돼 단위
// 테스트 가능 (verdict/exit-code 계약 고정).
func newPrereqStatus(osName string, comps []PrereqComponent) PrereqStatus {
	st := PrereqStatus{OS: osName, Components: comps}
	for _, c := range comps {
		if c.Name == PrereqDocker && c.Status == PrereqOK {
			st.DockerReady = true
		}
	}
	return st
}

// WSLDistro 는 설치된 첫 WSL distro 이름을 돌려준다 (없으면 ""). `ivm setup`
// 이 이 값을 ps1 의 -Distro 로 넘겨, 새 기본 distro(22.04)를 깔지 않고 기존
// distro(예: 20.04)를 재사용하게 한다. distro component 의 Detail 은 Status==OK
// 일 때 콤마-조인된 distro 이름들(prereq_windows.go 에서 설정) — 그 첫 항목.
//
// Docker Desktop 관리 distro(docker-desktop/-data)는 prereq_windows.go 의
// detectPrereqs 가 Detail 빌드 시 이미 제외하므로, WSLDistro() 는 절대
// Desktop distro 를 반환하지 않는다 (설계 docker-warmup-wsl.md §3).
func (s PrereqStatus) WSLDistro() string {
	for _, c := range s.Components {
		if c.Name == PrereqDistro && c.Status == PrereqOK {
			name := c.Detail
			if i := strings.IndexByte(name, ','); i >= 0 {
				name = name[:i]
			}
			return strings.TrimSpace(name)
		}
	}
	return ""
}

// MissingAutoFix 는 `ivm setup` 이 설치할 수 있는데 빠진 component 이름들을
// 반환한다 (드라이버 같은 검출-전용은 제외). 비어 있으면 setup 으로 채울 게 없음.
func (s PrereqStatus) MissingAutoFix() []string {
	var out []string
	for _, c := range s.Components {
		if c.AutoFix && c.Status == PrereqMissing {
			out = append(out, c.Name)
		}
	}
	return out
}

// driverComponent 은 nvidia-smi(via detectGPU) 로 NVIDIA 드라이버 유무를 본다.
// **검출 전용** — `ivm setup` 은 드라이버를 자동 설치하지 않는다 (하드웨어·버전
// 다양 + 잘못 깔면 디스플레이 brick 위험). CPU-only 노드에서는 없는 게 정상.
func driverComponent() PrereqComponent {
	gpu := detectGPU()
	if !gpu.Available {
		return PrereqComponent{
			Name:    PrereqDriver,
			Status:  PrereqMissing,
			AutoFix: false,
			Detail:  "nvidia-smi not found - install driver manually for GPU nodes (ignore on CPU-only nodes)",
		}
	}
	detail := gpu.Name
	if gpu.DriverVersion != "" {
		detail += " (driver " + gpu.DriverVersion
		if gpu.CUDAVersion != "" {
			detail += ", CUDA " + gpu.CUDAVersion
		}
		detail += ")"
	}
	return PrereqComponent{Name: PrereqDriver, Status: PrereqOK, AutoFix: false, Detail: detail}
}
