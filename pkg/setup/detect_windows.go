//go:build windows

package setup

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func detectMemory() MemInfo {
	return detectMemoryWindows()
}

func detectMemoryWindows() MemInfo {
	type memoryStatusEx struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	var mem memoryStatusEx
	mem.Length = uint32(unsafe.Sizeof(mem))
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")
	proc.Call(uintptr(unsafe.Pointer(&mem)))
	return MemInfo{
		TotalGB: roundGB(mem.TotalPhys),
	}
}

func detectDisk(dir string) DiskInfo {
	info := DiskInfo{ModelDir: dir}
	if dir == "" {
		return info
	}
	p, _ := windows.UTF16PtrFromString(dir)
	var free, total, totalFree uint64
	windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree)
	info.FreeGB = roundGB(free)
	return info
}

// FreeDiskBytes returns the number of bytes available to the current user on
// the volume containing dir. Used by the installer for pre-download capacity
// check — detectDisk's roundGB loses precision (a few-MB shortfall would
// silently round to 0 GB difference), so this returns raw bytes.
//
// Returns an error when dir is empty, malformed, or unreachable; callers
// typically log + skip the check rather than aborting (rare to fail).
func FreeDiskBytes(dir string) (uint64, error) {
	if dir == "" {
		return 0, fmt.Errorf("FreeDiskBytes: empty path")
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("UTF16: %w", err)
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return free, nil
}

// hasCUDA12Runtime checks if cudart64_12.dll is available on the system.
func hasCUDA12Runtime() bool {
	dll := windows.NewLazySystemDLL("cudart64_12.dll")
	return dll.Load() == nil
}

// fetchCPUProcessorID collects all CPU ProcessorIds, sorts and joins them.
func fetchCPUProcessorID() string {
	out, err := exec.Command("wmic", "cpu", "get", "ProcessorId", "/value").Output()
	if err != nil {
		return "unknown-cpu"
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ProcessorId=") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "ProcessorId="))
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return "unknown-cpu"
	}
	sort.Strings(ids)
	return strings.Join(ids, ":")
}

// fetchGPUUUID collects all GPU UUIDs via nvidia-smi, sorts and joins them.
func fetchGPUUUID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=gpu_uuid", "--format=csv,noheader").Output()
	if err != nil {
		return "no-gpu"
	}
	var uuids []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			uuids = append(uuids, line)
		}
	}
	if len(uuids) == 0 {
		return "no-gpu"
	}
	sort.Strings(uuids)
	return strings.Join(uuids, ":")
}

// fetchMainboardUUID returns the system UUID via wmic csproduct.
func fetchMainboardUUID() string {
	out, err := exec.Command("wmic", "csproduct", "get", "UUID", "/value").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UUID=") {
			uuid := strings.TrimSpace(strings.TrimPrefix(line, "UUID="))
			if uuid != "" && uuid != "FFFFFFFF-FFFF-FFFF-FFFF-FFFFFFFFFFFF" {
				return uuid
			}
		}
	}
	return ""
}

// detectRAMFreeGB returns available physical memory in GB using GlobalMemoryStatusEx.
func detectRAMFreeGB() float64 {
	type memoryStatusEx struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys            uint64
		AvailPhys            uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	var mem memoryStatusEx
	mem.Length = uint32(unsafe.Sizeof(mem))
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")
	proc.Call(uintptr(unsafe.Pointer(&mem)))
	return roundGB(mem.AvailPhys)
}

// detectCPUsDynamic returns per-CPU dynamic metrics (temperature) via WMI thermal zones.
// Returns CPU name with temp; returns temp=-1 on failure.
func detectCPUsDynamic() []CPUSpec {
	ctx2s, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	name := ""
	out, err := exec.CommandContext(ctx2s, "wmic", "cpu", "get", "Name", "/value").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Name=") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "Name="))
				break
			}
		}
	}

	// Try to get CPU temperature via WMI MSAcpi_ThermalZoneTemperature
	// Temperature is in tenths of Kelvin: (value - 2731) / 10 = Celsius
	tempC := -1
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	out2, err2 := exec.CommandContext(ctx2, "wmic", "/namespace:\\\\root\\wmi",
		"PATH", "MSAcpi_ThermalZoneTemperature", "get", "CurrentTemperature", "/value").Output()
	if err2 == nil {
		for _, line := range strings.Split(string(out2), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "CurrentTemperature=") {
				val := strings.TrimPrefix(line, "CurrentTemperature=")
				if v, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && v > 0 {
					tempC = (v - 2731) / 10
					break
				}
			}
		}
	}

	return []CPUSpec{{Name: name, TempC: tempC}}
}

func detectCPUClock() float64 {
	out, err := exec.Command("wmic", "cpu", "get", "MaxClockSpeed", "/value").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MaxClockSpeed=") {
			val := strings.TrimPrefix(line, "MaxClockSpeed=")
			mhz, _ := strconv.ParseFloat(strings.TrimSpace(val), 64)
			return mhz
		}
	}
	return 0
}
