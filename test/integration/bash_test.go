//go:build integration

package integration

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pagombin/upwell/internal/logx"
	"github.com/pagombin/upwell/internal/orch"
	"github.com/pagombin/upwell/internal/pg"
)

// TestReadinessFailsClosed (bug bash 1): the readiness collectors' own
// queries fail (heartbeat latency, the apply watchdog's origin read, the
// schema comparison) while every cluster stays reachable. Each condition
// they feed must turn red, never keep its last good value, and recover when
// the queries work again.
func TestReadinessFailsClosed(t *testing.T) {
	var broken atomic.Bool
	restore := pg.SetFaultHook(func(sql string) error {
		if broken.Load() && (strings.Contains(sql, "max(written_at)") || strings.Contains(sql, "pg_replication_origin_progress($1, false)") || strings.Contains(sql, "pg_get_expr(c.relpartbound")) {
			return errInjected
		}
		return nil
	})
	defer restore()
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_rfc")
	id := streamingMigration(t, e, "Readiness fails closed", db)
	var rd struct {
		Ready      bool             `json:"ready"`
		Conditions []orch.Condition `json:"conditions"`
	}
	cond := func(key string) orch.Condition {
		for _, c := range rd.Conditions {
			if c.Key == key && c.Database == db {
				return c
			}
		}
		return orch.Condition{}
	}
	e.waitFor(5*time.Minute, "readiness to pass", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	broken.Store(true)
	e.waitFor(3*time.Minute, "lag, apply and schema to turn red", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return !cond("lag").OK && !cond("apply").OK && !cond("schema").OK
	})
	for _, k := range []string{"lag", "apply", "schema"} {
		t.Logf("%s: %s", k, cond(k).Detail)
	}
	if rd.Ready {
		t.Fatal("the readiness gate passes while its measurements fail")
	}
	if c := e.do("POST", "/api/v1/migrations/"+id+"/cutover", map[string]any{"confirm_writers_stopped": true}, nil); c != 409 {
		t.Fatalf("a cutover started on failed measurements: %d", c)
	}
	broken.Store(false)
	e.waitFor(4*time.Minute, "readiness to recover", func() bool {
		e.do("GET", "/api/v1/migrations/"+id+"/cutover/readiness", nil, &rd, 200)
		return rd.Ready
	})
	e.abortAndClean(id, "Readiness fails closed")
}

// TestPartitionedKeyedLeavesGO (bug bash 2): a partitioned parent without a
// key whose partitions have primary keys is not a blocker (PostgreSQL checks
// the partition); the application's UPDATE and DELETE through the parent
// work on the source while streaming and reach the target; GO.
func TestPartitionedKeyedLeavesGO(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_parts")
	seedDB(t, db, 500)
	exec1(t, srcConn, db, `
		CREATE TABLE measures(id int NOT NULL, region text NOT NULL, v int) PARTITION BY LIST (region);
		CREATE TABLE measures_eu PARTITION OF measures FOR VALUES IN ('eu'); ALTER TABLE measures_eu ADD PRIMARY KEY (id);
		CREATE TABLE measures_us PARTITION OF measures FOR VALUES IN ('us'); ALTER TABLE measures_us ADD PRIMARY KEY (id);
		INSERT INTO measures SELECT g, CASE WHEN g % 2 = 0 THEN 'eu' ELSE 'us' END, g FROM generate_series(1, 400) g;`)
	id := e.defineMigration("Partitions", []string{db}, nil)
	pv := e.preflight(id)
	for _, r := range pv.Results {
		if r.CheckID == "replica_identity" {
			t.Logf("replica_identity: %s %s", r.Level, r.Message)
			if r.Level != "ok" {
				t.Fatalf("a partitioned parent with keyed partitions is flagged: %s", r.Message)
			}
		}
	}
	e.do("POST", "/api/v1/migrations/"+id+"/start", map[string]any{"warnings_reviewed": true}, nil, 200)
	e.waitDBState(id, db, 3*time.Minute, orch.DInSync)
	exec1(t, srcConn, db, `UPDATE measures SET v = v + 1000 WHERE id <= 50; DELETE FROM measures WHERE id > 380`)
	time.Sleep(3 * time.Second)
	e.mustGO(id)
	for _, qy := range []string{"SELECT count(*)::text FROM measures", "SELECT sum(v)::text FROM measures"} {
		if s, d := q(t, srcConn, db, qy), q(t, dstConn, db, qy); s != d {
			t.Fatalf("%s: source %s, target %s", qy, s, d)
		}
	}
	e.cleanup(id, "Partitions")
}

// inflatePDF returns a PDF's text with every Flate stream decompressed, so a
// search cannot miss text inside a compressed page.
func inflatePDF(b []byte) string {
	var out strings.Builder
	out.Write(b)
	for i := 0; ; {
		j := bytes.Index(b[i:], []byte("stream"))
		if j < 0 {
			break
		}
		start := i + j + len("stream")
		for start < len(b) && (b[start] == '\r' || b[start] == '\n') {
			start++
		}
		end := bytes.Index(b[start:], []byte("endstream"))
		if end < 0 {
			break
		}
		if r, err := zlib.NewReader(bytes.NewReader(b[start : start+end])); err == nil {
			d, _ := io.ReadAll(r)
			out.Write(d)
		}
		i = start + end
	}
	return out.String()
}

// TestRedactionCanaries (bug bash 9): the clusters' real password, an API
// token and a bearer header planted in the logs appear nowhere a migration
// can be exported: the support bundle, /metrics, the HTML, Markdown and PDF
// reports, the logs, events and audit APIs.
func TestRedactionCanaries(t *testing.T) {
	e := newEnv(t, "", orch.TestHooks{})
	db := uniq("it_canary")
	id := streamingMigration(t, e, "Canary", db)
	var tm map[string]any
	e.do("POST", "/api/v1/api-tokens", map[string]any{"name": "canary", "scope": "metrics"}, &tm, 201)
	tok := tm["token"].(string)
	mid := e.migration(id).Migration.ID
	// Through the logger, as every log line is written (engine lines too).
	for _, msg := range []string{"dialing postgres://doadmin:" + password + "@127.0.0.1:55432/defaultdb", "Authorization: Bearer " + tok, "password=" + password, "a token pasted bare: " + tok} {
		e.log.Write(logx.Record{Level: "info", Component: "engine", Migration: mid, Msg: msg, Raw: msg})
	}
	e.mustGO(id)
	get := func(path, bearer string) []byte {
		req, _ := http.NewRequest("GET", e.Srv.URL+path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, b)
		}
		return b
	}
	outputs := map[string]string{
		"report.html": string(get("/api/v1/migrations/"+id+"/report?format=html", "")),
		"report.md":   string(get("/api/v1/migrations/"+id+"/report?format=md", "")),
		"report.pdf":  inflatePDF(get("/api/v1/migrations/"+id+"/report?format=pdf", "")),
		"metrics":     string(get("/metrics", tok)),
		"logs":        string(get("/api/v1/logs?migration="+mid+"&limit=5000", "")),
		"events":      string(get("/api/v1/migrations/"+id+"/events?limit=2000", "")),
		"audit":       string(get("/api/v1/audit?limit=2000", "")),
	}
	gz, err := gzip.NewReader(bytes.NewReader(get("/api/v1/migrations/"+id+"/support-bundle/download", "")))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		outputs["bundle:"+h.Name] = string(b)
	}
	if !strings.Contains(outputs["logs"], "upw_****") || !strings.Contains(outputs["logs"], "doadmin:****@") {
		t.Fatal("the planted log lines are not in the logs at all, so the search proves nothing")
	}
	if !strings.Contains(outputs["report.pdf"], "Canary") {
		t.Fatal("the PDF text could not be read, so the search proves nothing")
	}
	for name, out := range outputs {
		for label, canary := range map[string]string{"database password": password, "API token": tok} {
			if strings.Contains(out, canary) {
				t.Errorf("%s contains the %s", name, label)
			}
		}
	}
	t.Logf("searched %d outputs", len(outputs))
	e.cleanup(id, "Canary")
}
