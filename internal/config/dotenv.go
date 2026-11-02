package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv reads KEY=VALUE lines from a .env file into the process
// environment, so secrets like TELEGRAM_BOT_TOKEN can live in a gitignored
// file instead of being typed into every new terminal.
//
// A variable that is already set in the real environment wins over the
// file: that way a one-off `$env:X = ...` still overrides .env. A missing
// file is not an error. It returns the names it set (never the values,
// which may be secrets).
//
// Format: blank lines and lines starting with # are skipped; an optional
// "export " prefix is allowed; the value may be wrapped in "double" or
// 'single' quotes. No variable expansion or multi-line values: we don't
// need them, and a small parser is easier to trust than a dependency.
func LoadDotEnv(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var set []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 {
			// PowerShell's Out-File and Notepad can write a UTF-8 byte order
			// mark (bytes EF BB BF); without this the first key would start
			// with an invisible character and never match.
			line = strings.TrimPrefix(line, "\xef\xbb\xbf")
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return set, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		val = unquote(strings.TrimSpace(val))
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return set, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		set = append(set, key)
	}
	return set, sc.Err()
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
