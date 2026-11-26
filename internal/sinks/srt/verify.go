package srt

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// The CtrlIRL-Coder links the CERALIVE SRT fork in place of the stock Haivision
// libsrt. Both share the SONAME libsrt.so.1.5, so a mis-built binary can silently
// fall back to the system library. To make that a hard failure, the dist build
// (Makefile / Dockerfile / CI) embeds the exact SHA-256 and source commit of the
// bundled fork via ldflags (-X) and sets a $ORIGIN rpath so the process loads the
// bundled copy. verifyLoadedSRT re-reads which libsrt the running process actually
// mapped and rejects it if it is missing, unembedded, or a different build.
//
// These variables are set at build time; empty means "not built with the CERALIVE
// bundle", which is a guard failure (see verifyLoadedSRTCached).
var (
	// ceraliveLibSHA256 is the SHA-256 (hex) of the bundled libsrt shared object.
	ceraliveLibSHA256 string
	// ceraliveSourceSHA is the CERALIVE/srt git commit the library was built from.
	ceraliveSourceSHA string
)

// ErrWrongSRTBackend is returned when the loaded SRT library is not the expected
// CERALIVE fork (or the expected identity was never embedded). It is a permanent
// configuration/build error, not a transient link condition, so callers fail fast
// instead of reconnecting.
var ErrWrongSRTBackend = errors.New("srt: unexpected SRT backend")

var (
	verifyOnce sync.Once
	verifyErr  error
)

// srtBuildIdentity reports the embedded CERALIVE build metadata.
func srtBuildIdentity() (libSHA256, sourceSHA string) {
	return ceraliveLibSHA256, ceraliveSourceSHA
}

// verifyLoadedSRT confirms the mapped libsrt is the embedded CERALIVE fork.
// It runs at most once per process; the result is cached.
func verifyLoadedSRT() error {
	verifyOnce.Do(func() { verifyErr = verifyLoadedSRTCached() })
	return verifyErr
}

// verifyLoadedSRTCached is the guard body: require an embedded identity, locate
// the mapped libsrt, and verify it hashes to the expected build.
func verifyLoadedSRTCached() error {
	if ceraliveLibSHA256 == "" {
		return fmt.Errorf("%w: no CERALIVE build identity embedded (build via `make dist` / the Dockerfile with the bundled fork, not a bare `go build`)", ErrWrongSRTBackend)
	}
	mappedPath, err := mappedSRTLib()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWrongSRTBackend, err)
	}
	return verifyMappedSRT(mappedPath, ceraliveLibSHA256)
}

// verifyMappedSRT hashes the lib at libPath and compares it against wantSHA.
func verifyMappedSRT(libPath, wantSHA string) error {
	got, err := sha256File(libPath)
	if err != nil {
		return fmt.Errorf("%w: hash %s: %v", ErrWrongSRTBackend, libPath, err)
	}
	if got != wantSHA {
		return fmt.Errorf(
			"%w: loaded %s (sha256 %s) is not the bundled CERALIVE fork (expected sha256 %s, source %s); a stock/different libsrt is being used",
			ErrWrongSRTBackend, libPath, got, wantSHA, ceraliveSourceSHA)
	}
	return nil
}

// mappedSRTLib returns the path of the libsrt shared object this process mapped.
func mappedSRTLib() (string, error) { return readMappedSRTLib("/proc/self/maps") }

// readMappedSRTLib scans a /proc/self/maps-format file for the libsrt.so.1.5
// mapping and returns its path.
func readMappedSRTLib(mapsPath string) (string, error) {
	f, err := os.Open(mapsPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// A mapping line is: <addr> <perms> <offset> <dev> <inode> <path?>.
		// The path (if any) is everything after the inode field.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue // anonymous mapping, no file path
		}
		path := fields[5]
		if path == "" || strings.HasPrefix(path, "[") {
			continue
		}
		if base := path[strings.LastIndex(path, "/")+1:]; strings.Contains(base, "libsrt.so.1.5") {
			return path, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan %s: %w", mapsPath, err)
	}
	return "", errors.New("no libsrt.so.1.5 mapped in the running process")
}

// sha256File hex-encodes the SHA-256 of a file.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
