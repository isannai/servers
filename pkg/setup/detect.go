package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/cpuid/v2"
)

// SystemInfo holds detected system capabilities (lightweight, for asset selection).
type SystemInfo struct {
	OS        string // runtime.GOOS
	Arch      string // runtime.GOARCH
	HasCUDA   bool
	HasAVX    bool
	HasAVX2   bool
	HasAVX512 bool
}

// FullSystemInfo holds detailed hardware specs for API response.
type FullSystemInfo struct {
	OS   string   `json:"os"`
	Arch string   `json:"arch"`
	CPU  CPUInfo  `json:"cpu"`
	Mem  MemInfo  `json:"memory"`
	GPU  GPUInfo  `json:"gpu"`
	Disk DiskInfo `json:"disk"`
}

type CPUInfo struct {
	Name     string  `json:"name"`
	Cores    int     `json:"cores"`
	Threads  int     `json:"threads"`
	ClockMHz float64 `json:"clock_mhz"`
	AVX      bool    `json:"avx"`
	AVX2     bool    `json:"avx2"`
	AVX512   bool    `json:"avx512"`
}

type MemInfo struct {
	TotalGB float64 `json:"total_gb"`
}

type GPUInfo struct {
	Available     bool    `json:"available"`
	Name          string  `json:"name,omitempty"`
	VRAMGB        float64 `json:"vram_gb,omitempty"`
	CUDASupport   bool    `json:"cuda_support"`
	DriverVersion string  `json:"driver_version,omitempty"`
	CUDAVersion   string  `json:"cuda_version,omitempty"`
}

type DiskInfo struct {
	ModelDir string  `json:"model_dir"`
	FreeGB   float64 `json:"free_gb"`
}

// DetectSystem probes the current machine for OS, arch, GPU, and CPU features.
func DetectSystem() SystemInfo {
	sys := SystemInfo{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		HasAVX:    cpuid.CPU.Supports(cpuid.AVX),
		HasAVX2:   cpuid.CPU.Supports(cpuid.AVX2),
		HasAVX512: cpuid.CPU.Supports(cpuid.AVX512F),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "nvidia-smi").Run(); err == nil {
		sys.HasCUDA = hasCUDA12Runtime()
	}

	return sys
}

// DetectSystemFull returns detailed hardware specs.
func DetectSystemFull(modelDir string) FullSystemInfo {
	info := FullSystemInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
		CPU: CPUInfo{
			Name:     cpuid.CPU.BrandName,
			Cores:    cpuid.CPU.PhysicalCores,
			Threads:  runtime.NumCPU(),
			ClockMHz: detectCPUClock(),
			AVX:      cpuid.CPU.Supports(cpuid.AVX),
			AVX2:     cpuid.CPU.Supports(cpuid.AVX2),
			AVX512:   cpuid.CPU.Supports(cpuid.AVX512F),
		},
		Mem:  detectMemory(),
		GPU:  detectGPU(),
		Disk: detectDisk(modelDir),
	}
	return info
}

func detectGPU() GPUInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return GPUInfo{Available: false}
	}

	line := strings.TrimSpace(string(out))
	// Can have multiple GPUs, take first line
	if idx := strings.IndexByte(line, '\n'); idx > 0 {
		line = line[:idx]
	}
	parts := strings.SplitN(line, ", ", 3)
	gpu := GPUInfo{
		Available:   true,
		CUDASupport: true, // nvidia-smi succeeded → CUDA capable
	}

	if len(parts) >= 1 {
		gpu.Name = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 2 {
		vramMB, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		gpu.VRAMGB = math.Round(vramMB/1024*10) / 10
	}
	if len(parts) >= 3 {
		gpu.DriverVersion = strings.TrimSpace(parts[2])
	}

	// Get CUDA version from nvidia-smi plain output
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if smiOut, err := exec.CommandContext(ctx2, "nvidia-smi").Output(); err == nil {
		for _, line := range strings.Split(string(smiOut), "\n") {
			if idx := strings.Index(line, "CUDA Version:"); idx >= 0 {
				ver := strings.TrimSpace(line[idx+len("CUDA Version:"):])
				if sp := strings.IndexAny(ver, " |"); sp > 0 {
					ver = ver[:sp]
				}
				gpu.CUDAVersion = strings.TrimSpace(ver)
				break
			}
		}
	}

	return gpu
}

func detectMemoryLinux() MemInfo {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemInfo{}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				kb, _ := strconv.ParseUint(parts[1], 10, 64)
				return MemInfo{TotalGB: roundGB(kb * 1024)}
			}
		}
	}
	return MemInfo{}
}

func detectDiskLinux(dir string) DiskInfo {
	info := DiskInfo{ModelDir: dir}
	if dir == "" {
		return info
	}
	out, err := exec.Command("df", "-B1", "--output=avail", dir).Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			b, _ := strconv.ParseUint(strings.TrimSpace(lines[1]), 10, 64)
			info.FreeGB = roundGB(b)
		}
	}
	return info
}

func detectCPUClockLinux() float64 {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu MHz") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				mhz, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				return math.Round(mhz)
			}
		}
	}
	return 0
}

func roundGB(bytes uint64) float64 {
	return math.Round(float64(bytes)/1024/1024/1024*10) / 10
}

// ---------------------------------------------------------------------------
// Hardware spec types (for rendezvous / status reporting)
// ---------------------------------------------------------------------------

// CPUSpec holds per-socket CPU information (static + dynamic).
type CPUSpec struct {
	Name     string  `json:"name,omitempty"`
	Cores    int     `json:"cores,omitempty"`
	ClockMHz float64 `json:"clock_mhz,omitempty"`
	TempC    int     `json:"temp_c,omitempty"`
}

// GPUSpec holds per-GPU information (static + dynamic).
type GPUSpec struct {
	Name        string  `json:"name,omitempty"`
	Driver      string  `json:"driver,omitempty"`
	VramTotalGB float64 `json:"vram_total_gb,omitempty"`
	TempC       int     `json:"temp_c,omitempty"`
	UtilPct     int     `json:"util_pct,omitempty"`
	VramFreeGB  float64 `json:"vram_free_gb,omitempty"`
}

// RAMSpec holds RAM information (static + dynamic).
type RAMSpec struct {
	TotalGB float64 `json:"total_gb,omitempty"`
	FreeGB  float64 `json:"free_gb,omitempty"`
}

// HardwareSpec holds combined hardware information.
type HardwareSpec struct {
	CPUs []CPUSpec `json:"cpus,omitempty"`
	GPUs []GPUSpec `json:"gpus,omitempty"`
	RAM  *RAMSpec  `json:"ram,omitempty"`
}

// HashHardware returns a short FNV-1a fingerprint of a HardwareSpec. It is the
// single source of truth for that hash: the provider uses it for register
// delta-detection, and both isannd (when it re-signs a relayed register frame)
// and RV (when it verifies) reconstruct the RegisterDigest from it — so every
// caller MUST produce identical bytes. nil → "".
func HashHardware(hw *HardwareSpec) string {
	if hw == nil {
		return ""
	}
	data, err := json.Marshal(hw)
	if err != nil {
		return ""
	}
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for _, b := range data {
		h ^= uint64(b)
		h *= prime
	}
	return fmt.Sprintf("%016x", h)
}

// ServiceEntry is the configuration for a service the provider polls. It
// covers two flavors:
//
//   - Local (Type == "" or "local"): an engine-runner spawned by IANN's
//     installer. /health + /v1/queue/stats follow the internal contract.
//   - vLLM (Type == "vllm"): a user-managed vLLM endpoint. Health is probed
//     via /v1/models and metrics via /metrics (Prometheus). IANN does not
//     manage its lifecycle. Future engine-specific types (e.g. "ollama",
//     "tgi") would follow the same pattern with their own poll adapter.
//
// Enable is a *bool so the default (field omitted) means enabled. Set
// "enable": false to turn an entry off without deleting it — useful when
// you want to temporarily disable a backend.
type ServiceEntry struct {
	Name   string `json:"name"`             // "sd-api" | "llm-api" | "llm-xl-api" | any
	Addr   string `json:"addr"`             // "localhost:7860" or "192.168.1.50:8000"
	Type   string `json:"type,omitempty"`   // "" (local, default) | "vllm"
	Engine string `json:"engine,omitempty"` // engine package name → packages/engines/<engine>/manifest.json
	Enable *bool  `json:"enable,omitempty"` // nil = enabled (default), false = skip

	// Options carries free-form configured options for this service entry —
	// used by inspect fields with from="service" (e.g. vLLM's quantization
	// or max_num_seqs that aren't exposed by the engine's HTTP API).
	Options map[string]string `json:"options,omitempty"`

	// Queue is per-instance queue config that overrides manifest defaults.
	// Nil/zero values fall back to manifest's default_* fields. Active reader
	// is Phase 3 (resolveMaxQueue etc.); Phase 1 only defines the schema so
	// operators can place values now.
	Queue *QueueOverride `json:"queue,omitempty"`
}

// QueueOverride carries instance-level queue tunables. All fields are
// optional — leave zero/nil to inherit the manifest's default_* values.
//
// SaveToDisk uses *bool because the zero-value of bool (false) is a
// meaningful setting that needs to be distinguished from "unspecified".
type QueueOverride struct {
	MaxQueue    int   `json:"max_queue,omitempty"`     // pending+running 합산 한도, 0 = unlimited
	Concurrency int   `json:"concurrency,omitempty"`   // 동시 처리 수, 0 = manifest default
	MaxDone     int   `json:"max_done,omitempty"`      // LRU cap, 0 = manifest default
	TTLSec      int   `json:"ttl_sec,omitempty"`       // done/failed 보관 시간 (초), 0 = manifest default
	SaveToDisk  *bool `json:"save_to_disk,omitempty"`  // pointer = 명시 여부 구분 (false vs unset)
}

// IsManagedLocally reports whether IANN's installer spawned this service
// and tracks its pidfile. External engines (vllm, future ollama/tgi) return
// false — their lifecycle is driven purely by HTTP reachability.
func (s ServiceEntry) IsManagedLocally() bool {
	return s.Type == "" || s.Type == "local"
}

// IsEnabled reports whether the provider should poll / heartbeat this
// service. Default (nil) is enabled.
func (s ServiceEntry) IsEnabled() bool {
	return s.Enable == nil || *s.Enable
}

// ServiceInfo holds information about a running service.
//
// Static fields (Name, Version, Model, ServerReady, ChildPID, …) are sent
// to RV at register time and exposed via /v1/nodes. Volatile fields
// (QueueDepth, TotalJobsDone, Progress, …) flow through the 1 Hz heartbeat
// path and are exposed via /v1/metrics instead — they are stripped from the
// /v1/nodes response so the static and live views never disagree.
//
// All volatile fields use omitempty so they disappear cleanly from any
// JSON path that does not opt into them.
type ServiceInfo struct {
	Name             string `json:"name"`
	Type             string `json:"type,omitempty"` // "" = local, "vllm" = external vLLM, etc.
	// Engine carries svc.Engine (engine package name — "sd", "vllm", "llama")
	// from the operator's conf into the register frame. isannd's nlb_listener
	// uses it as the key to look up `.isann/engine-state/<engine>.json` and
	// inject Model / ModelHash / ModelOriginURL on the forward path. Provider
	// itself doesn't compute the hash — it just labels which engine this svc
	// belongs to.
	Engine           string `json:"engine,omitempty"`
	// Launcher comes straight from the engine manifest's `launcher` field
	// ("docker" | "external"). Used by broker UI to decide whether a
	// service exposes Start/Stop controls (docker = yes, external = no).
	Launcher         string `json:"launcher,omitempty"`
	Version          string `json:"version,omitempty"`
	BinHash          string `json:"bin_hash,omitempty"`
	Model            string `json:"model,omitempty"`
	ModelHash        string `json:"model_hash,omitempty"`
	// ModelOriginURL is the package.json's downloads[0].download_url —
	// where this model was fetched from. Empty for file:// imports
	// (broker UI treats empty as the "imported" placeholder) or when
	// the package can't be located on disk. Lets cards render
	// "owner/repo" / "civitai-id" prefixes that round-trip cleanly via
	// the search bar (HF text search hits owner/repo paths exactly).
	ModelOriginURL   string `json:"model_origin_url,omitempty"`
	ServerReady      bool   `json:"server_ready,omitempty"`
	ServerLoading    bool   `json:"server_loading,omitempty"`
	ChildPID         int    `json:"child_pid,omitempty"`
	ChildName        string `json:"child_name,omitempty"`
	QueueDepth       int    `json:"queue_depth,omitempty"`        // volatile — /v1/metrics 전용
	Progress         int    `json:"progress,omitempty"`           // running job progress 0-100 (0 = idle/unknown)
	EstimatedWaitSec *int   `json:"estimated_wait_sec,omitempty"` // nil이면 생략
	LastJobAt        int64  `json:"last_job_at,omitempty"`        // Unix timestamp of last submitted job
	TotalJobsDone    int    `json:"total_jobs_done,omitempty"`    // volatile — /v1/metrics 전용
	AvgJobSec        *int   `json:"avg_job_sec,omitempty"`        // nil이면 생략

	// Capacity (static): pending+running 한도 + 동시 처리 워커 수. resolveQueue
	// 결과를 register에서 한 번 보내고 RV가 /v1/nodes로 노출. broker가 이걸로
	// dispatch 사전 회피. 0 = unspecified (broker는 unlimited 처리).
	MaxQueue    int `json:"max_queue,omitempty"`
	Concurrency int `json:"concurrency,omitempty"`

	// QueueDisabled가 true면 이 서비스는 큐를 거치지 않고 직접 reverse-proxy
	// 됩니다. webdav/terminal 같은 스트리밍/long-lived 서비스를 manifest 기반
	// 으로 통합할 때 사용. 큐 통계(queue_depth, running_count 등)는 의미 없음.
	QueueDisabled bool `json:"queue_disabled,omitempty"`

	// Inspect carries the manifest-declared "configured options" for this
	// service. Built per register from manifest.Inspect.Fields, with values
	// resolved from manifest defaults / conf / engine HTTP probe / service
	// options. UI renders this as a Configured Options panel.
	Inspect       map[string]string `json:"inspect,omitempty"`        // {key: value}
	InspectLabels map[string]string `json:"inspect_labels,omitempty"` // {key: human label}
	InspectOrder  []string          `json:"inspect_order,omitempty"`  // manifest declaration order
}

// ---------------------------------------------------------------------------
// Hardware detection helpers
// ---------------------------------------------------------------------------

// detectGPUsStatic queries nvidia-smi for static GPU info (name, vram_total, driver).
func detectGPUsStatic() []GPUSpec {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []GPUSpec
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ", ", 3)
		gpu := GPUSpec{}
		if len(parts) >= 1 {
			gpu.Name = strings.TrimSpace(parts[0])
		}
		if len(parts) >= 2 {
			vramMB, _ := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
			gpu.VramTotalGB = math.Round(vramMB/1024*10) / 10
		}
		if len(parts) >= 3 {
			gpu.Driver = strings.TrimSpace(parts[2])
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

// detectGPUsDynamic queries nvidia-smi for dynamic GPU metrics (temp, util, vram_free).
func detectGPUsDynamic() []GPUSpec {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,temperature.gpu,utilization.gpu,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	var gpus []GPUSpec
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ", ", 4)
		gpu := GPUSpec{}
		if len(parts) >= 1 {
			gpu.Name = strings.TrimSpace(parts[0])
		}
		if len(parts) >= 2 {
			v, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			gpu.TempC = v
		}
		if len(parts) >= 3 {
			v, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
			gpu.UtilPct = v
		}
		if len(parts) >= 4 {
			vramMB, _ := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
			gpu.VramFreeGB = math.Round(vramMB/1024*10) / 10
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

// DetectHardwareStatic returns static hardware specs (CPU name/cores/clock, GPU name/driver/vram_total, RAM total).
// modelDir is used only for disk detection (not included in HardwareSpec).
func DetectHardwareStatic(modelDir string) HardwareSpec {
	full := DetectSystemFull(modelDir)
	spec := HardwareSpec{
		CPUs: []CPUSpec{{
			Name:     full.CPU.Name,
			Cores:    full.CPU.Cores,
			ClockMHz: full.CPU.ClockMHz,
		}},
		RAM: &RAMSpec{TotalGB: full.Mem.TotalGB},
	}
	if full.GPU.Available {
		spec.GPUs = detectGPUsStatic()
	}
	return spec
}

// DetectHardwareDynamic returns dynamic hardware metrics (CPU temp, GPU temp/util/vram_free, RAM free).
func DetectHardwareDynamic() HardwareSpec {
	return HardwareSpec{
		CPUs: detectCPUsDynamic(),
		GPUs: detectGPUsDynamic(),
		RAM:  &RAMSpec{FreeGB: detectRAMFreeGB()},
	}
}

// StableHardware returns a copy containing only stable fields
// (CPU name/cores/clock, GPU name/driver/vram_total, RAM total).
func StableHardware(hw *HardwareSpec) *HardwareSpec {
	if hw == nil {
		return nil
	}
	out := &HardwareSpec{}
	if hw.CPUs != nil {
		cpus := make([]CPUSpec, len(hw.CPUs))
		for i, c := range hw.CPUs {
			cpus[i] = CPUSpec{Name: c.Name, Cores: c.Cores, ClockMHz: c.ClockMHz}
		}
		out.CPUs = cpus
	}
	if hw.GPUs != nil {
		gpus := make([]GPUSpec, len(hw.GPUs))
		for i, g := range hw.GPUs {
			gpus[i] = GPUSpec{Name: g.Name, Driver: g.Driver, VramTotalGB: g.VramTotalGB}
		}
		out.GPUs = gpus
	}
	if hw.RAM != nil {
		out.RAM = &RAMSpec{TotalGB: hw.RAM.TotalGB}
	}
	return out
}

// VolatileHardware returns a copy containing only volatile fields
// (CPU temp, GPU temp/util/vram_free, RAM free).
func VolatileHardware(hw *HardwareSpec) *HardwareSpec {
	if hw == nil {
		return nil
	}
	out := &HardwareSpec{}
	if hw.CPUs != nil {
		cpus := make([]CPUSpec, len(hw.CPUs))
		for i, c := range hw.CPUs {
			cpus[i] = CPUSpec{TempC: c.TempC}
		}
		out.CPUs = cpus
	}
	if hw.GPUs != nil {
		gpus := make([]GPUSpec, len(hw.GPUs))
		for i, g := range hw.GPUs {
			gpus[i] = GPUSpec{TempC: g.TempC, UtilPct: g.UtilPct, VramFreeGB: g.VramFreeGB}
		}
		out.GPUs = gpus
	}
	if hw.RAM != nil {
		out.RAM = &RAMSpec{FreeGB: hw.RAM.FreeGB}
	}
	return out
}

// MergeHardware merges volatile metrics into a stable HardwareSpec in-place.
func MergeHardware(stable *HardwareSpec, vol *HardwareSpec) {
	if stable == nil || vol == nil {
		return
	}
	for i := range stable.CPUs {
		if i < len(vol.CPUs) {
			stable.CPUs[i].TempC = vol.CPUs[i].TempC
		}
	}
	for i := range stable.GPUs {
		if i < len(vol.GPUs) {
			stable.GPUs[i].TempC = vol.GPUs[i].TempC
			stable.GPUs[i].UtilPct = vol.GPUs[i].UtilPct
			stable.GPUs[i].VramFreeGB = vol.GPUs[i].VramFreeGB
		}
	}
	if stable.RAM != nil && vol.RAM != nil {
		stable.RAM.FreeGB = vol.RAM.FreeGB
	}
}

// LogSystemInfo prints detected capabilities.
func LogSystemInfo(sys SystemInfo) {
	gpu := "none"
	if sys.HasCUDA {
		gpu = "NVIDIA (CUDA)"
	}

	cpu := "noavx"
	if sys.HasAVX512 {
		cpu = "AVX512"
	} else if sys.HasAVX2 {
		cpu = "AVX2"
	} else if sys.HasAVX {
		cpu = "AVX"
	}

	log.Printf("[setup] Detected: %s %s, GPU: %s, CPU: %s", sys.OS, sys.Arch, gpu, cpu)
}

// FormatSystemSummary returns a human-readable summary.
func FormatSystemSummary(info FullSystemInfo) string {
	gpu := "none"
	if info.GPU.Available {
		gpu = fmt.Sprintf("%s (%.1f GB)", info.GPU.Name, info.GPU.VRAMGB)
	}
	return fmt.Sprintf("CPU: %s (%d cores), RAM: %.1f GB, GPU: %s, Disk free: %.1f GB",
		info.CPU.Name, info.CPU.Cores, info.Mem.TotalGB, gpu, info.Disk.FreeGB)
}
