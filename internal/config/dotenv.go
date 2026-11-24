// Package config provides externalized YAML configuration loading, environment overrides,
// and strict fail-fast validation for the agy-gateway service.
package config

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
)

// LoadDotEnv loads key-value pairs from .env files without external dependencies.
// Existing environment variables are never overwritten.
//
// @purpose Ingests local configuration files into the runtime process environment without external dependencies.
// @rationale Provides zero-dependency 12-factor configuration ingestion while preserving existing process environment values set by container runtimes or CI/CD pipelines.
// @preconditions Target .env files must be formatted as valid UTF-8 text with KEY=VAL lines.
// - @param: paths Optional sequence of file paths to probe and load in order.
// @returns The total number of newly injected environment variables, and any fatal I/O error encountered.
// @throws Returns non-nil error if an explicit file cannot be opened (unless non-existent) or scanned.
// @invariants Idempotent; existing environment keys are strictly non-overwritable; thread-safe process environment modification via standard os.Setenv.
func LoadDotEnv(paths ...string) (int, error) {
	if len(paths) == 0 {
		targetPaths := []string{}
		if custom := os.Getenv("AGY_GW_ENV_FILE"); custom != "" {
			targetPaths = append(targetPaths, custom)
		}
		targetPaths = append(targetPaths, ".env", ".env.local")
		paths = targetPaths
	}

	loadedCount := 0
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return loadedCount, err
		}

		count, err := parseDotEnv(f)
		_ = f.Close()
		if err != nil {
			return loadedCount, err
		}
		loadedCount += count
	}

	return loadedCount, nil
}

// parseDotEnv parses KEY=VAL lines from an io.Reader.
//
// @purpose Parses key-value pairs from stream input and registers unestablished variables in the process environment.
// @rationale Zero-dependency stream scanning avoiding memory overhead of loading entire files into contiguous slices.
// @preconditions r must be a valid, readable non-nil io.Reader.
// - @param: r Source input reader containing dotenv formatted lines.
// @returns The count of newly registered environment variables and scanner error if any.
// @throws Returns scanner I/O error if line buffer exceeds maximum tokens or read fails.
// @invariants Linear time complexity O(N) relative to stream size; memory usage O(M) where M is max line length.
func parseDotEnv(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	count := 0

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		// Handle export prefix if present
		if bytes.HasPrefix(line, []byte("export ")) {
			line = bytes.TrimSpace(line[7:])
		}

		idx := bytes.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}

		key := string(bytes.TrimSpace(line[:idx]))
		val := string(bytes.TrimSpace(line[idx+1:]))

		// Strip matching wrapping quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		// Clean inline comments if not inside quotes
		if commentIdx := strings.Index(val, " #"); commentIdx != -1 {
			val = strings.TrimSpace(val[:commentIdx])
		}

		// Set only if not already established in process environment
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err == nil {
				count++
			}
		}
	}

	return count, scanner.Err()
}
