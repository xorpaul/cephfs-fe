// cephfs-fe is a web frontend for the PostgreSQL indexes written by
// "cephfs-indexd build --pg-dsn". It serves a search page and a JSON API,
// and only answers clients with an allow-listed TLS client certificate.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xorpaul/cephfs-fe/internal/pgsearch"
)

//go:embed static
var staticDir embed.FS

// Set by go-build-release: -X main.buildversion=… -X main.buildtime=….
var (
	buildversion = "dev"
	buildtime    = ""
)

func main() { os.Exit(run()) }

func run() int {
	fl := flag.NewFlagSet("cephfs-fe", flag.ExitOnError)
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), `usage: cephfs-fe [flags]

Settings are read from --config (default %s, skipped if missing):
one "key = value" per line, the keys being the flag names below, # comments.
Flags given on the command line override the file.

flags:
`, defaultConfig)
		fl.PrintDefaults()
	}
	config := fl.String("config", defaultConfig, "config file")
	listen := fl.String("listen", ":7788", "address to listen on")
	pgDSN := fl.String("pg-dsn", os.Getenv("CEPHFS_INDEX_PG_DSN"), "PostgreSQL DSN of the cephfs-index database; password must not be included — use ~/.pgpass or $PGPASSFILE (also via $CEPHFS_INDEX_PG_DSN)")
	certFile := fl.String("tls-cert", "", "server certificate (PEM)")
	keyFile := fl.String("tls-key", "", "server private key (PEM)")
	auth := fl.String("auth", "mtls", "client authentication: 'mtls' (client certificate on an allow list) or 'basic' (user and password from htpasswd-file)")
	caFile := fl.String("tls-ca", "", "CA bundle that client certificates must chain to (PEM; auth = mtls)")
	htpasswdFile := fl.String("htpasswd-file", "", "htpasswd file with bcrypt entries, created with htpasswd -B (auth = basic)")
	subjectsFile := fl.String("allowed-subjects-file", "", "file with one allowed client certificate subject DN per line, e.g. UID=1000,CN=alice,O=Example Org (# comment lines; auth = mtls)")
	certHelpURL := fl.String("cert-help-url", "", "optional link on the 401 page: where users get a client certificate")
	accessHelpURL := fl.String("access-help-url", "", "optional link on the 403 page: where users request access with their subject DN")
	maxConc := fl.Int("max-concurrent", 2, "searches run at the same time; more wait for a slot")
	timeout := fl.Duration("timeout", 5*time.Minute, "per-search timeout, including the wait for a slot")
	limit := fl.Int("limit", 500, "rows shown per search; use the export for all matches")
	exportLimit := fl.Int("export-limit", 1_000_000, "rows written per export at most")
	exportTimeout := fl.Duration("export-timeout", 30*time.Minute, "per-export timeout, including the wait for a slot")
	dirWorkers := fl.Int("dir-workers", 4, "connections per search that look up parent dirs in parallel")
	maxExports := fl.Int("max-exports", 1, "exports running at the same time, in addition to max-concurrent searches; more wait")
	version := fl.Bool("version", false, "print the version and exit")
	reloadEvery := fl.Duration("reload-interval", time.Minute, "how often to check the TLS and allowed-subjects files for changes (SIGHUP reloads at once)")
	_ = fl.Parse(os.Args[1:])
	if fl.NArg() > 0 {
		fl.Usage()
		return 2
	}
	if *version {
		fmt.Printf("cephfs-fe %s (built %s)\n", buildversion, buildtime)
		return 0
	}
	configGiven := false
	fl.Visit(func(f *flag.Flag) { configGiven = configGiven || f.Name == "config" })
	if err := applyConfig(fl, *config, configGiven); err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 2
	}

	required := map[string]string{"pg-dsn": *pgDSN, "tls-cert": *certFile, "tls-key": *keyFile}
	unused := map[string]string{}
	switch *auth {
	case "mtls":
		required["tls-ca"], required["allowed-subjects-file"] = *caFile, *subjectsFile
		unused["htpasswd-file"] = *htpasswdFile
	case "basic":
		required["htpasswd-file"] = *htpasswdFile
		unused["tls-ca"], unused["allowed-subjects-file"] = *caFile, *subjectsFile
		unused["cert-help-url"], unused["access-help-url"] = *certHelpURL, *accessHelpURL
	default:
		fmt.Fprintf(os.Stderr, "auth must be 'mtls' or 'basic', not %q\n", *auth)
		return 2
	}
	// A setting the chosen auth mode ignores is most likely a mistake, e.g.
	// an allow list someone expects to still apply with basic auth.
	for name, v := range unused {
		if v != "" {
			hint := ""
			if name == "htpasswd-file" {
				hint = " (auth defaults to mtls; set auth = basic)"
			}
			fmt.Fprintf(os.Stderr, "%s is not used with auth = %s%s\n", name, *auth, hint)
			return 2
		}
	}
	for name, v := range required {
		if v == "" {
			fmt.Fprintf(os.Stderr, "%s is required with auth = %s (config file or --%s)\n", name, *auth, name)
			return 2
		}
	}
	if *maxConc < 1 || *maxExports < 1 || *dirWorkers < 1 || *limit < 1 || *exportLimit < 1 {
		fmt.Fprintln(os.Stderr, "max-concurrent, max-exports, dir-workers, limit and export-limit must be at least 1")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	certs, err := newCertManager(*certFile, *keyFile, *caFile, *subjectsFile, *htpasswdFile)
	if err != nil {
		log.Print(err)
		return 1
	}
	certs.certHelpURL, certs.accessHelpURL = *certHelpURL, *accessHelpURL
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go certs.watch(ctx, *reloadEvery, hup)

	// Each search or export holds a transaction for the name query and
	// resolves paths on up to dir-workers more; leave room for /api/volumes and the stats worker.
	pool, err := pgsearch.NewPool(ctx, *pgDSN, int32((*maxConc+*maxExports)*(1+*dirWorkers)+3))
	if err != nil {
		log.Print("pg pool: ", err)
		return 1
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Print("pg: ", err)
		return 1
	}

	s := &server{pool: pool, sem: make(chan struct{}, *maxConc), exportSem: make(chan struct{}, *maxExports), timeout: *timeout, limit: *limit,
		exportTimeout: *exportTimeout, exportLimit: *exportLimit, dirWorkers: *dirWorkers, sc: newStatsCache()}
	go s.RunStatsWorker(ctx)
	static, _ := fs.Sub(staticDir, "static")
	handler := certs.requireClientCert(s.routes(static))
	if *auth == "basic" {
		handler = certs.requireBasicAuth(s.routes(static))
	}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		TLSConfig:         certs.tlsConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      *timeout + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServeTLS("", "") }()
	log.Printf("cephfs-fe %s (built %s) listening on https://%s", buildversion, buildtime, *listen)

	select {
	case err := <-errc:
		log.Print(err)
		return 1
	case <-ctx.Done():
	}
	log.Print("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
	}
	return 0
}

func (s *server) routes(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/volumes", s.volumes)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/export", s.export)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}
