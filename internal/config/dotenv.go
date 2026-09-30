package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// LoadDotEnv copies KEY=VALUE lines from path into the process environment.
// Variables that are already set win, so a real environment (systemd
// EnvironmentFile, CI) is never overridden by a stray local file. A missing
// file is not an error.
func LoadDotEnv(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return true, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch {
		case len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0]:
			val = val[1 : len(val)-1]
		default:
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, val); err != nil {
				return true, err
			}
		}
	}
	return true, sc.Err()
}
