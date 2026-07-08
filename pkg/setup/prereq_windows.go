//go:build windows

package setup

// prereq_windows.go — Windows host 의 OS prereq 검출 (read-only).
//
// Windows 에서 docker 는 WSL2 안에서 돈다. 따라서 검출 순서는:
//   WSL2 feature → Linux distro → (WSL 안) docker → (WSL 안) toolkit → 드라이버
//
// **WSL 안 깨움 원칙**: distro/state 조회(`wsl --status`, `wsl -l -v`)는 cold-boot
// 를 유발하지 않는다. 그러나 `wsl -e <cmd>`(WSL 안에서 실행)는 정지된 distro 를
// 부팅시킨다 → docker/toolkit in-WSL probe 는 **타깃 distro 가 이미 Running 일 때만** 한다.
// 정지 상태면 "unknown" 으로 보고하고 boot 안 함 (isannd QuickProbe 와 동일 정책).
//
// **Docker Desktop 공존**: docker-desktop/-data 관리 distro 는 iSANN 의 native
// docker 타깃이 아니다 — distro 목록/probe 에서 제외해 WSLDistro()/docker
// readiness 가 절대 Desktop distro 를 가리키지 않게 한다. docker probe 는
// /usr/bin/docker + `-H unix:///var/run/docker.sock` 로 native dockerd 만 본다
// (Desktop 의 WSL-integration shim/context 우회 — runtime cli.go 와 동일 핀).
// 설계: docs/TODO/docker-warmup-wsl.md §3.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// wslUTF8Env — `wsl --status` / `wsl -l -v` 가 UTF-16LE 대신 UTF-8 로 출력하게
// 함. 이게 없으면 문자열 파서가 출력을 못 읽는다 (WSL Aug 2022+ 지원).
var wslUTF8Env = append(os.Environ(), "WSL_UTF8=1")

const wslProbeTimeout = 5 * time.Second

// native docker 핀 — Docker Desktop WSL-integration 우회용 (cli.go 와 동일 값).
//   - wslNativeDockerBin: 절대경로로 PATH shim 우회.
//   - wslNativeDockerHost: -H 소켓으로 docker context(desktop-linux) 우선 우회.
const (
	wslNativeDockerBin  = "/usr/bin/docker"
	wslNativeDockerHost = "unix:///var/run/docker.sock"
)

// hideWSLWindow — wsl.exe 를 부모 콘솔에 붙이지 않게 한다. 반복 호출 시
// 콘솔 상태 오염/창 깜빡임 방지 (docker pkg 의 hideChildConsole 와 동일).
func hideWSLWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}

// wslOutput 은 wsl 서브커맨드를 UTF-8 env + 콘솔 숨김 + timeout 으로 실행하고
// stdout 을 반환한다. 실패 시 (out, err).
func wslOutput(ctx context.Context, args ...string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, wslProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "wsl", args...)
	cmd.Env = wslUTF8Env
	hideWSLWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}

func detectPrereqs(ctx context.Context) ([]PrereqComponent, *DesktopInfo) {
	var comps []PrereqComponent

	wslPresent := wslFeaturePresent(ctx)
	all := wslDistros(ctx)

	// Docker Desktop 관리 distro 제외 → realDistros. 타깃 = 첫 real distro
	// (WSLDistro() 가 Detail 첫 항목을 쓰는 것과 정합). docker/toolkit probe 는
	// 타깃 distro 한정 + 그 distro 가 Running 일 때만. 동시에 Docker Desktop
	// 설치/실행 여부도 같은 목록에서 수집(진단용).
	var realDistros []string
	targetDistro := ""
	targetRunning := false
	desktopInstalled := dockerDesktopInstalledExe()
	desktopRunning := false
	for _, d := range all {
		if isDockerDesktopDistro(d.Name) {
			desktopInstalled = true
			if d.Running {
				desktopRunning = true
			}
			continue
		}
		realDistros = append(realDistros, d.Name)
		if targetDistro == "" {
			targetDistro = d.Name
			targetRunning = d.Running
		}
	}

	// (1) WSL2 feature
	if wslPresent {
		comps = append(comps, PrereqComponent{
			Name: PrereqWSL2, Status: PrereqOK, AutoFix: true, Detail: "available",
		})
	} else {
		comps = append(comps, PrereqComponent{
			Name: PrereqWSL2, Status: PrereqMissing, AutoFix: true,
			Detail: "run 'wsl --install' (reboot required on first enable)",
		})
	}

	// (2) Linux distro — Docker Desktop distro 제외한 realDistros 기준.
	switch {
	case len(realDistros) > 0:
		comps = append(comps, PrereqComponent{
			Name: PrereqDistro, Status: PrereqOK, AutoFix: true,
			Detail: strings.Join(realDistros, ", "),
		})
	case !wslPresent:
		// WSL 자체가 없으면 distro 도 당연히 없음 — 중복 경고 줄임.
		comps = append(comps, PrereqComponent{
			Name: PrereqDistro, Status: PrereqUnknown, AutoFix: true, Detail: "WSL required first",
		})
	default:
		comps = append(comps, PrereqComponent{
			Name: PrereqDistro, Status: PrereqMissing, AutoFix: true,
			Detail: "Ubuntu-22.04 not installed (wsl --install -d Ubuntu-22.04)",
		})
	}

	// (3) Docker Engine (WSL 안). 타깃 distro 가 Running 일 때만 in-WSL probe.
	// /var/run/docker.sock 의 데몬 정체성도 같이 본다 — Desktop 이 그 소켓을
	// 점유하면 native 아닌 Desktop 버전이 응답해 "ready" 오판이 되므로(runtime
	// 정체성 가드의 check 버전), isDesktop 이면 native 미준비 + socketConflict.
	docker := PrereqComponent{Name: PrereqDocker, AutoFix: true}
	socketConflict := false
	switch {
	case !wslPresent || targetDistro == "":
		docker.Status = PrereqUnknown
		docker.Detail = "needs WSL/distro first"
	case !targetRunning:
		docker.Status = PrereqUnknown
		docker.Detail = "WSL stopped - run 'isann docker warmup' then re-check"
	default:
		ver, isDesktop := dockerDaemonInWSL(ctx, targetDistro)
		switch {
		case isDesktop:
			socketConflict = true
			docker.Status = PrereqMissing
			docker.Detail = "native docker-ce not ready - /var/run/docker.sock served by Docker Desktop"
		case ver != "":
			docker.Status = PrereqOK
			docker.Detail = "engine " + ver + " (WSL)"
		default:
			docker.Status = PrereqMissing
			docker.Detail = "docker not installed/running in WSL"
		}
	}
	comps = append(comps, docker)

	// (4) nvidia-container-toolkit (WSL 안). 마찬가지로 타깃 distro Running 일 때만.
	toolkit := PrereqComponent{Name: PrereqToolkit, AutoFix: true}
	switch {
	case !wslPresent || targetDistro == "":
		toolkit.Status = PrereqUnknown
		toolkit.Detail = "needs WSL/distro first"
	case !targetRunning:
		toolkit.Status = PrereqUnknown
		toolkit.Detail = "WSL stopped - warmup then re-check"
	case toolkitInWSL(ctx, targetDistro):
		toolkit.Status = PrereqOK
		toolkit.Detail = "nvidia-container-runtime present (WSL)"
	default:
		toolkit.Status = PrereqMissing
		toolkit.Detail = "for GPU containers (optional on CPU-only nodes)"
	}
	comps = append(comps, toolkit)

	// (5) NVIDIA 드라이버 (host) — 검출 전용.
	comps = append(comps, driverComponent())

	desktop := &DesktopInfo{
		Installed:      desktopInstalled,
		Running:        desktopRunning,
		SocketConflict: socketConflict,
	}
	return comps, desktop
}

// wslFeaturePresent 는 WSL2 가 구성돼 있는지 본다. `wsl --status` 가 성공하면
// (default version 등을 출력) WSL feature 활성으로 본다. 미설치 머신에서는
// wsl.exe 가 install 안내를 출력하며 non-zero 로 끝나거나 stdout 이 빈다.
func wslFeaturePresent(ctx context.Context) bool {
	if _, err := exec.LookPath("wsl"); err != nil {
		return false
	}
	out, err := wslOutput(ctx, "--status")
	if err != nil {
		// freshly-installed-but-never-launched 면 non-zero 일 수 있으나, 그 경우
		// distro 조회(wslDistros)가 권위 소스. 여기선 stdout 유무로 보조 판정.
		return strings.TrimSpace(stripBOM(out)) != ""
	}
	return true
}

// wslDistro — `wsl -l -v` 한 행 (이름 + Running 여부).
type wslDistro struct {
	Name    string
	Running bool
}

// wslDistros 는 `wsl -l -v` 로 설치된 distro 들과 각 Running 여부를 반환한다.
// listing 은 distro 를 cold-boot 시키지 않는다 (read-only).
func wslDistros(ctx context.Context) []wslDistro {
	out, err := wslOutput(ctx, "-l", "-v")
	if err != nil {
		return nil
	}
	var ds []wslDistro
	for _, line := range strings.Split(stripBOM(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// 선두 '*' (default 표시) 제거.
		if fields[0] == "*" {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		// 헤더 줄 (NAME STATE VERSION) skip — distro 이름이 "NAME" 일 수 없음.
		if strings.EqualFold(fields[0], "NAME") {
			continue
		}
		d := wslDistro{Name: fields[0]}
		for _, tok := range fields[1:] {
			if strings.EqualFold(tok, "Running") {
				d.Running = true
			}
		}
		ds = append(ds, d)
	}
	return ds
}

// isDockerDesktopDistro 는 Docker Desktop 이 관리하는 WSL distro 인지 본다
// (docker-desktop / docker-desktop-data, 대소문자 무시). 이 distro 들은
// iSANN 의 native docker 타깃이 아니므로 distro 선택/probe 에서 제외된다.
func isDockerDesktopDistro(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "docker-desktop", "docker-desktop-data":
		return true
	}
	return false
}

// DockerDesktopRunning 은 Docker Desktop 의 WSL distro 가 현재 Running 인지
// (= Desktop 앱이 실행 중인지) 본다. `ivm setup` 의 가드용: Desktop 활성 중
// WSL 에 native docker 를 설치하면 PATH/socket 혼동이 생기므로, setup 은
// 설치를 막고 종료를 요구한다. STATE 기반(wsl -l -v) — 프로세스 열거 X,
// cold-boot X. 설계: docs/TODO/docker-warmup-wsl.md §4.
func DockerDesktopRunning(ctx context.Context) bool {
	for _, d := range wslDistros(ctx) {
		if isDockerDesktopDistro(d.Name) && d.Running {
			return true
		}
	}
	return false
}

// wslNativeDockerArgs 는 `wsl` 다음에 올 인자(맨 앞 "wsl" 제외)를 만든다 —
// distro 한정 + native docker 핀(/usr/bin/docker + -H 소켓). runtime(cli.go)
// 의 3중 핀과 동일 타깃이라 check/status 가 실제 op 와 같은 데몬을 본다.
// distro=="" → 기본 distro.
func wslNativeDockerArgs(distro string, dockerArgs ...string) []string {
	a := []string{}
	if distro != "" {
		a = append(a, "-d", distro)
	}
	a = append(a, "-e", wslNativeDockerBin, "-H", wslNativeDockerHost)
	return append(a, dockerArgs...)
}

// dockerDaemonInWSL 는 지정 distro 안에서 /var/run/docker.sock 의 데몬 정보를
// 본다 — native docker 핀(/usr/bin/docker + -H)로 `docker info` 한 번에 버전 +
// 정체성(OperatingSystem/Name)을 받는다. isDesktop=true 면 그 소켓을 Docker
// Desktop 이 서빙 중(native dockerd 아님). 타깃 distro 가 이미 Running 일 때만
// 호출되므로 cold-boot 안 함. 빈 버전 = docker 없음/미기동.
func dockerDaemonInWSL(ctx context.Context, distro string) (version string, isDesktop bool) {
	out, err := wslOutput(ctx, wslNativeDockerArgs(distro, "info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}|{{.Name}}")...)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(stripBOM(out))
	if line == "" {
		return "", false
	}
	parts := strings.SplitN(line, "|", 3)
	version = strings.TrimSpace(parts[0])
	id := ""
	if len(parts) >= 2 {
		id += parts[1]
	}
	if len(parts) >= 3 {
		id += " " + parts[2]
	}
	id = strings.ToLower(id)
	isDesktop = strings.Contains(id, "docker desktop") || strings.Contains(id, "docker-desktop")
	return version, isDesktop
}

// dockerDesktopInstalledExe 는 Docker Desktop 실행파일이 표준 위치에 있는지
// 본다 — WSL2 백엔드 distro 가 아직 등록 안 된(설치 후 미실행) 케이스 보강.
// 검출 전용 (실행 X).
func dockerDesktopInstalledExe() bool {
	for _, env := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
		root := os.Getenv(env)
		if root == "" {
			continue
		}
		if _, err := os.Stat(root + `\Docker\Docker\Docker Desktop.exe`); err == nil {
			return true
		}
	}
	return false
}

// toolkitInWSL 는 지정 distro 안에 nvidia-container-runtime 이 있는지 본다.
func toolkitInWSL(ctx context.Context, distro string) bool {
	a := []string{}
	if distro != "" {
		a = append(a, "-d", distro)
	}
	a = append(a, "-e", "sh", "-c", "command -v nvidia-container-runtime || true")
	out, err := wslOutput(ctx, a...)
	if err != nil {
		return false
	}
	return strings.TrimSpace(stripBOM(out)) != ""
}

// stripBOM 은 UTF-8/UTF-16 BOM 잔재(U+FEFF)를 떼낸다 (wsl 출력 방어).
func stripBOM(s string) string {
	return strings.TrimPrefix(s, string(rune(0xFEFF)))
}
