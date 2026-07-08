package setup

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// NewSDProject returns a Project for stable-diffusion.cpp binaries.
func NewSDProject(destDir string) Project {
	required := []string{"sd-server", "sd-cli"}
	if runtime.GOOS == "windows" {
		required = []string{"sd-server.exe", "sd-cli.exe", "stable-diffusion.dll"}
	}

	return Project{
		Name:          "stable-diffusion.cpp",
		Owner:         "leejet",
		Repo:          "stable-diffusion.cpp",
		DestDir:       destDir,
		RequiredFiles: required,
		SelectAsset:   selectSDAsset,
	}
}

func selectSDAsset(assets []string, sys SystemInfo) (string, error) {
	// Allow explicit variant override via env var
	if variant := os.Getenv("SD_VARIANT"); variant != "" {
		for _, a := range assets {
			if strings.Contains(a, variant) && matchesPlatform(a, sys) {
				return a, nil
			}
		}
		return "", fmt.Errorf("no asset matching variant %q for %s/%s", variant, sys.OS, sys.Arch)
	}

	// Filter by platform
	var candidates []string
	for _, a := range assets {
		if matchesPlatform(a, sys) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no asset found for %s/%s", sys.OS, sys.Arch)
	}

	// For Windows amd64: pick best variant by priority
	if sys.OS == "windows" && sys.Arch == "amd64" {
		variants := []struct {
			keyword string
			ok      bool
		}{
			{"cuda12", sys.HasCUDA},
			{"avx512", sys.HasAVX512},
			{"avx2", sys.HasAVX2},
			{"avx", sys.HasAVX},
			{"noavx", true}, // fallback
		}
		for _, v := range variants {
			if !v.ok {
				continue
			}
			for _, a := range candidates {
				// "avx" should not match "avx2" or "avx512" or "noavx"
				if matchesVariant(a, v.keyword) {
					return a, nil
				}
			}
		}
	}

	// For other platforms, return first match
	return candidates[0], nil
}

func matchesPlatform(asset string, sys SystemInfo) bool {
	a := strings.ToLower(asset)
	switch sys.OS {
	case "windows":
		if !strings.Contains(a, "win") {
			return false
		}
	case "linux":
		if !strings.Contains(a, "linux") {
			return false
		}
	case "darwin":
		if !strings.Contains(a, "darwin") {
			return false
		}
	default:
		return false
	}

	switch sys.Arch {
	case "amd64":
		return strings.Contains(a, "x64") || strings.Contains(a, "x86_64")
	case "arm64":
		return strings.Contains(a, "arm64")
	}
	return false
}

// matchesVariant checks if the asset name contains exactly the given variant keyword.
// For example, "avx" should not match "avx2", "avx512", or "noavx".
func matchesVariant(asset, variant string) bool {
	a := strings.ToLower(asset)
	idx := strings.Index(a, variant)
	if idx < 0 {
		return false
	}

	// Check character before: must be non-alphanumeric (or start of string)
	if idx > 0 {
		prev := a[idx-1]
		if prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9' {
			return false
		}
	}

	// Check character after: must be non-alphanumeric (or end of string)
	end := idx + len(variant)
	if end < len(a) {
		next := a[end]
		if next >= 'a' && next <= 'z' || next >= '0' && next <= '9' {
			return false
		}
	}

	return true
}
