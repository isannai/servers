package setup

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// FileMtime returns the file's modification time truncated to one-second
// precision. Use this everywhere mtime is captured — second precision is
// the canonical resolution because zip's wire format already lops off
// sub-second info on round-trip, so anything finer is misleading.
func FileMtime(info os.FileInfo) time.Time {
	return info.ModTime().Truncate(time.Second)
}

// FileHash returns the full SHA256 hash of the entire file (64 hex chars).
func FileHash(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// FileHashShort returns a short hash for identifying a file (legacy).
// Uses SHA256(file_size_bytes + first_64KB) and returns the first 12 hex chars.
// Deprecated: Use FileHash for full SHA256 verification.
func FileHashShort(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	h := sha256.New()

	// Write file size as 8 bytes LE
	sizeBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(sizeBuf, uint64(info.Size()))
	h.Write(sizeBuf)

	// Read first 64KB
	buf := make([]byte, 64*1024)
	n, err := io.ReadFull(f, buf)
	if n > 0 {
		h.Write(buf[:n])
	}

	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// SelfHash returns the full SHA256 hash of the currently running executable.
func SelfHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return FileHash(exe)
}

// NormalizeSHA256 strips common prefixes (sha256:, 0x) and lowercases the
// input. Returns "" for invalid input (not 64 hex chars). Use this everywhere
// hashes flow between sources (HuggingFace, Civitai, isann, RV) so storage
// and comparison are always pure hex.
func NormalizeSHA256(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(h, "sha256:")
	h = strings.TrimPrefix(h, "0x")
	if len(h) != 64 {
		return ""
	}
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return h
}
