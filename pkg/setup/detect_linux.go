//go:build !windows

package setup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func detectMemory() MemInfo {
	return detectMemoryLinux()
}

func detectDisk(dir string) DiskInfo {
	return detectDiskLinux(dir)
}

// FreeDiskBytes returns the number of bytes available to the current user on
// the volume containing dir. Used by the installer for pre-download capacity
// check — detectDisk's roundGB loses precision (a few-MB shortfall would
// silently round to 0 GB difference), so this returns raw bytes.
//
// Uses syscall.Statfs which is portable to Linux/macOS/BSD. Bavail (blocks
// available to non-root user) × Bsize (block size) = bytes available.
func FreeDiskBytes(dir string) (uint64, error) {
	if dir == "" {
		return 0, fmt.Errorf("FreeDiskBytes: empty path")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

func detectCPUClock() float64 {
	return detectCPUClockLinux()
}

// hasCUDA12Runtime on non-Windows: assume CUDA runtime is available if nvidia-smi succeeded.
func hasCUDA12Runtime() bool {
	return true
}

// fetchCPUProcessorID collects CPU serial via dmidecode, sorts and joins for multi-socket.
func fetchCPUProcessorID() string {
	out, err := exec.Command("dmidecode", "-t", "processor", "-q").Output()
	if err != nil {
		return "unknown-cpu"
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID:") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "ID:"))
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

// detectRAMFreeGB returns available physical memory in GB from /proc/meminfo.
func detectRAMFreeGB() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				kb, _ := strconv.ParseUint(parts[1], 10, 64)
				return roundGB(kb * 1024)
			}
		}
	}
	return 0
}

// detectCPUsDynamic returns per-CPU dynamic metrics (temperature) via /sys/class/hwmon.
func detectCPUsDynamic() []CPUSpec {
	name := ""
	// Best-effort CPU name from /proc/cpuinfo
	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "model name") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					name = strings.TrimSpace(parts[1])
					break
				}
			}
		}
	}

	// Read CPU temperature from hwmon (look for coretemp or k10temp)
	tempC := -1
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err == nil {
		for _, e := range entries {
			nameFile := "/sys/class/hwmon/" + e.Name() + "/name"
			nameBytes, err := os.ReadFile(nameFile)
			if err != nil {
				continue
			}
			hwmonName := strings.TrimSpace(string(nameBytes))
			if hwmonName != "coretemp" && hwmonName != "k10temp" {
				continue
			}
			// Read temp1_input (millidegrees Celsius)
			tempFile := "/sys/class/hwmon/" + e.Name() + "/temp1_input"
			tempBytes, err := os.ReadFile(tempFile)
			if err != nil {
				continue
			}
			if v, err := strconv.Atoi(strings.TrimSpace(string(tempBytes))); err == nil {
				tempC = v / 1000
				break
			}
		}
	}

	return []CPUSpec{{Name: name, TempC: tempC}}
}

// fetchMainboardUUID returns the system UUID via dmidecode.
func fetchMainboardUUID() string {
	out, err := exec.Command("dmidecode", "-s", "system-uuid").Output()
	if err != nil {
		return ""
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" || uuid == "Not Settable" || strings.HasPrefix(uuid, "Not") {
		return ""
	}
	return uuid
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
