package pgsearch

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildPaths(t *testing.T) {
	root := int64(RootIno)
	info := map[int64]dirInfo{
		10: {root, "home"},
		11: {10, "alice"},
		12: {11, "project"},
		13: {12, "content"},
		14: {13, "plugins"},
		20: {12, "other"},
		30: {99, "lost"}, // parent 99 never fetched
		40: {41, "child"},
	}
	orphan := map[int64]bool{41: true}
	// Deepest first, so the shared ancestors are resolved by the first walk
	// and reused by later ones.
	want := []int64{14, 20, 13, root, 30, 40, 41}
	got := map[int64]string{root: "/mnt/cephfs/vol1"}
	buildPaths(got, info, orphan, want)
	exp := map[int64]string{
		14:   "/mnt/cephfs/vol1/home/alice/project/content/plugins",
		20:   "/mnt/cephfs/vol1/home/alice/project/other",
		13:   "/mnt/cephfs/vol1/home/alice/project/content",
		root: "/mnt/cephfs/vol1",
		30:   "<ino 0x63>/lost",
		40:   "<ino 0x29>/child",
		41:   "<ino 0x29>",
	}
	for ino, p := range exp {
		if got[ino] != p {
			t.Errorf("path(%d) = %q, want %q", ino, got[ino], p)
		}
	}
}

func TestKeeper(t *testing.T) {
	for _, tc := range []struct{ limit, add, kept int }{
		{0, 5, 5},
		{3, 5, 3},
		{10, 5, 5},
		{1, 0, 0},
		{-1, 5, 0}, // count only
	} {
		k := keeper{limit: tc.limit}
		var got []int
		for i := range tc.add {
			if k.add() {
				got = append(got, i)
			}
		}
		if k.n != tc.add || k.kept != tc.kept || len(got) != tc.kept {
			t.Errorf("limit %d, %d added: n=%d kept=%d, want n=%d kept=%d", tc.limit, tc.add, k.n, k.kept, tc.add, tc.kept)
		}
		for i, g := range got {
			if g != i {
				t.Errorf("limit %d: kept %v, want the first rows in order", tc.limit, got)
				break
			}
		}
	}
}

func TestBuildQuery(t *testing.T) {
	const sel = `SELECT parent, name, ino, type, uid, size, mtime, ctime FROM "vol1".entries WHERE `
	for _, tc := range []struct {
		pattern string
		limit   int
		where   string
		full    bool
		sqlLim  int
		bitmap  bool
	}{
		{`^backup\.json$`, 1000, `name = $1 LIMIT 1001`, false, 1001, true},
		{`^backup\.json$`, -1, `name = $1 LIMIT 1`, false, 1, false}, // count-only volume: one index read
		{`^backup\.json$`, 0, `name = $1`, false, 0, true},           // unlimited
		{`^tmp_`, 1000, `name >= $1 AND name < $2 LIMIT 1001`, false, 1001, true},
		{`^ab`, 1000, `name >= $1 AND name < $2 LIMIT 1001`, false, 1001, false}, // broad prefix: plain index scan
		{`^tmp_[0-9]+$`, 1000, `name >= $1 AND name < $2`, false, 0, true},       // needs the regex re-check
		{`app-config\.php`, 1000, `strpos(name, $1) > 0 LIMIT 1001`, true, 1001, false},
		{`app-config\.php$`, 1000, `strpos(name, $1) > 0`, true, 0, false},
		{`tmp_[0-9a-z]+\.tmp`, 1000, `strpos(name, $1) > 0 AND strpos(name, $2) > 0`, true, 0, false},
	} {
		q := Query{Pattern: tc.pattern, UID: -1, Limit: tc.limit}
		pl := buildQuery("vol1", q)
		if pl.sql != sel+tc.where || pl.fullScan != tc.full || pl.sqlLimit != tc.sqlLim || pl.bitmap != tc.bitmap {
			t.Errorf("%q limit %d:\n got %s (full=%v limit=%d bitmap=%v)\nwant %s (full=%v limit=%d bitmap=%v)",
				tc.pattern, tc.limit, pl.sql, pl.fullScan, pl.sqlLimit, pl.bitmap, sel+tc.where, tc.full, tc.sqlLim, tc.bitmap)
		}
	}

	// Filters go before the LIMIT.
	q := Query{Pattern: `^a\.b$`, Type: TypeFile, UID: 33, Mtime: 1700000000, Limit: 10}
	pl := buildQuery("vol1", q)
	want := sel + `name = $1 AND type = $2 AND uid = $3 AND type != $4 AND mtime >= $5 LIMIT 11`
	if pl.sql != want || len(pl.args) != 5 || pl.args[0] != "a.b" {
		t.Errorf("filters:\n got %s %v\nwant %s", pl.sql, pl.args, want)
	}
}

func TestForLayout(t *testing.T) {
	pl := buildQuery("vol1", Query{Pattern: `^backup\.json$`, UID: -1, Limit: 500})
	for layout, want := range map[int]bool{0: true, 1: false, 2: false, 3: false} {
		if got := pl.forLayout(layout).bitmap; got != want {
			t.Errorf("layout %d: bitmap %v, want %v", layout, got, want)
		}
	}
	if got := strings.Join(pl.forLayout(2).settings(0), ";"); strings.Contains(got, "enable_indexscan") {
		t.Errorf("layout 2 must not disable index scans (it disables index-only scans too): %s", got)
	}
}

func TestPlanSettings(t *testing.T) {
	for _, tc := range []struct {
		pl   plan
		want string
	}{
		{plan{bitmap: true}, "SET LOCAL statement_timeout = 60000;SET LOCAL enable_indexscan = off;SET LOCAL enable_seqscan = off"},
		{plan{fullScan: true}, "SET LOCAL statement_timeout = 60000;SET LOCAL parallel_setup_cost = 0;SET LOCAL parallel_tuple_cost = 0;SET LOCAL max_parallel_workers_per_gather = 8"},
		{plan{}, "SET LOCAL statement_timeout = 60000"},
	} {
		if got := strings.Join(tc.pl.settings(time.Minute), ";"); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.pl, got, tc.want)
		}
	}
	if got := (plan{bitmap: true}).settings(0); len(got) != 2 {
		t.Errorf("no timeout: %v", got)
	}
}

func TestSplitChunks(t *testing.T) {
	ids := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(i)
		}
		return out
	}
	for _, tc := range []struct {
		n, workers int
		sizes      string
	}{
		{500, 4, "125 125 125 125"},
		{10, 4, "10"},                               // below minDirChunk: one query
		{70, 4, "32 32 6"},                          // min size 32
		{50000, 4, "10000 10000 10000 10000 10000"}, // max size 10000
		{0, 4, ""},
		{7, 1, "7"},
	} {
		var sizes []string
		total := 0
		for _, c := range splitChunks(ids(tc.n), tc.workers, 32, 10000) {
			sizes = append(sizes, strconv.Itoa(len(c)))
			total += len(c)
		}
		if got := strings.Join(sizes, " "); got != tc.sizes || total != tc.n {
			t.Errorf("splitChunks(%d, %d) = %q, want %q", tc.n, tc.workers, got, tc.sizes)
		}
	}
}

func TestNamedPaths(t *testing.T) {
	const sel = `SELECT path, type, uid, size, mtime, ctime FROM "vol1".named_paths WHERE name = $1`
	for _, tc := range []struct {
		q    Query
		want string
		lim  int
	}{
		{Query{Limit: 500, UID: -1}, sel + ` LIMIT 501`, 501},
		{Query{Limit: -1, UID: -1}, sel + ` LIMIT 1`, 1},
		{Query{UID: -1}, sel, 0},
		{Query{Type: TypeDir, UID: 33, Mtime: 1700000000, Limit: 10}, sel + ` AND type = $2 AND uid = $3 AND type != $4 AND mtime >= $5 LIMIT 11`, 11},
	} {
		pl := buildNamedQuery("vol1", "mu-plugins", tc.q)
		if pl.sql != tc.want || pl.sqlLimit != tc.lim || pl.args[0] != "mu-plugins" {
			t.Errorf("%+v:\n got %s (limit %d)\nwant %s", tc.q, pl.sql, pl.sqlLimit, tc.want)
		}
	}

	d := &DB{named: map[string]bool{"mu-plugins": true}}
	for pattern, want := range map[string]bool{
		`^mu-plugins$`: true,  // exact
		`^mu-plugins`:  false, // prefix: other names too
		`mu-plugins`:   false, // contains
		`^plugins$`:    false, // not covered
	} {
		if _, ok := d.namedName(Query{Pattern: pattern}); ok != want {
			t.Errorf("%s: named=%v, want %v", pattern, ok, want)
		}
	}
	if _, ok := (&DB{}).namedName(Query{Pattern: `^mu-plugins$`}); ok {
		t.Error("volume without named_paths must not use it")
	}
}
