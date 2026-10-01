package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/xorpaul/cephfs-fe/internal/pgsearch"
)

var errExportLimit = errors.New("export limit reached")

var typeNames = map[string]string{"f": "file", "d": "dir", "l": "symlink", "h": "hardlink"}

// rowWriter writes export rows in one format.
type rowWriter interface {
	header() error
	write(row) error
	// trailer notes an abnormal end (error or row cap); msg is "" on success.
	trailer(summary exportSummary) error
	flush() error
}

type exportSummary struct {
	Rows      int      `json:"rows"`
	Truncated bool     `json:"truncated,omitempty"` // stopped at export-limit
	Skipped   []string `json:"skipped,omitempty"`   // volumes whose index could not be opened
	Error     string   `json:"error,omitempty"`
}

// csvWriter writes paths as raw bytes, so a path that is not valid UTF-8 is
// still exact. An abnormal end is noted in a final "# " line.
type csvWriter struct {
	bw *bufio.Writer
	w  *csv.Writer
}

func newCSVWriter(bw *bufio.Writer) *csvWriter { return &csvWriter{bw, csv.NewWriter(bw)} }

func (c *csvWriter) header() error {
	return c.w.Write([]string{"fs", "path", "type", "uid", "size", "mtime", "mtime_utc"})
}

func (c *csvWriter) write(r row) error {
	uid := ""
	if r.UID != nil {
		uid = strconv.FormatUint(uint64(*r.UID), 10)
	}
	path := r.Path
	if r.PathB64 != "" {
		b, _ := base64.StdEncoding.DecodeString(r.PathB64)
		path = string(b)
	}
	return c.w.Write([]string{r.FS, path, typeNames[r.Type], uid, strconv.FormatInt(r.Size, 10),
		strconv.FormatInt(r.Mtime, 10), time.Unix(r.Mtime, 0).UTC().Format(time.RFC3339)})
}

func (c *csvWriter) trailer(s exportSummary) error {
	var msg string
	switch {
	case s.Error != "":
		msg = "ERROR: " + s.Error
	case s.Truncated:
		msg = fmt.Sprintf("TRUNCATED at %d rows (export-limit)", s.Rows)
	}
	if len(s.Skipped) > 0 {
		msg += fmt.Sprintf(" skipped volumes: %v", s.Skipped)
	}
	if msg == "" {
		return nil
	}
	c.w.Flush()
	return c.w.Write([]string{"# " + msg})
}

func (c *csvWriter) flush() error {
	c.w.Flush()
	if err := c.w.Error(); err != nil {
		return err
	}
	return c.bw.Flush()
}

// jsonlWriter writes one JSON object per line and always ends with a
// {"summary": …} line, so a consumer can tell a complete export from a
// cut-off one.
type jsonlWriter struct {
	bw  *bufio.Writer
	enc *json.Encoder
}

func (j *jsonlWriter) header() error { return nil }
func (j *jsonlWriter) write(r row) error {
	r.Type = typeNames[r.Type]
	return j.enc.Encode(r)
}
func (j *jsonlWriter) trailer(s exportSummary) error {
	return j.enc.Encode(map[string]exportSummary{"summary": s})
}
func (j *jsonlWriter) flush() error { return j.bw.Flush() }

var slugRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// export streams every match (up to --export-limit) as CSV or JSON lines.
func (s *server) export(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	sr, err := parseSearch(r.URL.Query(), 0)
	if err != nil {
		writeError(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "jsonl" {
		writeError(w, badRequest("bad format %q: want csv or jsonl", format))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.exportTimeout)
	defer cancel()
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(s.exportTimeout + 30*time.Second)); err != nil {
		// The server-wide WriteTimeout would then cut long exports.
		log.Printf("export: cannot extend write deadline: %v", err)
	}

	names, release, err := s.acquire(ctx, s.exportSem, sr)
	defer release()
	if err != nil {
		writeError(w, err)
		return
	}

	bw := bufio.NewWriterSize(w, 64<<10)
	var rw rowWriter
	ctype := "text/csv; charset=utf-8"
	if format == "csv" {
		rw = newCSVWriter(bw)
	} else {
		ctype = "application/x-ndjson"
		enc := json.NewEncoder(bw)
		enc.SetEscapeHTML(false)
		rw = &jsonlWriter{bw, enc}
	}
	slug := slugRE.ReplaceAllString(sr.pattern, "_")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="cephfs-fe_%s_%s.%s"`, slug, time.Now().Format("20060102-150405"), format))

	var sum exportSummary
	err = s.exportRows(ctx, sr, names, rw, &sum, func() error {
		if err := bw.Flush(); err != nil {
			return err
		}
		return rc.Flush()
	})
	if errors.Is(err, errExportLimit) {
		sum.Truncated, err = true, nil
	}
	if err != nil {
		sum.Error = err.Error()
	}
	if werr := rw.trailer(sum); werr == nil {
		_ = rw.flush()
	}
	log.Printf("export client=%q pattern=%q match=%s fs=%v type=%q uid=%d mtime>=%d format=%s: %d rows in %s, truncated=%v skipped=%v err=%v",
		clientID(r), sr.pattern, sr.match, sr.fs, string(sr.q.Type), sr.q.UID, sr.q.Mtime, format, sum.Rows, time.Since(t0).Round(time.Millisecond), sum.Truncated, sum.Skipped, err)
}

func (s *server) exportRows(ctx context.Context, sr *searchRequest, names []string, rw rowWriter, sum *exportSummary, flush func() error) error {
	if err := rw.header(); err != nil {
		return err
	}
	q := sr.q
	q.Limit = 0
	q.Timeout = s.exportTimeout
	q.DirWorkers = s.dirWorkers
	for _, name := range names {
		d, err := pgsearch.Open(ctx, s.pool, name)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			sum.Skipped = append(sum.Skipped, name)
			continue
		}
		_, err = d.Search(ctx, q, func(m pgsearch.Match) error {
			if sum.Rows >= s.exportLimit {
				return errExportLimit
			}
			r := newRow(name, m)
			if r.Lossy {
				r.PathB64 = base64.StdEncoding.EncodeToString([]byte(m.Path))
			}
			if err := rw.write(r); err != nil {
				return err
			}
			sum.Rows++
			// Search emits in batches; flush about once per batch so the
			// download progresses and memory stays flat.
			if sum.Rows%5000 == 0 {
				if err := rw.flush(); err != nil {
					return err
				}
				return flush()
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, errExportLimit) {
				return err
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := rw.flush(); err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
}
