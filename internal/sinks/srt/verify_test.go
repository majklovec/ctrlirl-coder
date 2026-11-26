package srt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLib creates a temp file named libsrt.so.1.5 with a known payload. Callers
// compute its real sha256 with shaOf and use that as the embedded identity.
func writeLib(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "libsrt.so.1.5")
	if err := os.WriteFile(p, []byte("fake ceralive libsrt payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func shaOf(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestVerifyLoadedSRTMatch(t *testing.T) {
	lib := writeLib(t)
	want := shaOf(t, lib)

	savedSHA, savedSrc := ceraliveLibSHA256, ceraliveSourceSHA
	ceraliveLibSHA256, ceraliveSourceSHA = want, "deadbeef"
	defer func() { ceraliveLibSHA256, ceraliveSourceSHA = savedSHA, savedSrc }()

	// Given a lib whose hash equals the embedded sha, verification passes.
	if err := verifyMappedSRT(lib, ceraliveLibSHA256); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
}

func TestVerifyLoadedSRTMismatch(t *testing.T) {
	lib := writeLib(t)
	got := shaOf(t, lib)
	want := "0000000000000000000000000000000000000000000000000000000000000000"

	err := verifyMappedSRT(lib, want)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if !errors.Is(err, ErrWrongSRTBackend) {
		t.Fatalf("expected ErrWrongSRTBackend, got %v", err)
	}
	if !strings.Contains(err.Error(), got) {
		t.Fatalf("error should name the loaded sha %s, got %v", got, err)
	}
}

func TestVerifyLoadedSRTNoIdentity(t *testing.T) {
	savedSHA, savedSrc := ceraliveLibSHA256, ceraliveSourceSHA
	ceraliveLibSHA256, ceraliveSourceSHA = "", ""
	defer func() { ceraliveLibSHA256, ceraliveSourceSHA = savedSHA, savedSrc }()

	err := verifyLoadedSRTCached()
	if !errors.Is(err, ErrWrongSRTBackend) {
		t.Fatalf("expected ErrWrongSRTBackend for unembedded identity, got %v", err)
	}
	if !strings.Contains(err.Error(), "no CERALIVE build identity") {
		t.Fatalf("expected the unembedded-identity message, got %v", err)
	}
}

func TestMappedSRTLibParsesProcMaps(t *testing.T) {
	dir := t.TempDir()
	// A real (any) file named libsrt.so.1.5 in one dir, plus a decoy elsewhere.
	real := filepath.Join(dir, "libsrt.so.1.5")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	maps := filepath.Join(dir, "maps")
	content := `
7f0000000000-7f0000021000 r-xp 00000000 00:00 0
7f0000020000-7f0000041000 rw-p 00000000 00:00 0
55d000000000-55d000020000 r-xp 00000000 fd:01 1234  ` + real + `
55d000020000-55d000040000 rw-p 00000000 fd:01 1234  ` + real + `
`
	if err := os.WriteFile(maps, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readMappedSRTLib(maps)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if got != real {
		t.Fatalf("expected %s, got %s", real, got)
	}
}

func TestMappedSRTLibNotFound(t *testing.T) {
	dir := t.TempDir()
	maps := filepath.Join(dir, "maps")
	content := `
7f0000000000-7f0000021000 r-xp 00000000 00:00 0
7f0000020000-7f0000041000 rw-p 00000000 00:00 0
`
	if err := os.WriteFile(maps, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMappedSRTLib(maps); err == nil {
		t.Fatal("expected not-found error")
	}
}
