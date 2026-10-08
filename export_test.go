package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xorpaul/cephfs-fe/internal/pgsearch"
)

func TestExportWriters(t *testing.T) {
	good := newRow("vol1", pgsearch.Match{Type: 'f', UID: 33, Size: 10, Mtime: 1700000000, Ctime: 1700000100, Path: "/data/a,b \"c\".json"})
	link := newRow("vol1", pgsearch.Match{Type: 'h', Path: "/data/link"})
	raw := "/data/bad\xffname"
	bad := newRow("vol2", pgsearch.Match{Type: 'd', UID: 0, Path: raw})
	if !bad.Lossy || bad.Path == raw {
		t.Fatalf("lossy row: %+v", bad)
	}
	bad.PathB64 = base64.StdEncoding.EncodeToString([]byte(raw))

	// Same layering as export(): csv.Writer on a 64 KiB bufio.Writer on the
	// response, so the trailer must get through both.
	var buf bytes.Buffer
	cw := newCSVWriter(bufio.NewWriterSize(&buf, 64<<10))
	cw.header()
	for _, r := range []row{good, link, bad} {
		if err := cw.write(r); err != nil {
			t.Fatal(err)
		}
	}
	cw.trailer(exportSummary{Rows: 3, Truncated: true})
	cw.flush()
	// The trailer line has one field, so read with variable field counts.
	r := csv.NewReader(strings.NewReader(buf.String()))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 5 || strings.Join(recs[0], ",") != "fs,path,type,uid,size,mtime,mtime_utc,ctime,ctime_utc" {
		t.Fatalf("csv: %q", recs)
	}
	if recs[1][1] != good.Path || recs[1][2] != "file" || recs[1][3] != "33" || recs[1][6] != "2023-11-14T22:13:20Z" || recs[1][8] != "2023-11-14T22:15:00Z" {
		t.Errorf("csv row: %q", recs[1])
	}
	if recs[2][3] != "" || recs[2][2] != "hardlink" {
		t.Errorf("hardlink uid should be empty: %q", recs[2])
	}
	if recs[3][1] != raw {
		t.Errorf("csv must keep raw path bytes: %q", recs[3][1])
	}
	if !strings.HasPrefix(recs[4][0], "# TRUNCATED at 3 rows") {
		t.Errorf("csv trailer: %q", recs[4])
	}

	// Clean end: no trailer line in CSV.
	buf.Reset()
	cw = newCSVWriter(bufio.NewWriter(&buf))
	cw.header()
	cw.write(good)
	cw.trailer(exportSummary{Rows: 1})
	cw.flush()
	if strings.Contains(buf.String(), "#") {
		t.Errorf("clean csv has a trailer: %q", buf.String())
	}

	buf.Reset()
	bw := bufio.NewWriter(&buf)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	jw := &jsonlWriter{bw, enc}
	for _, r := range []row{good, bad} {
		jw.write(r)
	}
	jw.trailer(exportSummary{Rows: 2, Skipped: []string{"vol3"}})
	jw.flush()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("jsonl: %q", lines)
	}
	var got row
	json.Unmarshal([]byte(lines[1]), &got)
	if got.Type != "dir" || !got.Lossy || got.PathB64 == "" || got.UID == nil {
		t.Errorf("jsonl lossy row: %s", lines[1])
	}
	if lines[2] != `{"summary":{"rows":2,"skipped":["vol3"]}}` {
		t.Errorf("jsonl trailer: %s", lines[2])
	}
}
