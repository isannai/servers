package setup

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractZip_BackslashEntries guards the Compress-Archive (Windows
// PowerShell / .NET Framework) regression: it writes ZIP entry names with
// backslash separators, violating APPNOTE 4.4.17 (forward slash only). A
// backslash directory entry ("web\broker\") isn't recognized by archive/zip's
// IsDir() (which checks for a trailing '/'), so without normalization it gets
// written as a FILE named "broker" and the nested file under it then fails to
// extract ("cannot find the path specified"). ExtractZip must normalize
// separators so the nested tree lands correctly — this is exactly the shape
// of the suite zip once web/broker/build/ (deeply nested) was added.
func TestExtractZip_BackslashEntries(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "bs.zip")

	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	// Directory entries with backslash separators, as Compress-Archive emits
	// them — note these end in '\' (not '/'), so zip.Writer stores them as
	// plain entries and archive/zip's FileInfo().IsDir() returns false.
	for _, d := range []string{`web\`, `web\broker\`, `web\broker\build\`} {
		if _, err := zw.Create(d); err != nil {
			t.Fatal(err)
		}
	}
	w, err := zw.Create(`web\broker\build\index.html`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("<html></html>")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zf.Close()

	dest := filepath.Join(dir, "out")
	if _, err := ExtractZip(zipPath, dest); err != nil {
		t.Fatalf("ExtractZip: %v", err)
	}

	// The nested file must exist at the normalized path.
	idx := filepath.Join(dest, "web", "broker", "build", "index.html")
	b, err := os.ReadFile(idx)
	if err != nil {
		t.Fatalf("expected extracted file %s: %v", idx, err)
	}
	if string(b) != "<html></html>" {
		t.Fatalf("content = %q, want <html></html>", string(b))
	}

	// "web/broker" must be a directory, not a stray file from a
	// misinterpreted directory entry.
	brokerPath := filepath.Join(dest, "web", "broker")
	fi, err := os.Stat(brokerPath)
	if err != nil {
		t.Fatalf("stat %s: %v", brokerPath, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s should be a directory (dir entry was misinterpreted as a file)", brokerPath)
	}
}
