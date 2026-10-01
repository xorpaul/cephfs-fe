package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xorpaul/cephfs-fe/internal/pgsearch"
)

var types = map[string]byte{"": 0, "file": pgsearch.TypeFile, "dir": pgsearch.TypeDir, "symlink": pgsearch.TypeSymlink, "hardlink": pgsearch.TypeHardlink}

// statsEntry is one volume's cached statistics.
type statsEntry struct {
	startedAt string
	vs        pgsearch.VolumeStats
	pending   bool
	errMsg    string
}

// statsCache holds the most recently computed stats for all known volumes.
// It is safe for concurrent use.
type statsCache struct {
	mu      sync.RWMutex
	entries map[string]statsEntry
}

func newStatsCache() *statsCache { return &statsCache{entries: map[string]statsEntry{}} }

func (c *statsCache) get(fs string) (statsEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[fs]
	return e, ok
}

func (c *statsCache) set(fs string, e statsEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[fs] = e
}

type server struct {
	pool          *pgxpool.Pool
	sem           chan struct{} // one slot per concurrent search
	exportSem     chan struct{} // one slot per concurrent export, so exports never block searches
	timeout       time.Duration
	limit         int
	exportTimeout time.Duration
	exportLimit   int
	dirWorkers    int
	sc            *statsCache
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		status = ae.status
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type volumeInfo struct {
	FS             string `json:"fs"`
	Prefix         string `json:"prefix,omitempty"`
	StartedAt      string `json:"started_at,omitempty"`
	Entries        string `json:"entries,omitempty"`
	Complete       bool   `json:"complete"`
	JournalFlushed bool   `json:"journal_flushed"`
	Warning        string `json:"warning,omitempty"`
	Error          string `json:"error,omitempty"`
}

// warning says what is incomplete about an index, as cephfs-search's
// describe() does, or "" if nothing is.
func warning(m map[string]string) string {
	var w []string
	if m["complete"] != "true" {
		w = append(w, "PARTIAL walk")
	}
	if m["journal_flushed"] != "true" {
		w = append(w, "journal not flushed")
	}
	for _, k := range []string{"missed_dirs", "op_errors"} {
		if n := m[k]; n != "" && n != "0" {
			w = append(w, k+"="+n)
		}
	}
	return strings.Join(w, ", ")
}

type volumesResponse struct {
	Version  string       `json:"version"`
	Building []string     `json:"building"` // volumes with an index build running
	Volumes  []volumeInfo `json:"volumes"`
}

func (s *server) volumes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	names, err := pgsearch.Schemas(ctx, s.pool)
	if err != nil {
		writeError(w, err)
		return
	}
	building, err := pgsearch.Building(ctx, s.pool)
	if err != nil {
		writeError(w, err)
		return
	}
	// Open all volumes in parallel: on partitioned tables the crash check
	// loads per-partition catalog pages, which on cold HDD cache can take
	// 16–112 s per volume. Serial execution summed over 12 volumes was ~20 s;
	// parallel execution is bounded by the slowest single volume.
	infos := make([]volumeInfo, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			v := volumeInfo{FS: name}
			if d, err := pgsearch.Open(ctx, s.pool, name); err != nil {
				v.Error = err.Error()
			} else {
				m := d.Meta
				v.Prefix, v.StartedAt, v.Entries = m["prefix"], m["started_at"], m["entries"]
				v.Complete, v.JournalFlushed = m["complete"] == "true", m["journal_flushed"] == "true"
				v.Warning = warning(m)
			}
			infos[i] = v
		}(i, name)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, volumesResponse{Version: buildversion, Building: building, Volumes: infos})
}

// Match modes; the pattern is quoted for all but regex.
var matchModes = map[string]func(string) string{
	"exact":    func(p string) string { return "^" + regexp.QuoteMeta(p) + "$" },
	"prefix":   func(p string) string { return "^" + regexp.QuoteMeta(p) },
	"contains": regexp.QuoteMeta,
	"regex":    func(p string) string { return p },
}

type searchRequest struct {
	pattern string // as typed
	match   string
	fs      []string // empty = all
	q       pgsearch.Query
}

// parseSearch validates the query string without touching the database.
func parseSearch(v map[string][]string, limit int) (*searchRequest, error) {
	get := func(k string) string {
		if len(v[k]) == 0 {
			return ""
		}
		return strings.TrimSpace(v[k][0])
	}
	sr := &searchRequest{pattern: get("pattern"), match: get("match")}
	if sr.pattern == "" {
		return nil, badRequest("pattern is required")
	}
	if sr.match == "" {
		sr.match = "exact"
		if get("regex") == "1" { // legacy parameter
			sr.match = "regex"
		}
	}
	toRE, ok := matchModes[sr.match]
	if !ok {
		return nil, badRequest("bad match %q: want exact, prefix, contains or regex", sr.match)
	}
	re := toRE(sr.pattern)
	compiled, err := regexp.Compile(re)
	if err != nil {
		return nil, badRequest("bad pattern: %v", err)
	}
	// Without a literal the query would read the whole entries table.
	if pgsearch.NamePrefix(re) == "" && len(pgsearch.RequiredLiterals(re)) == 0 {
		if sr.match == "contains" {
			return nil, badRequest("contains needs at least 2 characters")
		}
		return nil, badRequest("pattern needs a case-sensitive literal of at least 2 characters (e.g. config or ^tmp_), outside alternations")
	}
	t, ok := types[get("type")]
	if !ok {
		return nil, badRequest("bad type %q", get("type"))
	}
	sr.q = pgsearch.Query{Re: compiled, Pattern: re, Type: t, UID: -1, Limit: limit}
	if u := get("uid"); u != "" {
		n, err := strconv.ParseUint(u, 10, 32)
		if err != nil {
			return nil, badRequest("bad uid %q", u)
		}
		sr.q.UID = int64(n)
	}
	if d := get("newer"); d != "" {
		tm, err := time.ParseInLocation("2006-01-02", d, time.Local)
		if err != nil {
			return nil, badRequest("bad newer %q: want YYYY-MM-DD", d)
		}
		sr.q.Mtime = tm.Unix()
	}
	if f := get("fs"); f != "" {
		for _, name := range strings.Split(f, ",") {
			if err := pgsearch.ValidateName(name); err != nil {
				return nil, badRequest("%v", err)
			}
			sr.fs = append(sr.fs, name)
		}
	}
	return sr, nil
}

type row struct {
	FS      string  `json:"fs"`
	Path    string  `json:"path"`
	Type    string  `json:"type"`
	UID     *uint32 `json:"uid"` // null for hardlinks: the owner is on the primary dentry
	Size    int64   `json:"size"`
	Mtime   int64   `json:"mtime"`
	Lossy   bool    `json:"lossy,omitempty"`    // path is not valid UTF-8; shown with U+FFFD
	PathB64 string  `json:"path_b64,omitempty"` // exact path bytes when lossy (export only)
}

func newRow(fs string, m pgsearch.Match) row {
	rw := row{FS: fs, Path: m.Path, Type: string([]byte{m.Type}), Size: m.Size, Mtime: m.Mtime}
	if m.Type != pgsearch.TypeHardlink {
		u := m.UID
		rw.UID = &u
	}
	if !utf8.ValidString(rw.Path) {
		rw.Path, rw.Lossy = strings.ToValidUTF8(rw.Path, "\uFFFD"), true
	}
	return rw
}

type volumeResult struct {
	FS         string `json:"fs"`
	Matches    int    `json:"matches"`
	Capped     bool   `json:"capped,omitempty"` // matches is a lower bound
	Candidates int    `json:"candidates"`
	QueryMS    int64  `json:"query_ms"`
	ResolveMS  int64  `json:"resolve_ms"`
	Warning    string `json:"warning,omitempty"`
	Error      string `json:"error,omitempty"` // volume skipped
}

type searchResponse struct {
	Rows        []row          `json:"rows"`
	Total       int            `json:"total"`
	TotalCapped bool           `json:"total_capped"` // total is a lower bound: there are more
	Truncated   bool           `json:"truncated"`    // not every match is in rows
	Limit       int            `json:"limit"`
	Volumes     []volumeResult `json:"volumes"`
	Method      string         `json:"method"`
	TookMS      int64          `json:"took_ms"`
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	sr, err := parseSearch(r.URL.Query(), s.limit)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.run(r.Context(), sr)
	total, more := 0, ""
	if resp != nil {
		total = resp.Total
		if resp.TotalCapped {
			more = "+"
		}
	}
	log.Printf("search client=%q pattern=%q match=%s fs=%v type=%q uid=%d mtime>=%d: %d%s matches in %s, err=%v",
		clientID(r), sr.pattern, sr.match, sr.fs, string(sr.q.Type), sr.q.UID, sr.q.Mtime, total, more, time.Since(t0).Round(time.Millisecond), err)
	if err != nil {
		writeError(w, err)
		return
	}
	resp.TookMS = time.Since(t0).Milliseconds()
	writeJSON(w, http.StatusOK, resp)
}

// acquire waits for a slot in sem and checks the requested volumes. The
// caller must call release when done, also after an error.
func (s *server) acquire(ctx context.Context, sem chan struct{}, sr *searchRequest) (names []string, release func(), err error) {
	release = func() {}
	select {
	case sem <- struct{}{}:
		release = func() { <-sem }
	case <-ctx.Done():
		return nil, release, fmt.Errorf("waiting for a free search slot: %w", ctx.Err())
	}
	all, err := pgsearch.Schemas(ctx, s.pool)
	if err != nil {
		return nil, release, err
	}
	names = sr.fs
	if len(names) == 0 {
		names = all
	}
	for _, n := range names {
		if !slices.Contains(all, n) {
			return nil, release, badRequest("unknown volume %q", n)
		}
	}
	return names, release, nil
}

func (s *server) run(ctx context.Context, sr *searchRequest) (*searchResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	names, release, err := s.acquire(ctx, s.sem, sr)
	defer release()
	if err != nil {
		return nil, err
	}

	resp := &searchResponse{Rows: []row{}, Volumes: []volumeResult{}, Limit: s.limit, Method: method(sr.q.Pattern)}
	q := sr.q
	q.Timeout = s.timeout
	q.DirWorkers = s.dirWorkers
	for _, name := range names {
		d, err := pgsearch.Open(ctx, s.pool, name)
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			// A volume whose index is unusable (e.g. entries cleared by crash
			// recovery) must not fail a search over all volumes.
			resp.Volumes = append(resp.Volumes, volumeResult{FS: name, Error: err.Error()})
			continue
		}
		// The limit is global: each volume gets what the previous ones left.
		q.Limit = s.limit - len(resp.Rows)
		if q.Limit <= 0 {
			q.Limit = -1 // count only
		}
		st, err := d.Search(ctx, q, func(m pgsearch.Match) error {
			resp.Rows = append(resp.Rows, newRow(name, m))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		resp.Total += st.Matches
		resp.TotalCapped = resp.TotalCapped || st.Capped
		resp.Volumes = append(resp.Volumes, volumeResult{
			FS: name, Matches: st.Matches, Capped: st.Capped, Candidates: st.Candidates,
			QueryMS: st.Query.Milliseconds(), ResolveMS: st.Resolve.Milliseconds(),
			Warning: warning(d.Meta),
		})
	}
	resp.Truncated = resp.TotalCapped || resp.Total > len(resp.Rows)
	return resp, nil
}

// statsRefreshInterval is how often the stats worker checks for updated
// started_at values. The actual stat recomputation only happens when the
// index for a volume was rebuilt (started_at changed).
const statsRefreshInterval = 10 * time.Minute

// statsComputeTimeout is the per-volume timeout for the full-table scan.
// Percentile computation on a large index can take tens of minutes.
const statsComputeTimeout = 30 * time.Minute

// statsVersion is bumped whenever ComputeStats or VolumeStats changes in a
// way that would make a cached result stale. Rows with a different version
// are ignored on load so the new computation runs automatically.
const statsVersion = 1

// statsCacheTable is the fully-qualified table that must be created, with
// SELECT, INSERT, UPDATE granted to the cephfs_fe role (see the README):
//
//	CREATE TABLE IF NOT EXISTS public.cephfs_fe_stats_cache (
//	  fs          text PRIMARY KEY,
//	  started_at  text NOT NULL,
//	  stats_ver   int  NOT NULL,
//	  computed_at timestamptz NOT NULL,
//	  stats       jsonb NOT NULL
//	);
const statsCacheTable = "public.cephfs_fe_stats_cache"

// loadStatsCache reads persisted stats from the DB into s.sc so that volumes
// whose index has not changed since the last run do not need recomputation.
// Any error (missing table, missing privilege, bad data) is logged and ignored
// — the cache simply starts empty and is filled by the normal worker loop.
func (s *server) loadStatsCache(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT fs, started_at, stats FROM `+statsCacheTable+
			` WHERE stats_ver = $1`, statsVersion)
	if err != nil {
		log.Printf("stats cache: load: %v (table may not exist yet — create it, see the README)", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var fs, startedAt string
		var raw []byte
		if err := rows.Scan(&fs, &startedAt, &raw); err != nil {
			log.Printf("stats cache: scan row: %v", err)
			continue
		}
		var vs pgsearch.VolumeStats
		if err := json.Unmarshal(raw, &vs); err != nil {
			log.Printf("stats cache: unmarshal %s: %v", fs, err)
			continue
		}
		s.sc.set(fs, statsEntry{startedAt: startedAt, vs: vs})
		n++
	}
	if err := rows.Err(); err != nil {
		log.Printf("stats cache: load rows: %v", err)
		return
	}
	log.Printf("stats cache: loaded %d volume(s) from DB", n)
}

// persistStatsEntry upserts a successfully computed result for one volume.
// Errors are logged but do not affect the caller.
func (s *server) persistStatsEntry(ctx context.Context, fs, startedAt string, vs pgsearch.VolumeStats) {
	raw, err := json.Marshal(vs)
	if err != nil {
		log.Printf("stats cache: marshal %s: %v", fs, err)
		return
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO `+statsCacheTable+` (fs, started_at, stats_ver, computed_at, stats)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (fs) DO UPDATE
		   SET started_at  = EXCLUDED.started_at,
		       stats_ver   = EXCLUDED.stats_ver,
		       computed_at = EXCLUDED.computed_at,
		       stats       = EXCLUDED.stats`,
		fs, startedAt, statsVersion, raw)
	if err != nil {
		log.Printf("stats cache: persist %s: %v", fs, err)
	}
}

// RunStatsWorker computes and caches per-volume statistics in the background.
// It runs until ctx is cancelled (server shutdown).
func (s *server) RunStatsWorker(ctx context.Context) {
	s.loadStatsCache(ctx)
	s.refreshStats(ctx)
	t := time.NewTicker(statsRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.refreshStats(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// refreshStats checks each schema's started_at and recomputes stats if the
// index was rebuilt since the last run. Volumes are processed one at a time
// to avoid competing for disk I/O during a scan of another volume.
func (s *server) refreshStats(ctx context.Context) {
	names, err := pgsearch.Schemas(ctx, s.pool)
	if err != nil {
		log.Printf("stats worker: list schemas: %v", err)
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		d, err := pgsearch.Open(ctx, s.pool, name)
		if err != nil {
			continue
		}
		startedAt := d.Meta["started_at"]
		if e, ok := s.sc.get(name); ok && !e.pending && e.errMsg == "" && e.startedAt == startedAt {
			continue // cache is current; error entries are retried on next tick
		}
		s.sc.set(name, statsEntry{startedAt: startedAt, pending: true})
		log.Printf("stats worker: computing %s (started_at %s)", name, startedAt)
		cctx, cancel := context.WithTimeout(ctx, statsComputeTimeout)
		vs, err := pgsearch.ComputeStats(cctx, s.pool, name)
		cancel()
		e := statsEntry{startedAt: startedAt}
		if err != nil {
			e.errMsg = err.Error()
			log.Printf("stats worker: %s: %v", name, err)
		} else {
			e.vs = vs
			log.Printf("stats worker: %s done: %d files, %d dirs, table %d bytes", name, vs.Files, vs.Dirs, vs.TableBytes)
			s.persistStatsEntry(ctx, name, startedAt, vs)
		}
		s.sc.set(name, e)
	}
}

type statsVolumeInfo struct {
	FS             string   `json:"fs"`
	StartedAt      string   `json:"started_at,omitempty"`
	Pending        bool     `json:"pending,omitempty"`
	Error          string   `json:"error,omitempty"`
	TableBytes     int64    `json:"table_bytes,omitempty"`
	Files          int64    `json:"files,omitempty"`
	Dirs           int64    `json:"dirs,omitempty"`
	Symlinks       int64    `json:"symlinks,omitempty"`
	Hardlinks      int64    `json:"hardlinks,omitempty"`
	Special        int64    `json:"special,omitempty"`
	CountsExact    bool     `json:"counts_exact,omitempty"`
	AvgFileSize    *float64 `json:"avg_file_size"`
	MedianFileSize *int64   `json:"median_file_size"`
	P90FileSize    *int64   `json:"p90_file_size"`
	P99FileSize    *int64   `json:"p99_file_size"`
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	names, err := pgsearch.Schemas(ctx, s.pool)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]statsVolumeInfo, 0, len(names))
	for _, name := range names {
		vi := statsVolumeInfo{FS: name}
		if e, ok := s.sc.get(name); ok {
			vi.StartedAt = e.startedAt
			vi.Pending = e.pending
			vi.Error = e.errMsg
			if !e.pending && e.errMsg == "" {
				vi.TableBytes = e.vs.TableBytes
				vi.Files = e.vs.Files
				vi.Dirs = e.vs.Dirs
				vi.Symlinks = e.vs.Symlinks
				vi.Hardlinks = e.vs.Hardlinks
				vi.Special = e.vs.Special
				vi.CountsExact = e.vs.CountsExact
				vi.AvgFileSize = e.vs.AvgFileSize
				vi.MedianFileSize = e.vs.MedianFileSize
				vi.P90FileSize = e.vs.P90FileSize
				vi.P99FileSize = e.vs.P99FileSize
			}
		} else {
			vi.Pending = true
		}
		out = append(out, vi)
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": out})
}

// method describes how the name query will run.
func method(pattern string) string {
	kind, lit := pgsearch.PureLiteral(pattern)
	switch {
	case kind == pgsearch.Exact:
		return fmt.Sprintf("name index, exact name %q", lit)
	case pgsearch.NamePrefix(pattern) != "":
		return fmt.Sprintf("name index, prefix %q", pgsearch.NamePrefix(pattern))
	}
	return fmt.Sprintf("full name scan, substring filter %q", pgsearch.RequiredLiterals(pattern))
}
