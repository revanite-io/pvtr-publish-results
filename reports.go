package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gemaraproj/go-gemara"
	"github.com/gemaraproj/go-gemara/gemaraconv"
	"github.com/goccy/go-yaml"
)

// writeReports emits the derived views of the logs this run publishes: a
// SARIF document per log under dir, and a markdown table appended to
// summaryPath ($GITHUB_STEP_SUMMARY). Both are built from the same stamped
// log that is published, so a report can never describe something other than
// what landed.
//
// Neither is part of the publication, and neither can fail it. Publishing
// passes the log through as ordered YAML on whatever go-gemara the plugin was
// built with; only these views need it decoded into this publisher's structs,
// which is the one thing here that can fail on a version this old. Every
// failure is a warning.
func writeReports(w io.Writer, dir, summaryPath string, logs []stamped) {
	if dir == "" && summaryPath == "" {
		return
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			warn(w, "cannot create the report directory %s: %v", dir, err)
			dir = ""
		}
	}
	for _, s := range logs {
		log, err := decodeLog(s)
		if err != nil {
			warn(w, "no reports for %s: %v", s.id, err)
			continue
		}
		if dir != "" {
			sarif, err := gemaraconv.ToSARIF(*log)
			if err != nil {
				warn(w, "no SARIF for %s: %v", s.id, err)
			} else {
				path := filepath.Join(dir, s.id+".sarif")
				if err := os.WriteFile(path, sarif, 0o644); err != nil {
					warn(w, "cannot write %s: %v", path, err)
				} else {
					_, _ = fmt.Fprintf(w, "Wrote %s\n", path)
				}
			}
		}
		if summaryPath != "" {
			if err := appendFile(summaryPath, summaryMarkdown(s, log)); err != nil {
				warn(w, "cannot write the job summary: %v", err)
			}
		}
	}
}

// decodeLog reads the stamped log into this publisher's gemara structs. The
// published bytes are unaffected: this is a second, throwaway view.
func decodeLog(s stamped) (*gemara.EvaluationLog, error) {
	raw, err := yaml.Marshal(s.log)
	if err != nil {
		return nil, fmt.Errorf("re-encoding the log: %w", err)
	}
	var log gemara.EvaluationLog
	if err := yaml.Unmarshal(raw, &log); err != nil {
		return nil, fmt.Errorf("this publisher's go-gemara cannot decode the log: %w", err)
	}
	return &log, nil
}

// summaryMarkdown renders one log as a GitHub job-summary section: the
// coordinate that was published, the verdict, the counts, and a row per
// evaluation.
func summaryMarkdown(s stamped, log *gemara.EvaluationLog) string {
	counts := map[gemara.Result]int{}
	var b strings.Builder
	var rows strings.Builder
	for _, ev := range log.Evaluations {
		if ev == nil {
			continue
		}
		counts[ev.Result]++
		fmt.Fprintf(&rows, "| %s | %s | %s |\n",
			icon(ev.Result), cell(ev.Control.EntryId), cell(firstNonEmpty(ev.Message, ev.Name)))
	}
	fmt.Fprintf(&b, "## %s\n\n", s.repository)
	fmt.Fprintf(&b, "`%s` — **%s**\n\n", s.tag, log.Result.String())
	b.WriteString("| Passed | Failed | Needs review | Not applicable | Not run |\n")
	b.WriteString("|:------:|:------:|:------------:|:--------------:|:-------:|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %d | %d |\n\n",
		counts[gemara.Passed], counts[gemara.Failed], counts[gemara.NeedsReview],
		counts[gemara.NotApplicable], counts[gemara.NotRun])
	if rows.Len() > 0 {
		b.WriteString("| Result | Requirement | Finding |\n|:------:|---|---|\n")
		b.WriteString(rows.String())
	}
	b.WriteString("\n")
	return b.String()
}

func icon(r gemara.Result) string {
	switch r {
	case gemara.Passed:
		return "✅"
	case gemara.Failed:
		return "❌"
	case gemara.NeedsReview:
		return "⚠️"
	case gemara.NotApplicable:
		return "➖"
	default:
		return "⬜"
	}
}

// cell makes one field safe to sit in a markdown table row: no pipes, no
// newlines. Plugin text is arbitrary.
func cell(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.WriteString(f, s)
	return err
}

func warn(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "::warning::"+format+"\n", args...)
}
