package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// realLog is an unmodified github-repo v0.29.0 log, captured from
// pvtr-publish-results run 34298773583. Reports are the one thing here that
// decodes a plugin's output into structs, so the fixture has to be real:
// go-gemara v0.9.2 could not decode this file at all.
func realLog(t *testing.T) stamped {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "real-plugin-log.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var logs []yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(raw, &logs, yaml.UseOrderedMap()); err != nil {
		t.Fatal(err)
	}
	return stamped{
		id: "privateer_osps-baseline", repository: "eknight/privateer-osps-baseline",
		tag: "1.0.0-20260909T021201Z", log: logs[0],
	}
}

func TestWriteReports_SARIFAndSummaryFromARealLog(t *testing.T) {
	dir := t.TempDir()
	summary := filepath.Join(dir, "summary.md")
	var out strings.Builder

	writeReports(&out, filepath.Join(dir, "sarif"), summary, []stamped{realLog(t)})

	if strings.Contains(out.String(), "::warning::") {
		t.Fatalf("a real log must produce reports cleanly:\n%s", out.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "sarif", "privateer_osps-baseline.sarif"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				RuleID string `json:"ruleId"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "2.1.0" || len(doc.Runs) != 1 || len(doc.Runs[0].Results) == 0 {
		t.Fatalf("sarif = version %q, %d runs, %d results", doc.Version, len(doc.Runs), len(doc.Runs[0].Results))
	}
	// GitHub code scanning rejects a run with no results; the workflow's
	// upload is conditioned on there being some.
	if !strings.HasPrefix(doc.Runs[0].Results[0].RuleID, "OSPS-") {
		t.Errorf("ruleId = %q", doc.Runs[0].Results[0].RuleID)
	}

	md, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## eknight/privateer-osps-baseline",
		"`1.0.0-20260909T021201Z`",
		"| Passed | Failed | Needs review | Not applicable | Not run |",
		"| Result | Requirement | Finding |",
		"OSPS-AC-01",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary lacks %q:\n%s", want, string(md))
		}
	}
}

// A log this publisher's go-gemara cannot decode still publishes; only the
// derived views are skipped, with a warning.
func TestWriteReports_UndecodableLogWarnsAndDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	bad := stamped{id: "svc_cat", repository: "acme/svc-cat", tag: "1-2",
		log: yaml.MapSlice{{Key: "evaluations", Value: "not a list"}}}

	writeReports(&out, dir, filepath.Join(dir, "summary.md"), []stamped{bad})

	if !strings.Contains(out.String(), "::warning::no reports for svc_cat") {
		t.Errorf("output = %q", out.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files for an undecodable log", len(entries))
	}
}

func TestWriteReports_NothingRequestedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeReports(io.Discard, "", "", []stamped{realLog(t)})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %d files", len(entries))
	}
}

// Plugin text is arbitrary; a pipe or a newline in it must not break the row.
func TestCell(t *testing.T) {
	if got := cell(" a | b\nc  d "); got != `a \| b c d` {
		t.Errorf("cell = %q", got)
	}
}
