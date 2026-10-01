# cephfs-fe

A web frontend for the CephFS name indexes that `cephfs-indexd build --pg-dsn` writes to PostgreSQL ([cephfs-index](https://github.com/xorpaul/cephfs-index)). You can find files by name without shell access to the cluster. It serves a single search page and a small JSON API. Clients log in with a TLS client certificate whose subject is on an allow list, or, without a PKI, with a user and password from an htpasswd file.

It is a single static binary with no cgo. Its dependencies are pgx and, for basic auth, `golang.org/x/crypto/bcrypt`. The page, script and stylesheet are embedded in the binary.

## TL;DR

1. Build an index with [cephfs-indexd](https://github.com/xorpaul/cephfs-index) into PostgreSQL.
2. Download `cephfs-fe` from the [releases](https://github.com/xorpaul/cephfs-fe/releases) to a host that can reach the database:
   ```bash
   curl -fLo /usr/local/bin/cephfs-fe https://github.com/xorpaul/cephfs-fe/releases/download/v1.0.0/cephfs-fe_v1.0.0_linux-amd64 && chmod +x /usr/local/bin/cephfs-fe
   ```
3. Give it database access with the password in a pgpass file (mode 0600, owned by the service user; the sample unit sets `PGPASSFILE=/etc/cephfs-fe/pgpass`). The simplest is the role that runs the builds, since it owns the schemas. For a separate read-only role, grant through default privileges: cephfs-indexd creates each volume's schema anew on every build, so plain grants are lost at the next build.
   ```sql
   CREATE ROLE cephfs_fe LOGIN PASSWORD '...';
   ALTER DEFAULT PRIVILEGES FOR ROLE cephfs_index GRANT USAGE ON SCHEMAS TO cephfs_fe;
   ALTER DEFAULT PRIVILEGES FOR ROLE cephfs_index GRANT SELECT ON TABLES TO cephfs_fe;
   ```
   Then rebuild the volumes once, or grant on the existing schemas by hand. Also create the stats cache table (see [Running](#running)).
4. Create a server certificate, a CA bundle for the client certificates and an allow list of client subject DNs (no PKI for client certificates? use [basic auth](#basic-auth-without-a-pki) instead), then write `/etc/cephfs-fe/config` from [`config.example`](config.example) and start it with the sample [`cephfs-fe.service`](cephfs-fe.service):
   ```bash
   useradd --system --no-create-home cephfs-fe            # the sample unit runs as this user
   openssl x509 -in alice.pem -noout -subject -nameopt RFC2253 | sed 's/^subject=//' >> /etc/cephfs-fe/allowed-subjects
   cp cephfs-fe.service /etc/systemd/system/ && systemctl enable --now cephfs-fe
   ```
5. Open `https://<host>:7788/` with the client certificate installed in the browser.

## Search semantics

- **Name, not path.** The pattern matches entry names, case-sensitive. There are four match modes:

  | match | pattern becomes | query |
  |---|---|---|
  | `exact` (default) | `^name$` | `name = $1` on the btree name index |
  | `prefix` | `^name` | btree range on the name index |
  | `contains` | `name` | parallel sequential scan with a server-side `strpos` filter |
  | `regex` | a Go RE2 pattern (no backreferences or lookaround) | name index if it starts with `^` and a literal, otherwise a sequential scan pre-filtered on its required literals, re-checked in Go |

- **Sequential scans are slow on big volumes.** On a volume with a billion entries on HDD storage, `contains` and unanchored `regex` searches can take many minutes. The page warns before one. Such searches need a literal of at least 2 characters outside an alternation, so none reads the whole table unfiltered.
- **Filters:** volumes, type (file/dir), mtime on or after a date (server local time), and owner uid. Hardlinks (remote dentries) carry no owner, so they never match a uid filter, and they show uid `-`.
- **Row limit:** a search shows at most `limit` rows (default 500) across all selected volumes. Only the shown rows have their paths resolved. For `exact`, `prefix` and single-literal `contains` searches the SQL filter selects exactly the matches, so the query ends with `LIMIT limit+1`: it stops after about 500 random heap reads and reports "500+" instead of reading every match just to count it. Other searches count every match.
- **Disk-friendly reads:** exact-name and prefix (3+ characters) searches, and the parent-dir lookups that build the paths, run with `enable_indexscan = off` and `enable_seqscan = off`. Postgres then uses bitmap heap scans, which read the matching rows in physical order with read-ahead (PG18 read streams, `effective_io_concurrency`) instead of one random read per row. Each tree level of dirs is fetched on up to `dir-workers` connections in parallel, so the RAID serves several reads at once. Short prefixes keep a plain index scan: a bitmap scan collects every match of a chunk before the LIMIT applies. Volumes built by the newer cephfs-indexd (`meta.layout = 2`) have covering name and dirs indexes on all-visible tables, so they are searched with default settings, which gives index-only scans with no heap reads. `enable_indexscan = off` would rule those out too. The query has no ORDER BY, so a truncated result is an arbitrary subset, and sorting in the page reorders only those rows.
- **Export:** *Export all* downloads every match (up to `export-limit`, default 1,000,000) as CSV or JSON lines. Rows stream while the query runs, and paths are resolved in batches of 5000 with a dir cache, so memory stays flat. An export takes a search slot and has its own `export-timeout` (default 30m).
- **Non-UTF-8 paths:** names are raw bytes (the database is `SQL_ASCII`). The page shows a path that is not valid UTF-8 with U+FFFD and flags it. The CSV export writes the raw bytes, and the JSONL export adds `path_b64` with the exact bytes.
- **Index builds:** while cephfs-indexd builds a volume (an `<fs>_new` schema exists), the page shows a banner: the build's COPY and index builds compete with searches for the same disks.

A search can be linked: the form state is kept in the page URL.

## API

All endpoints require the client certificate.

- `GET /api/volumes` returns `{version, building:[fs…], volumes:[{fs, prefix, started_at, entries, complete, journal_flushed, warning, error}]}`.
- `GET /api/search?pattern=P[&match=exact|prefix|contains|regex][&fs=vol1,vol2][&type=file|dir|symlink|hardlink][&newer=YYYY-MM-DD][&uid=N]` returns `{rows:[{fs,path,type,uid,size,mtime,lossy}], total, total_capped, truncated, limit, volumes:[{fs,matches,capped,…}], method, took_ms}`. `total_capped` means the total is a lower bound. The legacy parameter `regex=1` means `match=regex`.
- `GET /api/export?<same parameters>&format=csv|jsonl` streams all matches as an attachment.
  - CSV columns: `fs,path,type,uid,size,mtime,mtime_utc`. An export that ends early gets a final `# ERROR: …` or `# TRUNCATED …` line.
  - JSON lines: one object per match, then always `{"summary":{rows, truncated, skipped, error}}`.

Invalid input gets 400, a timeout gets 504, and every error body is `{error}`. A volume whose index can't be opened (for example `entries` cleared by crash recovery) is skipped and reported (`volumes[].error`, or `skipped` in the export), so it doesn't fail the whole search.

```
curl --cert me.pem --key me.key --cacert server-ca.pem 'https://search.example.org:7788/api/search?pattern=config.yaml&fs=vol1'
curl --cert me.pem --key me.key --cacert server-ca.pem -o backup.csv 'https://search.example.org:7788/api/export?pattern=backup.json&fs=vol1&format=csv'
```

## Running

Settings are read from `/etc/cephfs-fe/config` (another file with `--config`). The format is one `key = value` per line, with blank lines and `#` comment lines ignored. The keys are the flag names below. A flag given on the command line overrides the file, and `pg-dsn` falls back to `$CEPHFS_INDEX_PG_DSN`. If the default file is missing it is skipped; an explicit `--config` that is missing is an error. So are unknown keys and keys set twice. See [`config.example`](config.example).

```
cephfs-fe                                  # /etc/cephfs-fe/config
cephfs-fe --config ./dev.conf --listen :8443
```

| key / flag | default | |
|---|---|---|
| `listen` | `:7788` | listen address; bind it to the management interface |
| `pg-dsn` | `$CEPHFS_INDEX_PG_DSN` | without a password; pgx reads `$PGPASSFILE` or `~/.pgpass` |
| `tls-cert`, `tls-key` | — | server certificate and key |
| `auth` | `mtls` | `mtls`: client certificate on an allow list; `basic`: user and password from `htpasswd-file` |
| `tls-ca` | — | `mtls`: CA bundle that client certificates must chain to |
| `allowed-subjects-file` | — | `mtls`: one client-certificate subject DN per line, `#` comment lines |
| `cert-help-url` | — | `mtls`: optional link on the 401 page, where users get a client certificate |
| `access-help-url` | — | `mtls`: optional link on the 403 page, where users request access |
| `htpasswd-file` | — | `basic`: bcrypt entries, created with `htpasswd -B` |
| `max-concurrent` | 2 | searches running at the same time; others wait. Each full scan asks Postgres for up to 8 parallel workers |
| `max-exports` | 1 | exports running at the same time, in addition to the searches |
| `dir-workers` | 4 | connections per search that look up parent dirs in parallel |
| `timeout` | 5m | per search, including the wait for a slot; also sets `statement_timeout` |
| `limit` | 500 | rows shown per search |
| `export-limit` | 1000000 | rows written per export at most |
| `export-timeout` | 30m | per export, including the wait for a slot |
| `reload-interval` | 1m | how often the TLS and allowed-subjects files are checked for changes |

The config file itself is read once at startup; changing it needs a restart. The certificate files and the subject list are reloaded live (below).

The service is HTTPS only, in both auth modes. Settings that the chosen mode doesn't use (for example `tls-ca` with `auth = basic`) are an error at startup.

### Client certificates

This is `auth = mtls`, the default.

- The TLS handshake asks for a client certificate but does not require one.
- A certificate that doesn't chain to `--tls-ca` fails the handshake.
- A missing certificate gets a 401 page, with a link to `cert-help-url` if it is set.
- A certificate whose subject DN is not on the list gets a 403 page that shows the subject, with a link to `access-help-url` if it is set. The user can paste that subject into the access request.

The allow list matches the **whole subject DN**, not just the CN, in RFC 4514 form, e.g. `UID=1000,CN=alice,O=Example Org`. This is the order `openssl x509 -noout -subject -nameopt RFC2253` prints. Entries are normalized, so attribute-name case and spaces around `,` and `=` don't matter; values are case-sensitive. A line that isn't a DN (such as a bare CN) is an error at startup, and on reload it keeps the previous list.

The server reloads the certificate files and the subject list when their mtime changes, and at once on `SIGHUP` (`systemctl reload cephfs-fe`). A reload that fails keeps the previous state. Every search and export is logged with the client subject DN.

### Basic auth without a PKI

For setups without a CA for client certificates, `auth = basic` asks the browser for a user and password and checks them against `htpasswd-file`. No client certificate is requested. Only bcrypt entries are accepted, so a file written with htpasswd's MD5 default is an error at startup:

```bash
htpasswd -B -C 10 -c /etc/cephfs-fe/htpasswd alice    # -c creates the file; leave it out to add users
chown cephfs-fe: /etc/cephfs-fe/htpasswd && chmod 600 /etc/cephfs-fe/htpasswd
```

```
auth = basic
htpasswd-file = /etc/cephfs-fe/htpasswd
tls-cert = /etc/cephfs-fe/tls/cert.pem
tls-key = /etc/cephfs-fe/tls/key.pem
```

The server certificate can come from any CA the browsers trust, e.g. Let's Encrypt. The htpasswd file is reloaded like the subject list, when its mtime changes or on `SIGHUP`. A successful login is remembered in memory until the next reload, so the page's API calls don't each pay for a bcrypt check. Unknown users take as long to reject as wrong passwords. There is no rate limit on failed logins beyond the cost of bcrypt, so keep `listen` on a management network. Searches and exports are logged with `client="user=<name>"`.

The database role needs `USAGE` on the index schemas and `SELECT` on their `meta`, `entries` and `dirs` tables. It also needs `SELECT, INSERT, UPDATE` on `public.cephfs_fe_stats_cache`, which must be created before the service starts:

```sql
CREATE TABLE IF NOT EXISTS public.cephfs_fe_stats_cache (
  fs          text PRIMARY KEY,
  started_at  text NOT NULL,
  stats_ver   int  NOT NULL,
  computed_at timestamptz NOT NULL,
  stats       jsonb NOT NULL
);
GRANT SELECT, INSERT, UPDATE ON public.cephfs_fe_stats_cache TO cephfs_fe;
```

Without this table the stats worker falls back to the old in-memory-only behaviour (a warning is logged at startup) and recomputes on every restart.

`cephfs-fe.service` is a sample systemd unit.

## Relationship to cephfs-index

`internal/pgsearch` is a copy of cephfs-index's read path: `internal/pgindex/search.go`, the pool and name helpers from `internal/pgindex/writer.go`, and the pattern helpers from `internal/index/search.go`. Go does not allow importing another module's `internal/` packages, and a copy keeps this binary free of cgo (sqlite). The copy adds an mtime filter, a row limit that also bounds path resolution, per-call stats (so a `DB` can be shared), and a statement timeout.

cephfs-indexd owns the schema: `<fs>.meta`, `<fs>.entries(parent, name, ino, type, uid, size, mtime)`, `<fs>.dirs(ino, parent, name)`. When it changes there, update `internal/pgsearch` here.

## Building

```
make          # vet, test, build → bin/cephfs-fe
```

Releases are built with [go-build-release](https://github.com/xorpaul/go-build-release), a git submodule, which sets `main.buildversion` and `main.buildtime`. `cephfs-fe --version` prints them, and the page header shows the version.
