// Package pgsearch is the read path of cephfs-index's PostgreSQL backend.
//
// Copied from cephfs-index internal/pgindex/search.go (Open, Schemas, Search,
// resolveDirs → resolver, buildPaths) and internal/pgindex/writer.go (NewPool, qi,
// validateName), with the constants from internal/scan/scan.go. Differences:
// Query has Mtime and Limit, Search returns its Stats instead of storing them
// on DB, and a statement timeout can be set. The schema is owned by
// cephfs-indexd; keep this file in sync when it changes.
package pgsearch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry types as stored in entries.type (cephfs-index internal/scan).
const (
	TypeFile     = 'f'
	TypeDir      = 'd'
	TypeSymlink  = 'l'
	TypeHardlink = 'h' // remote dentry: metadata lives with the primary dentry
)

// RootIno is the inode of the filesystem root.
const RootIno = 1

type Query struct {
	Re      *regexp.Regexp
	Pattern string        // source of Re, used to derive a literal prefix
	Type    byte          // Type*, 0 = any
	UID     int64         // -1 = any
	Mtime   int64         // only entries with mtime >= Mtime (Unix seconds), 0 = any
	Limit   int           // emit at most this many matches, 0 = all, <0 = none; the rest are only counted
	Timeout time.Duration // statement_timeout for the name query, 0 = server default
	// DirWorkers is how many connections resolve one tree level of parent
	// dirs in parallel (<=1: one). The RAID can serve several random reads
	// at once; one session issues them one after another.
	DirWorkers int
}

type Match struct {
	Type  byte
	UID   uint32
	Size  int64
	Mtime int64 // Unix seconds
	Ctime int64 // Unix seconds
	Path  string
}

// DB is an open connection to a PostgreSQL index schema. It is safe for
// concurrent use.
type DB struct {
	pool   *pgxpool.Pool
	fsName string
	Meta   map[string]string
	prefix string // filesystem root path, without trailing slash
	layout int    // meta.layout: 2 = COPY FREEZE leaves; 1 = covering indexes, no layout key (the cephfs-index version just before layout 2); 0 = older
}

// Stats splits a search's time between the name query and path
// reconstruction.
type Stats struct {
	Query      time.Duration // name query incl. reading all candidate rows
	Candidates int           // rows returned by the name query
	Matches    int           // candidates that match the regex, including those over Limit (unless Capped)
	Capped     bool          // the query stopped at LIMIT: Matches counts the returned rows and more exist
	Resolve    time.Duration // walking parents up to the root via dirs
	DirsLooked int           // dir rows fetched while resolving
	RoundTrips int           // dirs queries issued
}

func (s Stats) String() string {
	more := ""
	if s.Capped {
		more = "+"
	}
	return fmt.Sprintf("query %s (%d candidates, %d%s matches), paths %s (%d dirs in %d queries)",
		s.Query.Round(time.Millisecond), s.Candidates, s.Matches, more, s.Resolve.Round(time.Millisecond), s.DirsLooked, s.RoundTrips)
}

// searchParallelWorkers caps parallel workers for a full-scan search; the
// server's max_parallel_workers still applies.
const searchParallelWorkers = 8

type matchRow struct {
	parent int64
	name   string
	ino    int64
	typ    byte
	uid    int64
	size   int64
	mtime  int64
	ctime  int64
}

// NewPool creates a pgxpool configured for cephfs-index. The DSN must not
// contain a password — pgx reads ~/.pgpass (or $PGPASSFILE) automatically.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// SQL_ASCII databases accept any byte sequence. Setting client_encoding
	// explicitly avoids confusion if the client's default differs.
	config.ConnConfig.RuntimeParams["client_encoding"] = "SQL_ASCII"
	config.MaxConns = maxConns
	return pgxpool.NewWithConfig(ctx, config)
}

// Open reads the meta table of fsName schema and checks that the UNLOGGED
// entries table survived Postgres crash recovery.
func Open(ctx context.Context, pool *pgxpool.Pool, fsName string) (*DB, error) {
	if err := ValidateName(fsName); err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT key, value FROM `+qi(fsName)+`.meta`)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", fsName, err)
	}
	defer rows.Close()
	meta := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		meta[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The meta table is LOGGED and survives crash recovery; entries is UNLOGGED
	// and does not. Detect the mismatch so callers get a clear error instead of
	// silently returning 0 results.
	if meta["entries"] != "" && meta["entries"] != "0" {
		// Querying the partitioned parent (entries) with LIMIT 1 forces the
		// planner to load catalog metadata for every leaf partition — on cold
		// buffer cache with 84–100 partitions per volume this takes 16–112 s of
		// planning time per volume (all on HDDs). Query the first leaf heap
		// table by name instead: single-table planning is O(1) regardless of
		// partition count, and all leaves are truncated together on crash
		// recovery so checking one is sufficient.
		var leaf string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(
				(SELECT c.relname FROM pg_class c
				 JOIN pg_namespace n ON n.oid = c.relnamespace
				 WHERE n.nspname = $1 AND c.relkind = 'r' AND c.relname LIKE 'entries%'
				 ORDER BY c.relname LIMIT 1),
				'entries')`, fsName).Scan(&leaf); err != nil {
			return nil, fmt.Errorf("crash check for %s: %w", fsName, err)
		}
		var hasRows bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM `+qi(fsName)+`.`+qi(leaf)+` LIMIT 1)`).Scan(&hasRows); err != nil {
			return nil, fmt.Errorf("crash check for %s: %w", fsName, err)
		}
		if !hasRows {
			return nil, fmt.Errorf("schema %s: entries table is empty (UNLOGGED tables are cleared by Postgres crash recovery); rebuild with cephfs-indexd build --pg-dsn", fsName)
		}
	}

	d := &DB{pool: pool, fsName: fsName, Meta: meta}
	d.prefix = strings.TrimSuffix(meta["prefix"], "/")
	d.layout, _ = strconv.Atoi(meta["layout"])
	if d.layout == 0 {
		// Built by a cephfs-index version just before layout 2: covering
		// indexes and vacuumed chunks, but no layout key yet.
		var cover bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, qi(fsName)+".dirs_ino_cover").Scan(&cover); err != nil {
			return nil, fmt.Errorf("layout check for %s: %w", fsName, err)
		}
		if cover {
			d.layout = 1
		}
	}
	return d, nil
}

// Schemas returns schema names that look like live cephfs-index schemas
// (they have a meta table and do not end in _new or _partial).
func Schemas(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT n.nspname
		FROM pg_catalog.pg_namespace n
		JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid
		WHERE c.relname = 'meta'
		  AND n.nspname NOT LIKE '%_new'
		  AND n.nspname NOT LIKE '%_partial'
		ORDER BY n.nspname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Building returns the volumes with an index build in progress: those
// with an <fs>_new schema (cephfs-indexd's staging schema). While a build
// runs, its COPY and index builds compete with searches for disk I/O.
func Building(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT left(n.nspname, -4)
		FROM pg_catalog.pg_namespace n
		JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid
		WHERE c.relname = 'meta'
		  AND n.nspname LIKE '%\_new'
		ORDER BY n.nspname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Search calls emit for the first q.Limit entries whose name matches q.Re
// and counts all of them in Stats.Matches (see Stats.Capped). Matches are
// emitted in batches while the name query is still being read: each batch's
// parent dirs are resolved with a few queries on the dirs table, and resolved
// dirs are cached across batches, so an export of millions of matches runs
// in bounded memory.
//
// Postgres is used as a pre-filter only: the btree index on name handles
// prefix patterns; all other patterns do a sequential scan. Every row is
// re-checked against q.Re client-side because Postgres uses ARE regex dialect
// which differs from Go RE2 (e.g. \b means backspace in ARE, not word boundary).
func (d *DB) Search(ctx context.Context, q Query, emit func(Match) error) (Stats, error) {
	var st Stats
	pl := buildQuery(d.fsName, q).forLayout(d.layout)

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx)
	set := pl.settings(q.Timeout)
	for _, s := range set {
		if _, err := tx.Exec(ctx, s); err != nil {
			return st, err
		}
	}
	t0 := time.Now()
	pgRows, err := tx.Query(ctx, pl.sql, pl.args...)
	if err != nil {
		return st, err
	}
	defer pgRows.Close()

	// Resolving uses other pool connections while this one streams rows.
	res := newResolver(d, &st, q.DirWorkers)
	var batch []matchRow
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		t := time.Now()
		dirs, err := res.resolve(ctx, batch)
		st.Resolve += time.Since(t)
		if err != nil {
			return err
		}
		for i := range batch {
			r := &batch[i]
			if err := emit(Match{
				Type:  r.typ,
				UID:   uint32(r.uid),
				Size:  r.size,
				Mtime: r.mtime,
				Ctime: r.ctime,
				Path:  dirs[r.parent] + "/" + r.name,
			}); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}

	keep := keeper{limit: q.Limit}
	for pgRows.Next() {
		st.Candidates++
		var r matchRow
		var typ string
		if err := pgRows.Scan(&r.parent, &r.name, &r.ino, &typ, &r.uid, &r.size, &r.mtime, &r.ctime); err != nil {
			return st, err
		}
		r.typ = typ[0]
		// Client-side re-check guards against Postgres ARE vs Go RE2 differences.
		if !q.Re.MatchString(r.name) {
			continue
		}
		if !keep.add() {
			continue
		}
		batch = append(batch, r)
		if len(batch) == emitBatch {
			if err := flush(); err != nil {
				return st, err
			}
		}
	}
	pgRows.Close()
	if err := pgRows.Err(); err != nil {
		return st, err
	}
	// The name query is done; release the transaction's connection.
	tx.Rollback(ctx)
	if err := flush(); err != nil {
		return st, err
	}

	st.Query = time.Since(t0) - st.Resolve
	st.Matches = keep.n
	if pl.sqlLimit > 0 && st.Candidates == pl.sqlLimit {
		// The query stopped early: there are more matches than were read.
		// Report the returned rows and flag the count as a lower bound.
		st.Matches, st.Capped = keep.kept, true
	}
	return st, nil
}

// emitBatch is how many matches are resolved and emitted together.
const emitBatch = 5000

// plan is the name query for a Query and how to run it.
type plan struct {
	sql  string
	args []any
	// fullScan: no name index applies; the session makes a parallel
	// sequential scan cheap.
	fullScan bool
	// sqlLimit is the LIMIT added to the query, or 0. It is set only when
	// the SQL filter selects exactly the matching names (see PureLiteral),
	// so every row read is a match and reading Limit+1 rows is enough to
	// know whether more exist. Without it, a common name costs one random
	// heap read per match, all of which would be read just to count them.
	sqlLimit int
	// bitmap: use a bitmap heap scan instead of a plain index scan, so the
	// matching rows are read in physical order with read-ahead (PG18 read
	// streams, effective_io_concurrency) instead of one random read at a
	// time. Only for exact names and prefixes of at least minBitmapPrefix
	// bytes: the bitmap index scan collects every match of a chunk before
	// the LIMIT applies, which a broad prefix would make expensive.
	bitmap bool
}

const minBitmapPrefix = 3

// bitmapSettings steer the planner to a bitmap heap scan. enable_seqscan =
// off only makes a seq scan very expensive: with a LIMIT the planner could
// otherwise expect to find the rows early in a sequential read of the whole
// chunk.
var bitmapSettings = []string{`SET LOCAL enable_indexscan = off`, `SET LOCAL enable_seqscan = off`}

// forLayout drops the bitmap-scan steering on volumes with covering indexes
// (layout 1 and 2). Their name and dirs indexes cover every column read and
// their tables are all-visible, so the planner picks an index-only scan
// without heap fetches, and enable_indexscan = off would rule that out: it
// disables index-only scans too.
func (pl plan) forLayout(layout int) plan {
	if layout >= 1 {
		pl.bitmap = false
	}
	return pl
}

// settings returns the SET LOCAL statements for the name query.
func (pl plan) settings(timeout time.Duration) []string {
	var set []string
	if timeout > 0 {
		set = append(set, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, timeout.Milliseconds()))
	}
	if pl.fullScan {
		// A full scan returns few candidates, but the planner estimates a
		// third of the table for strpos(...) > 0 (6.9M estimated vs 10 real on a 20M-entry volume),
		// prices the Gather of that many tuples and picks a single-process
		// seq scan. These make parallel plans cheap so the scan is split.
		set = append(set,
			`SET LOCAL parallel_setup_cost = 0`,
			`SET LOCAL parallel_tuple_cost = 0`,
			fmt.Sprintf(`SET LOCAL max_parallel_workers_per_gather = %d`, searchParallelWorkers))
	}
	if pl.bitmap {
		set = append(set, bitmapSettings...)
	}
	return set
}

func buildQuery(fsName string, q Query) (pl plan) {
	var sb strings.Builder
	var args []any
	p := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	sb.WriteString(`SELECT parent, name, ino, type, uid, size, mtime, ctime FROM ` + qi(fsName) + `.entries WHERE `)

	kind, lit := PureLiteral(q.Pattern)
	exactFilter := false
	pl.fullScan = true
	if kind == Exact {
		pl.fullScan, pl.bitmap, exactFilter = false, true, true
		sb.WriteString(`name = ` + p(lit))
	} else if prefix := NamePrefix(q.Pattern); prefix != "" {
		pl.fullScan, pl.bitmap = false, len(prefix) >= minBitmapPrefix
		sb.WriteString(`name >= ` + p(prefix))
		if hi, ok := UpperBound(prefix); ok {
			sb.WriteString(` AND name < ` + p(hi))
			exactFilter = kind == Prefix
		}
		// No server-side regex: the btree range already bounds the result set,
		// and client-side re-check filters to exact matches.
	} else if lits := RequiredLiterals(q.Pattern); len(lits) > 0 {
		// Sequential scan, but filtered server-side on substrings every match
		// must contain, so only candidates cross the wire. strpos compares
		// bytes and needs no regex dialect translation.
		for i, l := range lits {
			if i > 0 {
				sb.WriteString(` AND `)
			}
			sb.WriteString(`strpos(name, ` + p(l) + `) > 0`)
		}
		exactFilter = kind == Contains
	} else {
		// No usable literal: every row is sent and filtered client-side.
		sb.WriteString(`true`)
	}

	if q.Type != 0 {
		sb.WriteString(` AND type = ` + p(string([]byte{q.Type})))
	}
	if q.UID >= 0 {
		// Hardlinks carry no owner (stored as 0); they never match a uid filter.
		sb.WriteString(` AND uid = ` + p(q.UID) + ` AND type != ` + p(string([]byte{TypeHardlink})))
	}
	if q.Mtime > 0 {
		sb.WriteString(` AND mtime >= ` + p(q.Mtime))
	}
	if exactFilter && q.Limit != 0 {
		pl.sqlLimit = max(q.Limit, 0) + 1
		sb.WriteString(fmt.Sprintf(` LIMIT %d`, pl.sqlLimit))
	}
	if pl.sqlLimit == 1 {
		// Count-only volume ("are there more?"): a plain index scan returns
		// the first row with one read, a bitmap scan would first collect
		// every match of each chunk.
		pl.bitmap = false
	}
	pl.sql, pl.args = sb.String(), args
	return pl
}

// keeper decides which matches to keep: the first limit (all if limit is
// 0, none if it is negative). It counts every match.
type keeper struct {
	limit int
	n     int // matches seen
	kept  int
}

func (k *keeper) add() bool {
	k.n++
	if k.limit == 0 || k.kept < k.limit {
		k.kept++
		return true
	}
	return false
}

type dirInfo struct {
	parent int64
	name   string
}

// resolver turns parent dir inos into full paths. It fetches ancestors level
// by level with ino = ANY($1::bigint[]) (about one query per tree level) and
// keeps what it fetched and built across calls, so later batches of the same
// search mostly hit the cache.
type resolver struct {
	d       *DB
	st      *Stats
	workers int
	info    map[int64]dirInfo
	orphan  map[int64]bool // referenced but not in dirs (not seen during the walk)
	paths   map[int64]string
}

// maxCachedDirs bounds the resolver's memory on huge exports; past it the
// cache is dropped and rebuilt from the dirs table as needed.
const maxCachedDirs = 1_000_000

func newResolver(d *DB, st *Stats, workers int) *resolver {
	r := &resolver{d: d, st: st, workers: max(workers, 1)}
	r.reset()
	return r
}

func (r *resolver) reset() {
	r.info = map[int64]dirInfo{}
	r.orphan = map[int64]bool{}
	r.paths = map[int64]string{RootIno: r.d.prefix}
}

// resolve returns a map that holds the path of every parent in hits.
func (r *resolver) resolve(ctx context.Context, hits []matchRow) (map[int64]string, error) {
	if len(r.info) > maxCachedDirs || len(r.paths) > maxCachedDirs {
		r.reset()
	}
	var level []int64
	queued := map[int64]bool{}
	want := func(ino int64) {
		if queued[ino] || r.orphan[ino] {
			return
		}
		if _, ok := r.paths[ino]; ok {
			return
		}
		if _, ok := r.info[ino]; ok {
			// Fetched earlier, so its ancestors are in info or orphan too:
			// a resolve fetches level by level until every parent is known.
			return
		}
		queued[ino] = true
		level = append(level, ino)
	}
	for i := range hits {
		want(hits[i].parent)
	}

	for len(level) > 0 {
		cur := level
		level = nil
		chunks := splitChunks(cur, r.workers, minDirChunk, maxDirChunk)
		found := make([]map[int64]dirInfo, len(chunks))
		errs := make([]error, len(chunks))
		var wg sync.WaitGroup
		sem := make(chan struct{}, r.workers)
		for i, ch := range chunks {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				found[i], errs[i] = r.fetch(ctx, ch)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		r.st.RoundTrips += len(chunks)
		for _, f := range found {
			for ino, di := range f {
				r.info[ino] = di
			}
			r.st.DirsLooked += len(f)
		}
		for _, ino := range cur {
			if di, ok := r.info[ino]; ok {
				want(di.parent)
			} else {
				r.orphan[ino] = true
			}
		}
	}

	parents := make([]int64, len(hits))
	for i := range hits {
		parents[i] = hits[i].parent
	}
	buildPaths(r.paths, r.info, r.orphan, parents)
	return r.paths, nil
}

// Parent dirs of one tree level are fetched in chunks of minDirChunk to
// maxDirChunk inos, up to resolver.workers chunks at a time.
const (
	minDirChunk = 32
	maxDirChunk = 10000
)

// splitChunks splits inos into about workers chunks of at least minSize
// (except the last) and at most maxSize.
func splitChunks(inos []int64, workers, minSize, maxSize int) [][]int64 {
	size := (len(inos) + workers - 1) / max(workers, 1)
	size = min(max(size, minSize), maxSize)
	var out [][]int64
	for off := 0; off < len(inos); off += size {
		out = append(out, inos[off:min(off+size, len(inos))])
	}
	return out
}

// fetch reads the dirs rows of inos. On layout 2 the covering dirs index
// makes this an index-only scan; before, like the name query, it uses a
// bitmap heap scan so the rows are read in physical order with read-ahead.
func (r *resolver) fetch(ctx context.Context, inos []int64) (map[int64]dirInfo, error) {
	tx, err := r.d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if (plan{bitmap: true}).forLayout(r.d.layout).bitmap {
		for _, s := range bitmapSettings {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, err
			}
		}
	}
	rows, err := tx.Query(ctx,
		`SELECT ino, parent, name FROM `+qi(r.d.fsName)+`.dirs WHERE ino = ANY($1::bigint[])`, inos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]dirInfo, len(inos))
	for rows.Next() {
		var ino int64
		var di dirInfo
		if err := rows.Scan(&ino, &di.parent, &di.name); err != nil {
			return nil, err
		}
		out[ino] = di
	}
	return out, rows.Err()
}

// buildPaths adds to paths the full path of every ino in want (plus the
// ancestors resolved on the way), walking info up to a known path once per
// chain and memoizing in paths, which must hold the root. Inos missing from
// info or listed in orphan resolve to "<ino 0x...>" and their descendants
// hang off that placeholder.
func buildPaths(paths map[int64]string, info map[int64]dirInfo, orphan map[int64]bool, want []int64) {
	var chain []int64
	for _, ino := range want {
		chain = chain[:0]
		base := ""
		for x := ino; ; {
			if p, ok := paths[x]; ok {
				base = p
				break
			}
			di, ok := info[x]
			if orphan[x] || !ok || len(chain) > 4096 {
				base = fmt.Sprintf("<ino 0x%x>", x)
				paths[x] = base
				break
			}
			chain = append(chain, x)
			x = di.parent
		}
		for i := len(chain) - 1; i >= 0; i-- {
			base = base + "/" + info[chain[i]].name
			paths[chain[i]] = base
		}
	}
}

func qi(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// VolumeStats holds aggregated statistics for one volume's index.
type VolumeStats struct {
	TableBytes     int64    `json:"table_bytes"`
	Files          int64    `json:"files"`
	Dirs           int64    `json:"dirs"`
	Symlinks       int64    `json:"symlinks"`
	Hardlinks      int64    `json:"hardlinks"`
	Special        int64    `json:"special"`          // device nodes, FIFOs, sockets; only known from exact counts
	CountsExact    bool     `json:"counts_exact"`     // type counts from meta, written by cephfs-indexd; else estimated
	AvgFileSize    *float64 `json:"avg_file_size"`    // nil if no files
	MedianFileSize *int64   `json:"median_file_size"` // nil if no files
	P90FileSize    *int64   `json:"p90_file_size"`    // nil if no files
	P99FileSize    *int64   `json:"p99_file_size"`    // nil if no files
}

// metaTypeCounts returns files, dirs, symlinks, hardlinks and special from
// fsName's meta, or nil if any of them is missing (built by a cephfs-indexd
// that did not count types yet).
func metaTypeCounts(ctx context.Context, pool *pgxpool.Pool, fsName string) ([]int64, error) {
	keys := []string{"files", "dirs", "symlinks", "hardlinks", "special"}
	rows, err := pool.Query(ctx, `SELECT key, value FROM `+qi(fsName)+`.meta WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("stats meta for %s: %w", fsName, err)
	}
	defer rows.Close()
	got := map[string]int64{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("stats meta for %s: %s = %q: %w", fsName, k, v, err)
		}
		got[k] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]int64, len(keys))
	for i, k := range keys {
		n, ok := got[k]
		if !ok {
			return nil, nil
		}
		out[i] = n
	}
	return out, nil
}

// ComputeStats gathers type counts and file-size distribution for fsName.
//
// Type counts come from meta (files, dirs, symlinks, hardlinks, special),
// which cephfs-indexd writes exactly during the build. For volumes built
// before it did, they are estimated from pg_stats.most_common_vals ×
// pg_class.reltuples — pre-computed by ANALYZE, no table scan needed, but
// rare types are missed in leaves whose sample had none. Avg and percentiles come
// from a 1% TABLESAMPLE SYSTEM of each leaf partition, which reads ~1% of
// disk pages without sorting the full table.
func ComputeStats(ctx context.Context, pool *pgxpool.Pool, fsName string) (VolumeStats, error) {
	if err := ValidateName(fsName); err != nil {
		return VolumeStats{}, err
	}
	var vs VolumeStats

	// Table bytes via pg_class (no table scan).
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(pg_total_relation_size(c.oid)), 0)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'`, fsName).Scan(&vs.TableBytes); err != nil {
		return VolumeStats{}, fmt.Errorf("stats table size for %s: %w", fsName, err)
	}

	exact, err := metaTypeCounts(ctx, pool, fsName)
	if err != nil {
		return VolumeStats{}, err
	}
	if exact != nil {
		vs.Files, vs.Dirs, vs.Symlinks, vs.Hardlinks, vs.Special = exact[0], exact[1], exact[2], exact[3], exact[4]
		vs.CountsExact = true
	}

	// Enumerate leaf partitions, joining pg_stats for the type column so we
	// can approximate per-type counts from most_common_vals × reltuples.
	// This touches only catalog tables — no table scan at all.
	srows, err := pool.Query(ctx, `
		SELECT
			c.relname,
			GREATEST(c.reltuples, 0)::bigint,
			COALESCE(s.most_common_vals::text, ''),
			COALESCE(s.most_common_freqs::text, '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_stats s ON s.schemaname = n.nspname
			AND s.tablename = c.relname AND s.attname = 'type'
		WHERE n.nspname = $1 AND c.relkind = 'r' AND c.relname LIKE 'entries%'
		ORDER BY c.relname`, fsName)
	if err != nil {
		return VolumeStats{}, fmt.Errorf("stats partitions for %s: %w", fsName, err)
	}
	var leaves []string
	for srows.Next() {
		var relname string
		var reltuples int64
		var valsText, freqsText string
		if err := srows.Scan(&relname, &reltuples, &valsText, &freqsText); err != nil {
			srows.Close()
			return VolumeStats{}, err
		}
		leaves = append(leaves, relname)
		vals := parsePGTextArray(valsText)
		freqs := parsePGFloatArray(freqsText)
		for i, v := range vals {
			if i >= len(freqs) {
				break
			}
			if vs.CountsExact {
				break
			}
			count := int64(float64(reltuples) * freqs[i])
			switch v {
			case "f":
				vs.Files += count
			case "d":
				vs.Dirs += count
			case "l":
				vs.Symlinks += count
			case "h":
				vs.Hardlinks += count
			}
		}
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return VolumeStats{}, fmt.Errorf("stats partitions for %s: %w", fsName, err)
	}
	if len(leaves) == 0 {
		return vs, nil
	}

	// Avg and percentiles from a 1% page-level TABLESAMPLE of each leaf.
	// SYSTEM sampling reads ~1% of disk pages (no full sort), which is orders
	// of magnitude faster than PERCENTILE_DISC on the full table.
	var unions []string
	for _, leaf := range leaves {
		unions = append(unions, `SELECT size FROM `+
			qi(fsName)+`.`+qi(leaf)+
			` TABLESAMPLE SYSTEM(1) WHERE type = 'f' AND size IS NOT NULL`)
	}
	var avg pgtype.Float8
	var p50, p90, p99 pgtype.Int8
	if err := pool.QueryRow(ctx, `
		SELECT
			AVG(size)::float8,
			PERCENTILE_DISC(0.50) WITHIN GROUP (ORDER BY size),
			PERCENTILE_DISC(0.90) WITHIN GROUP (ORDER BY size),
			PERCENTILE_DISC(0.99) WITHIN GROUP (ORDER BY size)
		FROM (`+strings.Join(unions, " UNION ALL ")+`) s`).Scan(&avg, &p50, &p90, &p99); err != nil {
		return VolumeStats{}, fmt.Errorf("stats sample for %s: %w", fsName, err)
	}
	if avg.Valid {
		v := avg.Float64
		vs.AvgFileSize = &v
	}
	if p50.Valid {
		v := p50.Int64
		vs.MedianFileSize = &v
	}
	if p90.Valid {
		v := p90.Int64
		vs.P90FileSize = &v
	}
	if p99.Valid {
		v := p99.Int64
		vs.P99FileSize = &v
	}
	return vs, nil
}

// parsePGTextArray parses a PostgreSQL array literal like {f,d,l,h}.
func parsePGTextArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// parsePGFloatArray parses a PostgreSQL float array literal like {0.75,0.2,0.04,0.01}.
func parsePGFloatArray(s string) []float64 {
	parts := parsePGTextArray(s)
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err == nil {
			out = append(out, v)
		}
	}
	return out
}

// ValidateName rejects fs/schema names that contain characters outside
// [a-z0-9_]. This prevents SQL injection via the qi() helper and matches
// typical CephFS volume names.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("schema name is empty")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return fmt.Errorf("schema name %q contains invalid character %q (only [a-z0-9_] allowed)", name, c)
		}
	}
	return nil
}
