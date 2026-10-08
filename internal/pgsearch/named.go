package pgsearch

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Volumes built by cephfs-indexd with --pg-named-paths (v1.2.0+) carry
// <fs>.named_paths: the precomputed path of every entry with one of the
// names in <fs>.named_paths_names. path is relative to the volume root
// ("/a/b/name") or starts with "<ino 0x...>" for a broken chain, the same
// strings resolver builds. An exact search for a covered name reads that
// table instead of resolving parents level by level through dirs.

// loadNamedPaths reads the names covered by named_paths, if the table exists.
func (d *DB) loadNamedPaths(ctx context.Context) error {
	var exists bool
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, qi(d.fsName)+".named_paths_names").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	rows, err := d.pool.Query(ctx, `SELECT name FROM `+qi(d.fsName)+`.named_paths_names`)
	if err != nil {
		return err
	}
	defer rows.Close()
	d.named = map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		d.named[n] = true
	}
	return rows.Err()
}

// namedName returns the name q searches for exactly, if named_paths covers it.
func (d *DB) namedName(q Query) (string, bool) {
	kind, lit := PureLiteral(q.Pattern)
	return lit, kind == Exact && d.named[lit]
}

// buildNamedQuery is buildQuery for named_paths: the same filters and the
// same LIMIT rule, since every row read is a match.
func buildNamedQuery(fsName, name string, q Query) (pl plan) {
	var sb strings.Builder
	var args []any
	p := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	sb.WriteString(`SELECT path, type, uid, size, mtime, ctime FROM ` + qi(fsName) + `.named_paths WHERE name = ` + p(name))
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
	if q.Limit != 0 {
		pl.sqlLimit = max(q.Limit, 0) + 1
		sb.WriteString(fmt.Sprintf(` LIMIT %d`, pl.sqlLimit))
	}
	pl.sql, pl.args = sb.String(), args
	return pl
}

func (d *DB) searchNamed(ctx context.Context, name string, q Query, emit func(Match) error) (Stats, error) {
	st := Stats{NamedPaths: true}
	pl := buildNamedQuery(d.fsName, name, q)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx)
	for _, s := range pl.settings(q.Timeout) {
		if _, err := tx.Exec(ctx, s); err != nil {
			return st, err
		}
	}
	t0 := time.Now()
	rows, err := tx.Query(ctx, pl.sql, pl.args...)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	keep := keeper{limit: q.Limit}
	for rows.Next() {
		st.Candidates++
		var path, typ string
		var uid int64
		var m Match
		if err := rows.Scan(&path, &typ, &uid, &m.Size, &m.Mtime, &m.Ctime); err != nil {
			return st, err
		}
		if !keep.add() {
			continue
		}
		if strings.HasPrefix(path, "/") {
			path = d.prefix + path
		}
		m.Type, m.UID, m.Path = typ[0], uint32(uid), path
		if err := emit(m); err != nil {
			return st, err
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	st.Query = time.Since(t0)
	st.Matches = keep.n
	if pl.sqlLimit > 0 && st.Candidates == pl.sqlLimit {
		st.Matches, st.Capped = keep.kept, true
	}
	return st, nil
}
