package resources_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run with HEYARR_CATALOG_AUDIT=1 go test ./internal/api/resources -run TestCatalogFetchAudit -v.
// The fixture models a 4,600-work library and exercises the real HTTP router, SQL,
// browse embeds and JSON writer. It excludes client rendering and network distance.
// Set HEYARR_CATALOG_PROFILE=/tmp/catalog.cpu to profile only the timed fetches.
// Set HEYARR_CATALOG_RUNS=1 for alternating runs across two worktrees.
func TestCatalogFetchAudit(t *testing.T) {
	if os.Getenv("HEYARR_CATALOG_AUDIT") != "1" {
		t.Skip("opt-in catalog fetch measurement")
	}
	h := newHarness(t)
	seedCatalogAudit(t, h)
	for _, statement := range []string{
		`SELECT id FROM works ORDER BY sort_title, id LIMIT 201`,
		`SELECT id FROM works ORDER BY created_at DESC, id DESC LIMIT 51`,
		`SELECT id FROM works WHERE content_type = 'music' ORDER BY sort_title, id LIMIT 201`,
	} {
		rows, err := h.db.Reader().Query("EXPLAIN QUERY PLAN " + statement)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			t.Logf("plan %s: %s", statement, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}

	type scenario struct {
		name  string
		paths []string
		want  int
		gzip  bool
	}
	works := "/api/v1/works?include=artwork,primary_asset&limit="
	scenarios := []scenario{
		{"full-200", []string{works + "200"}, 4600, false},
		{"full-500", []string{works + "500"}, 4600, false},
		{"full-500-gzip", []string{works + "500"}, 4600, true},
		{"listen-shelf-plain", []string{works + "500&content_type=music", works + "500&content_type=audiobook", works + "500&content_type=podcast"}, 3, false},
		{"listen-shelf", []string{works + "500&content_type=music", works + "500&content_type=audiobook", works + "500&content_type=podcast"}, 3, true},
		{"home-recent", []string{works + "50&sort=recent"}, 50, false},
		{"home-recent-gzip", []string{works + "50&sort=recent"}, 50, true},
	}
	type result struct {
		duration                       time.Duration
		requests, wireBytes, jsonBytes int
	}
	results := map[string][]result{}
	baseline := os.Getenv("HEYARR_CATALOG_BASELINE") == "1"
	runCount := 5
	if value := os.Getenv("HEYARR_CATALOG_RUNS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			t.Fatal("HEYARR_CATALOG_RUNS must be positive")
		}
		runCount = parsed
	}
	if profilePath := os.Getenv("HEYARR_CATALOG_PROFILE"); profilePath != "" {
		file, err := os.Create(profilePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(file); err != nil {
			t.Fatal(err)
		}
		defer func() { pprof.StopCPUProfile(); _ = file.Close() }()
	}
	// Rotate the first scenario so cache warmup and machine drift affect each path.
	for run := 0; run < runCount; run++ {
		for i := range scenarios {
			s := scenarios[(i+run)%len(scenarios)]
			if baseline && s.gzip {
				continue
			}
			start := time.Now()
			var got result
			rows := 0
			for _, basePath := range s.paths {
				path := basePath
				for {
					req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if s.gzip {
						req.Header.Set("Accept-Encoding", "gzip")
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("%s: status %d: %s", s.name, resp.StatusCode, body)
					}
					got.wireBytes += len(body)
					if s.gzip && resp.Header.Get("Content-Encoding") == "gzip" {
						reader, err := gzip.NewReader(bytes.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						body, err = io.ReadAll(reader)
						_ = reader.Close()
						if err != nil {
							t.Fatal(err)
						}
					}
					if s.gzip && resp.Header.Get("Content-Encoding") != "gzip" && len(body) >= 1024 {
						t.Fatalf("%s: large response was not gzipped", s.name)
					}
					var page struct {
						Items []struct {
							ID string `json:"id"`
						} `json:"items"`
						NextCursor string `json:"next_cursor"`
					}
					if err := json.Unmarshal(body, &page); err != nil {
						t.Fatal(err)
					}
					got.requests++
					got.jsonBytes += len(body)
					rows += len(page.Items)
					if page.NextCursor == "" || strings.HasPrefix(s.name, "home-recent") {
						break
					}
					path = basePath + "&cursor=" + page.NextCursor
				}
			}
			got.duration = time.Since(start)
			if rows != s.want {
				t.Fatalf("%s: got %d works, want %d", s.name, rows, s.want)
			}
			results[s.name] = append(results[s.name], got)
		}
	}
	for _, s := range scenarios {
		if baseline && s.gzip {
			continue
		}
		runs := results[s.name]
		times := make([]time.Duration, len(runs))
		for i, r := range runs {
			times[i] = r.duration
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s: median=%s range=%s..%s requests=%d wire_bytes=%d json_bytes=%d works=%d", s.name,
			times[len(times)/2], times[0], times[len(times)-1], runs[0].requests, runs[0].wireBytes, runs[0].jsonBytes, s.want)
	}
}

func seedCatalogAudit(t *testing.T, h *harness) {
	t.Helper()
	tx, err := h.db.Writer().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	work, err := tx.Prepare(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, NULL, '{}', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = work.Close() }()
	edition, err := tx.Prepare(`INSERT INTO editions (id, work_id, label, edition_type, attributes, created_at) VALUES (?, ?, '', '', '{}', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edition.Close() }()
	blob, err := tx.Prepare(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1048576, 'image/jpeg', ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blob.Close() }()
	asset, err := tx.Prepare(`INSERT INTO assets (id, edition_id, source_class, blob_hash, role, filename, mime, created_at, updated_at) VALUES (?, ?, 'managed', ?, ?, ?, 'image/jpeg', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = asset.Close() }()
	mustExec := func(stmt *sql.Stmt, args ...any) {
		t.Helper()
		if _, err := stmt.Exec(args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4600; i++ {
		kind := "movie"
		switch {
		case i >= 3000 && i < 4480:
			kind = "series"
		case i >= 4480 && i < 4580:
			kind = "document"
		case i >= 4580 && i < 4583:
			kind = "music"
		case i >= 4583:
			kind = "book"
		}
		id, ed := fmt.Sprintf("w%06d", i), fmt.Sprintf("e%06d", i)
		title := fmt.Sprintf("Catalog work %06d with a descriptive title", i)
		hash := fmt.Sprintf("blake3:%064x", i+1)
		mustExec(work, id, kind, id, title, title, seedTime, seedTime)
		mustExec(edition, ed, id, seedTime)
		mustExec(blob, hash, seedTime)
		mustExec(asset, fmt.Sprintf("p%06d", i), ed, hash, "primary", "primary.mp4", seedTime, seedTime)
		mustExec(asset, fmt.Sprintf("a%06d", i), ed, hash, "artwork", "cover.jpg", seedTime, seedTime)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
