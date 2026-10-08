# Changelog

## v1.2.0 (2026-10-08)

### Added
- **Precomputed paths for frequent names** — exact searches and exports for a name that cephfs-indexd v1.2.0 precomputed with `--pg-named-paths` (e.g. `mu-plugins`) read the volume's `named_paths` table instead of resolving each match's parents level by level, which costs one random read per directory level on a cold cache. Volumes and names without the table are searched as before. The per-volume details show `precomputed` in the Paths column, and the API reports `named_paths: true` per volume.

## v1.1.0 (2026-10-08)

### Added
- **ctime (inode change time)** — search results show a "Changed" column next to "Modified", the API returns `ctime` per row, and CSV exports gain `ctime` and `ctime_utc` columns (appended after `mtime_utc`). Needs indexes built by cephfs-index v1.1.0 or later; schemas from older builds have no `ctime` column and their searches fail until rebuilt.

## v1.0.0 (2026-10-01)

First public release.

### Added
- **Web frontend for cephfs-index** — HTTPS search page and JSON API over the CephFS name indexes that `cephfs-indexd build --pg-dsn` writes to PostgreSQL. A single static binary (pure Go, no cgo) with the page, script and stylesheet embedded.
- **Match modes** — *exact name* (default), *starts with*, *contains* and *regex* (Go RE2). Exact and prefix use the btree name index; *contains* and unanchored regexes run a parallel sequential scan pre-filtered on their required literals with `strpos`, and need a literal of at least 2 characters. The page warns before a full scan over more than 100M entries.
- **Filters and limits** — volume, type, mtime on or after a date and owner uid. At most `limit` rows (default 500) per search; exact, prefix and single-literal searches stop at `LIMIT limit+1` and report "500+" instead of counting every match. Concurrent searches are limited by `max-concurrent`, and `timeout` also sets the PostgreSQL `statement_timeout`.
- **Disk-friendly reads** — volumes built with cephfs-index layout 2 (covering name and dirs indexes on all-visible tables) are searched with index-only scans. Older layouts use bitmap heap scans so rows are read in physical order. Parent dirs are resolved per tree level on up to `dir-workers` parallel connections.
- **Export** — *Export all* streams every match (up to `export-limit`, default 1,000,000) as CSV or JSON lines from `/api/export`, with its own slot (`max-exports`) and `export-timeout`. Paths are resolved in batches with a dir cache, so memory stays flat. Paths that are not valid UTF-8 are exact in the export: raw bytes in CSV, `path_b64` in JSONL.
- **Volume dashboard** — per volume: size on disk, files, dirs and other entries (exact counts from the index meta when cephfs-indexd wrote them, estimates otherwise), last-indexed time and file-size statistics. Stats are computed in the background, recomputed only after a rebuild, and persisted in `public.cephfs_fe_stats_cache` so they survive restarts.
- **Index status** — `/api/volumes` reports each index's freshness, PARTIAL and journal-not-flushed warnings, and running builds (an `<fs>_new` schema); the page shows a banner while a build competes with searches for the disks. A volume whose index can't be opened is skipped and reported instead of failing the search.
- **mTLS client-certificate auth** — `VerifyClientCertIfGiven` plus an allow list of whole subject DNs in RFC 4514 form, normalized (attribute case, spaces, and `E=`/`emailAddress=`/OID spellings). Missing or unlisted certificates get 401/403 pages; the 403 page shows the subject for the access request. `cert-help-url` and `access-help-url` add links to both pages. The server certificate, client CA and allow list reload when their files change or on SIGHUP. Every search and export is logged with the client DN.
- **Basic auth without a PKI** — `auth = basic` checks a user and password against a bcrypt htpasswd file (`htpasswd -B`) instead of a client certificate, still over HTTPS. The file reloads like the allow list, successful logins are cached until the next reload, and unknown users take as long to reject as wrong passwords.
- **Config file** — `/etc/cephfs-fe/config` (or `--config`) with one `key = value` per line, the flag names as keys. Command-line flags override the file; unknown or repeated keys are errors. See `config.example`.
- **Sample systemd unit** — `cephfs-fe.service`, hardened, with `ExecReload` sending SIGHUP.
- **Version** — `--version`, a startup log line and the page header show the version and build time set by go-build-release.
