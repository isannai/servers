package market

// recipe_app_validate.go — server-side re-validation of an `app`-type recipe's
// multi-OS platform declaration (app-json-platforms.md M6). The client (app
// push) already validates, but the server must not trust the client: a signed
// recipe could be replayed or hand-crafted with an inconsistent --platforms /
// {os}-{arch} token url. We re-check the `app pull` line here and 400 on any
// mismatch. The server still never RUNS the recipe — this is pure text scan.

import (
	"fmt"
	"strings"
)

// ValidateAppRecipe scans an `app` recipe body's `app pull` line(s) and enforces
// the multi-OS invariants that `AppManifest.ValidatePublish` enforces on the
// client:
//
//	url has {os}/{arch} tokens  ⇔  --platforms present
//	--platforms entries are os/arch form, no duplicates
//
// Called only for typ=="app" pushes. A recipe with neither tokens nor
// --platforms (a plain single-url tool) passes untouched.
func ValidateAppRecipe(body string) error {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";"))
		if !strings.HasPrefix(line, "app pull") {
			continue
		}
		fields := strings.Fields(line)
		url := ""
		platforms := ""
		for i := 2; i < len(fields); i++ { // skip "app" "pull"
			switch {
			case fields[i] == "--platforms" && i+1 < len(fields):
				platforms = fields[i+1]
				i++
			case fields[i] == "--name" && i+1 < len(fields):
				i++
			case fields[i] == "--sha256" && i+1 < len(fields):
				i++
			case !strings.HasPrefix(fields[i], "-") && url == "":
				url = fields[i]
			}
		}
		hasTokens := strings.Contains(url, "{os}") && strings.Contains(url, "{arch}")
		if hasTokens && platforms == "" {
			return fmt.Errorf("app pull url has {os}-{arch} tokens but no --platforms")
		}
		if platforms != "" {
			if !hasTokens {
				return fmt.Errorf("app pull --platforms set but url has no {os}-{arch} tokens")
			}
			if err := validatePlatformList(platforms); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePlatformList checks a "os/arch,os/arch,…" value: each entry is os/arch
// with both halves non-empty and no duplicate os/arch pair.
func validatePlatformList(list string) error {
	seen := map[string]bool{}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		os, arch, ok := strings.Cut(item, "/")
		if !ok || os == "" || arch == "" {
			return fmt.Errorf("app pull --platforms: %q is not os/arch", item)
		}
		if seen[item] {
			return fmt.Errorf("app pull --platforms: duplicate %s", item)
		}
		seen[item] = true
	}
	return nil
}
