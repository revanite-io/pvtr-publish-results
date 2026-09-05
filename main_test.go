package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gemaraproj/go-gemara"
	"github.com/gemaraproj/grc-store-clientkit/bundle"
	"github.com/gemaraproj/grc-store-clientkit/keyless"
	"github.com/goccy/go-yaml"
)

// writeLogs writes the gemara output a plugin would leave for service svc: a
// YAML list of logs with the plugin's <svc>_<catalog> ids.
func writeLogs(t *testing.T, dir, svc string, catalogs ...string) {
	t.Helper()
	var logs []gemara.EvaluationLog
	for _, c := range catalogs {
		logs = append(logs, gemara.EvaluationLog{
			Metadata: gemara.Metadata{Id: svc + "_" + c, Type: gemara.EvaluationLogArtifact, GemaraVersion: "1.0.0",
				Author: gemara.Actor{Id: "acme/scanner", Name: "scanner", Type: gemara.Software}},
			Target: gemara.Resource{Id: svc, Name: svc, Type: gemara.Software},
		})
	}
	raw, err := yaml.Marshal(logs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, svc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, svc, svc+".yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

type call struct {
	in bundle.Input
	t  bundle.Target
}

// stubbed returns params for a run that left the given catalogs, with no
// network: creds are canned and publish records each call.
func stubbed(t *testing.T, catalogs ...string) (params, *[]call) {
	t.Helper()
	dir := t.TempDir()
	if len(catalogs) > 0 {
		writeLogs(t, filepath.Join(dir, "results"), "svc", catalogs...)
	}
	calls := &[]call{}
	return params{
		hubURL: "https://hub.example", writeDir: filepath.Join(dir, "results"),
		evaluator: evaluator{Coordinate: "acme/scanner", IndexDigest: "sha256:" + strings.Repeat("ab", 32)},
		target:    target{Namespace: "acme", ID: "my-repo", Version: "1.2.3"},
		license:   "CC0-1.0",
		startedOn: time.Date(2026, 9, 4, 10, 15, 0, 0, time.UTC),
		resolveCreds: func(context.Context, io.Writer, string) (creds, error) {
			return creds{bearer: "b", registry: "reg.example", signer: &keyless.Signer{IDToken: "tok"}}, nil
		},
		publish: func(_ context.Context, in bundle.Input, t bundle.Target, _ *keyless.Signer) (*bundle.Published, error) {
			*calls = append(*calls, call{in, t})
			return &bundle.Published{Signed: true, Attested: true}, nil
		},
	}, calls
}

func TestParseTarget(t *testing.T) {
	good, err := parseTarget(" acme/my-repo.v2@1.2.3-rc1 ")
	if err != nil || good != (target{Namespace: "acme", ID: "my-repo.v2", Version: "1.2.3-rc1"}) {
		t.Fatalf("got %+v, %v", good, err)
	}
	if _, err := parseTarget("Acme/My_Repo@1.0"); err == nil || !strings.Contains(err.Error(), `did you mean "acme"`) {
		t.Errorf("a non-slug should suggest the slug, got %v", err)
	}
	for _, bad := range []string{"", "acme/my-repo", "acme/my-repo@", "my-repo@1.0", "acme/a/b@1.0", "/repo@1.0", "acme/@1.0", "acme/my repo@1.0", "acme/-lead@1.0", "acme/dou--ble@1.0", "acme/my_repo@1.0",
		// OCI path components: no edge or doubled dots, no dot next to a hyphen.
		"acme/repo.@1.0", "acme/.repo@1.0", "acme/a..b@1.0", "acme/repo.-x@1.0",
		// The version must make a legal OCI tag.
		"acme/repo@1.2.0+build.7", "acme/repo@1.0/0", "acme/repo@1.0@1", "acme/repo@" + strings.Repeat("9", 112)} {
		if _, err := parseTarget(bad); err == nil {
			t.Errorf("parseTarget(%q) should fail", bad)
		}
	}
}

func TestCanonLicense(t *testing.T) {
	if _, err := canonLicense(" "); err == nil || !strings.Contains(err.Error(), "license is required") {
		t.Errorf("empty license should say so, got %v", err)
	}
	if got, err := canonLicense("cc0-1.0"); err != nil || got != "CC0-1.0" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestPublish_SplitsListIntoOneBundlePerLog(t *testing.T) {
	p, calls := stubbed(t, "osps-baseline", "CCC_ObjStor")
	p.runExitCode = testFail // failing results are still the honest record
	if err := publish(context.Background(), io.Discard, p); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("want 2 publishes, got %d", len(*calls))
	}
	// Raw catalog ids survive in metadata.id; the repository is the hub's slug of it.
	wantRepo := []string{"acme/my-repo-osps-baseline", "acme/my-repo-ccc-objstor"}
	wantID := []string{"my-repo_osps-baseline", "my-repo_CCC_ObjStor"}
	for i, c := range *calls {
		if c.t.Repository != wantRepo[i] || c.t.Tag != "1.2.3-20260904T101500Z" || c.t.HubURL != p.hubURL || c.t.Bearer != "b" {
			t.Errorf("target[%d] = %+v", i, c.t)
		}
		var log gemara.EvaluationLog
		if err := yaml.Unmarshal(c.in.Body, &log); err != nil {
			t.Fatal(err)
		}
		if log.Metadata.Id != wantID[i] || log.Metadata.Version != c.t.Tag || log.Target.Version != "1.2.3" {
			t.Errorf("stamped log[%d] metadata = %+v target = %+v", i, log.Metadata, log.Target)
		}
		if log.Metadata.Author.Id != "acme/scanner" {
			t.Errorf("author must be left alone, got %+v", log.Metadata.Author)
		}
		if c.in.ArtifactID != wantID[i] || c.in.Filename != wantID[i]+".yaml" || c.in.ArtifactType != "EvaluationLog" || c.in.License != "CC0-1.0" || c.in.GemaraVersion != "1.0.0" {
			t.Errorf("input[%d] = %+v", i, c.in)
		}
		if c.in.Provenance == nil {
			t.Errorf("input[%d] has no provenance predicate", i)
		}
	}
}

// Every validation failure must happen before credentials are touched.
func TestPublish_FailsClosedBeforeNetwork(t *testing.T) {
	cases := map[string]func() params{
		"plugin did not complete": func() params { p, _ := stubbed(t, "cat"); p.runExitCode = 2; return p },
		"missing license":         func() params { p, _ := stubbed(t, "cat"); p.license = ""; return p },
		"missing output":          func() params { p, _ := stubbed(t); return p },
		"two target directories": func() params {
			p, _ := stubbed(t, "cat")
			writeLogs(t, p.writeDir, "other", "cat")
			return p
		},
		"catalog id not an OCI path":         func() params { p, _ := stubbed(t, "."); return p },
		"author is not the installed plugin": func() params { p, _ := stubbed(t, "cat"); p.evaluator.Coordinate = "acme/other"; return p },
		"log id not <svc>_<catalog>": func() params {
			p, _ := stubbed(t, "cat")
			f := filepath.Join(p.writeDir, "svc", "svc.yaml")
			raw, _ := os.ReadFile(f)
			_ = os.WriteFile(f, []byte(strings.ReplaceAll(string(raw), "svc_cat", "other_cat")), 0o644)
			return p
		},
		"output left over from an earlier run": func() params {
			p, _ := stubbed(t, "cat")
			old := p.startedOn.Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(p.writeDir, "svc", "svc.yaml"), old, old); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"no evaluator binding": func() params { p, _ := stubbed(t, "cat"); p.evaluator = evaluator{}; return p },
		"evaluator not a coordinate": func() params {
			p, _ := stubbed(t, "cat")
			p.evaluator.Coordinate = "https://github.com/acme/scanner"
			return p
		},
		"evaluator digest malformed": func() params { p, _ := stubbed(t, "cat"); p.evaluator.IndexDigest = "sha256:abc"; return p },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := mk()
			p.resolveCreds = func(context.Context, io.Writer, string) (creds, error) {
				t.Fatal("creds must not be resolved")
				return creds{}, nil
			}
			if err := publish(context.Background(), io.Discard, p); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// The log is a plugin's artifact on its own go-gemara version. Fields the
// publisher does not stamp, including ones no struct here knows, must pass
// through untouched, and go-gemara's func-typed assessment steps must not
// break decoding.
func TestPublish_PassesUnknownFieldsThrough(t *testing.T) {
	p, calls := stubbed(t)
	raw := `- metadata:
    id: svc_cat
    type: EvaluationLog
    gemara-version: v1.0.0
    author:
      id: acme/scanner
    future-field: kept
  result: Failed
  evaluations:
  - assessment-logs:
    - steps:
      - github.com/privateerproj/privateer-sdk/pluginkit.adaptTypedSteps[...].func1
  target:
    id: svc
`
	if err := os.MkdirAll(filepath.Join(p.writeDir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.writeDir, "svc", "svc.yaml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), io.Discard, p); err != nil {
		t.Fatal(err)
	}
	body := string((*calls)[0].in.Body)
	for _, want := range []string{"future-field: kept", "adaptTypedSteps[...].func1", "id: my-repo_cat", "version: 1.2.3-20260904T101500Z", "  version: 1.2.3"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if (*calls)[0].in.GemaraVersion != "v1.0.0" || (*calls)[0].in.ArtifactID != "my-repo_cat" {
		t.Errorf("input = %+v", (*calls)[0].in)
	}
}

func TestPublish_DryRunWritesLayoutsWithoutCredentials(t *testing.T) {
	p, calls := stubbed(t, "osps-baseline", "other")
	p.dryRun = t.TempDir()
	p.resolveCreds = func(context.Context, io.Writer, string) (creds, error) {
		t.Fatal("creds must not be resolved")
		return creds{}, nil
	}
	if err := publish(context.Background(), io.Discard, p); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("dry run must not publish, got %d calls", len(*calls))
	}
	for _, repo := range []string{"acme/my-repo-osps-baseline", "acme/my-repo-other"} {
		if _, err := os.Stat(filepath.Join(p.dryRun, repo, "index.json")); err != nil {
			t.Errorf("no OCI layout for %s: %v", repo, err)
		}
	}
}

func TestPublish_StopsAtFirstFailureNamingTheTrustGap(t *testing.T) {
	p, calls := stubbed(t, "a", "b")
	p.publish = func(_ context.Context, in bundle.Input, t bundle.Target, _ *keyless.Signer) (*bundle.Published, error) {
		*calls = append(*calls, call{in, t})
		return nil, bundle.ErrPushDenied
	}
	err := publish(context.Background(), io.Discard, p)
	if !errors.Is(err, bundle.ErrPushDenied) || !strings.Contains(err.Error(), `namespace "acme"`) {
		t.Errorf("want the clientkit sentinel wrapped with the namespace, got %v", err)
	}
	if len(*calls) != 1 {
		t.Errorf("publishing must stop at the first failure, got %d calls", len(*calls))
	}
}
