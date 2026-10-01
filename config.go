package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const defaultConfig = "/etc/cephfs-fe/config"

// applyConfig sets every flag that was not given on the command line from
// the config file at path. The file has one "key = value" per line, keys
// being the flag names; blank lines and lines starting with # are ignored.
// A missing file is an error only if required (--config was given).
func applyConfig(fl *flag.FlagSet, path string, required bool) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	onCmdLine := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { onCmdLine[f.Name] = true })
	seen := map[string]int{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return fmt.Errorf("%s:%d: want key = value", path, n)
		}
		if k == "config" || fl.Lookup(k) == nil {
			return fmt.Errorf("%s:%d: unknown key %q", path, n, k)
		}
		if prev := seen[k]; prev != 0 {
			return fmt.Errorf("%s:%d: %s already set on line %d", path, n, k, prev)
		}
		seen[k] = n
		if onCmdLine[k] {
			continue
		}
		if err := fl.Set(k, v); err != nil {
			return fmt.Errorf("%s:%d: %s: %v", path, n, k, err)
		}
	}
	return sc.Err()
}
