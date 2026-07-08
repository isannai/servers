//go:build !windows

package setup

// prereq_other.go — Linux/macOS host 의 OS prereq 검출 (read-only).
//
// WSL 은 Windows 전용 개념이므로 N/A 로 보고하고, docker / nvidia-container-toolkit
// 은 host 에서 직접 본다. 아무것도 설치/생성하지 않는다.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

const nativeProbeTimeout = 5 * time.Second

func detectPrereqs(ctx context.Context) ([]PrereqComponent, *DesktopInfo) {
	var comps []PrereqComponent

	// (1) WSL2 / (2) distro — Linux/mac 엔 해당 없음.
	comps = append(comps,
		PrereqComponent{Name: PrereqWSL2, Status: PrereqNA, AutoFix: false, Detail: "Windows only"},
		PrereqComponent{Name: PrereqDistro, Status: PrereqNA, AutoFix: false, Detail: "Windows only"},
	)

	// (3) Docker Engine — host 에서 직접. `docker version` 의 Server.Version 이
	// 나오면 데몬 도달 가능 (= 진짜 ready). client 만 있고 데몬이 죽었으면
	// installed-but-not-running 으로 구분.
	docker := PrereqComponent{Name: PrereqDocker, AutoFix: true}
	if _, err := exec.LookPath("docker"); err != nil {
		docker.Status = PrereqMissing
		docker.Detail = "docker not installed"
	} else if v := dockerServerVersion(ctx); v != "" {
		docker.Status = PrereqOK
		docker.Detail = "engine " + v
	} else {
		docker.Status = PrereqMissing
		docker.Detail = "docker installed, daemon not running (systemctl start docker)"
	}
	comps = append(comps, docker)

	// (4) nvidia-container-toolkit — nvidia-container-runtime 바이너리 유무.
	toolkit := PrereqComponent{Name: PrereqToolkit, AutoFix: true}
	if nvidiaContainerToolkitPresent() {
		toolkit.Status = PrereqOK
		toolkit.Detail = "nvidia-container-runtime present"
	} else {
		toolkit.Status = PrereqMissing
		toolkit.Detail = "for GPU containers (optional on CPU-only nodes)"
	}
	comps = append(comps, toolkit)

	// (5) NVIDIA 드라이버 (host) — 검출 전용.
	comps = append(comps, driverComponent())

	// Docker Desktop 진단은 Windows 전용 (WSL distro 개념). 다른 OS 면 nil.
	return comps, nil
}

// DockerDesktopRunning 은 non-Windows 에선 항상 false — Docker Desktop 의 WSL
// distro 는 Windows 전용 개념이다. `ivm setup` 의 Desktop 가드는 Windows 에서만
// 발화한다. (cross-platform 시그니처 유지용 스텁.)
func DockerDesktopRunning(ctx context.Context) bool { return false }

// dockerServerVersion 은 docker 데몬의 Server.Version 을 반환한다. 데몬 미도달이면
// 빈 문자열 ('docker version --format {{.Server.Version}}' 가 client 만 있을 때 실패).
func dockerServerVersion(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, nativeProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// nvidiaContainerToolkitPresent 는 nvidia-container-runtime 이 PATH 또는 표준
// 위치에 있는지 본다. apt 로 깔면 /usr/bin/nvidia-container-runtime 에 놓인다.
func nvidiaContainerToolkitPresent() bool {
	if _, err := exec.LookPath("nvidia-container-runtime"); err == nil {
		return true
	}
	for _, p := range []string{
		"/usr/bin/nvidia-container-runtime",
		"/usr/local/bin/nvidia-container-runtime",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
