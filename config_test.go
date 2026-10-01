package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFlags() (*flag.FlagSet, *string, *string, *time.Duration) {
	fl := flag.NewFlagSet("t", flag.ContinueOnError)
	fl.String("config", "", "")
	listen := fl.String("listen", ":7788", "")
	dsn := fl.String("pg-dsn", "from-env", "")
	timeout := fl.Duration("timeout", time.Minute, "")
	return fl, listen, dsn, timeout
}

func TestApplyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte("# cephfs-fe\n\nlisten = 10.0.0.1:7788\n  pg-dsn=host=db dbname=x user=y  \ntimeout = 90s\n"), 0o644)

	fl, listen, dsn, timeout := testFlags()
	fl.Parse([]string{"--listen", ":9999"})
	if err := applyConfig(fl, path, true); err != nil {
		t.Fatal(err)
	}
	// The command line wins; values may contain '='.
	if *listen != ":9999" || *dsn != "host=db dbname=x user=y" || *timeout != 90*time.Second {
		t.Errorf("listen=%q dsn=%q timeout=%v", *listen, *dsn, *timeout)
	}

	// Missing file: fine by default, an error when --config was given.
	fl, _, dsn, _ = testFlags()
	missing := filepath.Join(t.TempDir(), "none")
	if err := applyConfig(fl, missing, false); err != nil || *dsn != "from-env" {
		t.Errorf("missing default config: %v, dsn=%q", err, *dsn)
	}
	if err := applyConfig(fl, missing, true); err == nil {
		t.Error("missing explicit config accepted")
	}

	for body, want := range map[string]string{
		"lsten = :1\n":               `unknown key "lsten"`,
		"config = /etc/other\n":      `unknown key "config"`,
		"listen\n":                   "want key = value",
		"timeout = soon\n":           "timeout",
		"listen = :1\nlisten = :2\n": "already set on line 1",
	} {
		os.WriteFile(path, []byte(body), 0o644)
		fl, _, _, _ := testFlags()
		if err := applyConfig(fl, path, true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}
