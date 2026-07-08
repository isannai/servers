package setup

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// NewLlamaProject returns a Project for llama.cpp binaries.
func NewLlamaProject(destDir string) Project {
	required := []string{"llama-server", "llama-cli"}
	if runtime.GOOS == "windows" {
		required = []string{"llama-server.exe", "llama-cli.exe"}
	}

	return Project{
		Name:          "llama.cpp",
		Owner:         "ggerganov",
		Repo:          "llama.cpp",
		DestDir:       destDir,
		RequiredFiles: required,
		SelectAsset:   selectLlamaAsset,
	}
}

func selectLlamaAsset(assets []string, sys SystemInfo) (string, error) {
	// Allow explicit variant override via env var
	if variant := os.Getenv("LLAMA_VARIANT"); variant != "" {
		for _, a := range assets {
			if strings.Contains(a, variant) && matchesLlamaPlatform(a, sys) {
				return a, nil
			}
		}
		return "", fmt.Errorf("no asset matching variant %q for %s/%s", variant, sys.OS, sys.Arch)
	}

	// Filter by platform
	var candidates []string
	for _, a := range assets {
		if matchesLlamaPlatform(a, sys) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no asset found for %s/%s", sys.OS, sys.Arch)
	}

	// For Windows: prefer CUDA if available, else CPU
	if sys.OS == "windows" {
		if sys.HasCUDA {
			for _, a := range candidates {
				if strings.Contains(strings.ToLower(a), "cuda") {
					return a, nil
				}
			}
		}
		// Fallback to CPU
		for _, a := range candidates {
			if strings.Contains(strings.ToLower(a), "cpu") {
				return a, nil
			}
		}
	}

	// For other platforms, return first match
	return candidates[0], nil
}

func matchesLlamaPlatform(asset string, sys SystemInfo) bool {
	a := strings.ToLower(asset)

	// Skip source archives
	if strings.Contains(a, "src") || strings.Contains(a, "source") {
		return false
	}

	switch sys.OS {
	case "windows":
		if !strings.Contains(a, "win") {
			return false
		}
	case "linux":
		if !strings.Contains(a, "ubuntu") && !strings.Contains(a, "linux") {
			return false
		}
	case "darwin":
		if !strings.Contains(a, "macos") && !strings.Contains(a, "darwin") {
			return false
		}
	default:
		return false
	}

	switch sys.Arch {
	case "amd64":
		return strings.Contains(a, "x64") || strings.Contains(a, "x86_64") || !strings.Contains(a, "arm64")
	case "arm64":
		return strings.Contains(a, "arm64")
	}
	return false
}
