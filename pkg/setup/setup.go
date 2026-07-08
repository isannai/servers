package setup

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Project defines how to find and extract binaries for a specific C++ project.
type Project struct {
	Name          string   // display name, e.g. "stable-diffusion.cpp"
	Owner         string   // GitHub owner
	Repo          string   // GitHub repo
	DestDir       string   // local directory to place binaries
	RequiredFiles []string // files to check (relative to DestDir)
	SelectAsset   func(assets []string, sys SystemInfo) (string, error)
}

// Ensure checks if binaries exist; if not, downloads and extracts them.
func Ensure(p Project) error {
	// Check if all required files exist
	allExist := true
	for _, f := range p.RequiredFiles {
		path := filepath.Join(p.DestDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			allExist = false
			break
		}
	}
	if allExist {
		return nil
	}

	log.Printf("[setup] %s binaries not found in %s, downloading...", p.Name, p.DestDir)

	// Detect system
	sys := DetectSystem()
	LogSystemInfo(sys)

	// Fetch latest release
	release, err := LatestRelease(p.Owner, p.Repo)
	if err != nil {
		return fmt.Errorf("failed to fetch release info: %w\n"+
			"  Download manually from: https://github.com/%s/%s/releases", err, p.Owner, p.Repo)
	}

	// Select the right asset
	assetName, err := p.SelectAsset(release.AssetNames(), sys)
	if err != nil {
		return fmt.Errorf("%w\n  Download manually from: https://github.com/%s/%s/releases", err, p.Owner, p.Repo)
	}

	asset := release.FindAsset(assetName)
	if asset == nil {
		return fmt.Errorf("asset %q not found in release", assetName)
	}

	log.Printf("[setup] Downloading %s (%s)...", asset.Name, formatBytes(asset.Size))

	// Create dest dir
	if err := os.MkdirAll(p.DestDir, 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Download to temp file
	tmpPath := filepath.Join(p.DestDir, ".download-tmp")
	if err := download(asset.BrowserDownloadURL, asset.Size, tmpPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("download failed: %w", err)
	}

	// Extract
	log.Printf("[setup] Extracting to %s...", p.DestDir)
	if strings.HasSuffix(asset.Name, ".tar.gz") || strings.HasSuffix(asset.Name, ".tgz") {
		_, err = ExtractTarGz(tmpPath, p.DestDir)
	} else {
		_, err = ExtractZip(tmpPath, p.DestDir)
	}
	os.Remove(tmpPath)
	if err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	// Verify
	var missing []string
	for _, f := range p.RequiredFiles {
		path := filepath.Join(p.DestDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("extraction completed but files still missing: %v", missing)
	}

	log.Printf("[setup] Done! %s ready in %s", p.Name, p.DestDir)
	return nil
}

// download fetches a URL to a local file with progress display.
func download(url string, totalSize int64, destPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	pw := &progressDisplay{
		total:    totalSize,
		lastTime: time.Now(),
	}

	_, err = io.Copy(f, io.TeeReader(resp.Body, pw))
	fmt.Fprintf(os.Stderr, "\n") // newline after progress
	return err
}

// progressDisplay tracks download progress and prints to stderr.
type progressDisplay struct {
	written  int64
	total    int64
	lastTime time.Time
}

func (p *progressDisplay) Write(data []byte) (int, error) {
	n := len(data)
	p.written += int64(n)

	now := time.Now()
	if now.Sub(p.lastTime) >= 200*time.Millisecond || p.written == p.total {
		pct := 0
		if p.total > 0 {
			pct = int(p.written * 100 / p.total)
		}
		fmt.Fprintf(os.Stderr, "\r[setup] %s / %s (%d%%)",
			formatBytes(p.written), formatBytes(p.total), pct)
		p.lastTime = now
	}
	return n, nil
}

// extractZip extracts a zip archive into destDir, flattening single top-level dir.
// ExtractedFile represents a file that was extracted from an archive.
// Hash + ModifiedAt are populated post-extraction so isArchiveUpToDate can
// verify the extracted tree wasn't tampered after install (zip itself is
// deleted after extraction so it can't be re-checked). ModifiedAt enables
// a fast pre-hash check — when mtime matches the recorded value we skip
// the (relatively expensive) sha256 read.
type ExtractedFile struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	Hash       string    `json:"hash,omitempty"`
	ModifiedAt time.Time `json:"modified_at,omitempty"`
}

// SkipFunc decides whether extracting a particular zip entry should be
// skipped. relPath is the archive-relative path (slash-separated) after
// any single-top-level prefix flattening. Return true to leave whatever
// is already on disk untouched.
//
// Used by installers to preserve user-mutable files (e.g. conf/*) across
// re-install: when the verify entry is marked mutable AND the file already
// exists on disk, skip extraction.
type SkipFunc func(relPath string) bool

// StripMode controls how ExtractZip handles a single common top-level
// directory inside the archive (the "github wrapper" pattern).
type StripMode int

const (
	// StripAuto: detect single top-level wrapper, strip it UNLESS it's a
	// known IANN structural directory (`packages`, `bin`, `conf`, ...).
	// Default — works for cmd/package output and most github tarballs.
	StripAuto StripMode = iota
	// StripNone: never strip. Use when the archive's paths must reach disk
	// exactly as-is (e.g., user-controlled archives where every level is
	// significant).
	StripNone
	// StripForce: always strip the single top-level wrapper when present,
	// even if it matches a known IANN dir name. Use only via explicit URL
	// rules (e.g., for sources known to wrap content unconventionally).
	StripForce
)

// ExtractZip extracts a zip archive to destDir and returns the list of
// extracted files. Wrapper around ExtractZipFiltered with default options.
func ExtractZip(zipPath, destDir string) ([]ExtractedFile, error) {
	return ExtractZipWithOptions(zipPath, destDir, nil, StripAuto)
}

// ExtractZipFiltered keeps backward-compat — uses StripAuto and the given
// skip predicate. New callers should use ExtractZipWithOptions for
// explicit control over both behaviors.
func ExtractZipFiltered(zipPath, destDir string, skip SkipFunc) ([]ExtractedFile, error) {
	return ExtractZipWithOptions(zipPath, destDir, skip, StripAuto)
}

// ExtractZipWithOptions is the full-control variant: skip filter + strip
// mode. Skipped entries that already exist on disk are still reported in
// the returned ExtractedFile slice with their current on-disk metadata,
// so callers building verify manifests see a consistent picture regardless
// of whether content came from the zip or was preserved from a previous
// install.
func ExtractZipWithOptions(zipPath, destDir string, skip SkipFunc, strip StripMode) ([]ExtractedFile, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	// Decide whether and how to strip a top-level wrapper based on mode.
	var prefix string
	switch strip {
	case StripNone:
		prefix = ""
	case StripForce:
		prefix = detectZipPrefixRaw(r.File)
	default: // StripAuto
		prefix = detectZipPrefix(r.File)
	}
	// Normalize a backslash-separated prefix too (see the loop) so TrimPrefix
	// still matches normalized entry names.
	prefix = strings.ReplaceAll(prefix, "\\", "/")

	var extracted []ExtractedFile
	for _, f := range r.File {
		// Compress-Archive (Windows PowerShell / .NET Framework) writes ZIP
		// entry names with backslash separators, violating APPNOTE 4.4.17
		// (forward slash only). Normalize so dir-entry detection and path
		// joins are correct and portable — on Linux filepath.Join would NOT
		// split a backslash, yielding one file with literal '\' in its name.
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue
			}
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(name))

		// Trailing slash → directory entry. Test the normalized name, since
		// f.FileInfo().IsDir() keys off the raw (backslash) name and misses
		// Compress-Archive directory entries (which end in '\', not '/').
		if f.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
			os.MkdirAll(destPath, 0755)
			continue
		}

		// Mutable-file preservation: when caller flags this entry as
		// "skip if already on disk", record the existing on-disk meta
		// instead of overwriting from the zip.
		if skip != nil && skip(filepath.ToSlash(name)) {
			if info, statErr := os.Stat(destPath); statErr == nil && !info.IsDir() {
				extracted = append(extracted, ExtractedFile{
					Path:       filepath.ToSlash(name),
					Size:       info.Size(),
					Hash:       FileHash(destPath),
					ModifiedAt: FileMtime(info),
				})
				continue
			}
			// Skip target is missing → fall through and extract default.
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return nil, err
		}

		if err := extractZipFile(f, destPath); err != nil {
			return nil, err
		}

		// Record relative path from destDir + hash + mtime for tamper detection
		relPath := filepath.Join(filepath.FromSlash(name))
		var mt time.Time
		if info, statErr := os.Stat(destPath); statErr == nil {
			mt = FileMtime(info)
		}
		extracted = append(extracted, ExtractedFile{
			Path:       filepath.ToSlash(relPath),
			Size:       int64(f.UncompressedSize64),
			Hash:       FileHash(destPath),
			ModifiedAt: mt,
		})
	}
	return extracted, nil
}

func extractZipFile(f *zip.File, destPath string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode()|0755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, rc); err != nil {
		return err
	}
	// Close before chtimes — Windows refuses mtime change on open files.
	out.Close()
	// Preserve original mtime from the zip header so verify hashes /
	// inventories stay reproducible across pack / unpack cycles.
	if !f.Modified.IsZero() {
		_ = os.Chtimes(destPath, time.Now(), f.Modified)
	}
	return nil
}

// detectZipPrefixRaw finds the single common top-level directory prefix
// shared by all entries, with no exemptions. Used by StripForce mode.
func detectZipPrefixRaw(files []*zip.File) string {
	if len(files) == 0 {
		return ""
	}
	first := files[0].Name
	sep := strings.IndexByte(first, '/')
	if sep < 0 {
		return ""
	}
	prefix := first[:sep+1]
	for _, f := range files {
		if !strings.HasPrefix(f.Name, prefix) {
			return ""
		}
	}
	return prefix
}

// detectZipPrefix finds a common single top-level wrapper directory to
// strip on extract. Conservative: refuses to strip when the prefix matches
// a known structural directory name (packages/, bin/, conf/, etc.) — those
// are part of the install layout, not a github-tarball-style wrapper.
//
// Returns empty string (= no stripping) for cmd/package-built zips since
// they don't add a wrapper. The function still recognizes legitimate
// wrappers like "repo-v1.0/" from github archive downloads.
func detectZipPrefix(files []*zip.File) string {
	if len(files) == 0 {
		return ""
	}
	first := files[0].Name
	sep := strings.IndexByte(first, '/')
	if sep < 0 {
		return ""
	}
	prefix := first[:sep+1]
	for _, f := range files {
		if !strings.HasPrefix(f.Name, prefix) {
			return ""
		}
	}
	// Don't strip when the prefix is a known IANN install-layout dir.
	// These are structural — they belong in the extracted output.
	switch strings.TrimSuffix(prefix, "/") {
	case "packages", "bin", "conf", "lib", "ai", "models", "manifests",
		"keystores", "logs", "certs", "src", "docs":
		return ""
	}
	return prefix
}

// ExtractTarGz extracts a tar.gz archive to destDir and returns the list of extracted files.
func ExtractTarGz(tarPath, destDir string) ([]ExtractedFile, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	// First pass: detect prefix
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		names = append(names, hdr.Name)
	}
	prefix := detectTarPrefix(names)

	// Second pass: extract
	f.Seek(0, 0)
	gz2, _ := gzip.NewReader(f)
	defer gz2.Close()
	tr2 := tar.NewReader(gz2)

	var extracted []ExtractedFile
	for {
		hdr, err := tr2.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		name := hdr.Name
		if prefix != "" {
			name = strings.TrimPrefix(name, prefix)
			if name == "" {
				continue
			}
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(destPath, 0755)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)|0755)
			if err != nil {
				return nil, err
			}
			io.Copy(out, tr2)
			out.Close()
			// Preserve original mtime from the tar header.
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(destPath, time.Now(), hdr.ModTime)
			}

			relPath := filepath.FromSlash(name)
			var mt time.Time
			if info, statErr := os.Stat(destPath); statErr == nil {
				mt = FileMtime(info)
			}
			extracted = append(extracted, ExtractedFile{
				Path:       filepath.ToSlash(relPath),
				Size:       hdr.Size,
				Hash:       FileHash(destPath),
				ModifiedAt: mt,
			})
		}
	}
	return extracted, nil
}

func detectTarPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	first := names[0]
	sep := strings.IndexByte(first, '/')
	if sep < 0 {
		return ""
	}
	prefix := first[:sep+1]
	for _, n := range names {
		if !strings.HasPrefix(n, prefix) {
			return ""
		}
	}
	return prefix
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
