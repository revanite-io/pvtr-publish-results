// Command pvtr-publish-results publishes the Gemara EvaluationLogs of one
// pvtr run to grc.store as signed OCI bundles. It is the publish half of the
// reusable workflow in .github/workflows/publish.yml and is only meaningful
// there: the hub bearer is the job's OIDC token (trusted publishing), and the
// Sigstore certificate names the workflow, which is what the hub checks to
// rank a bundle as verified.
//
// The publish sequence itself (mint, pack, push, sign, provenance, sync) is
// grc-store-clientkit's bundle.Publish. This program stamps the logs,
// validates everything before the first network call, and binds each log to
// the plugin that produced it.
//
// One run, one target: the run's log becomes one bundle per catalog at the
// coordinate grc-store-protocol's slug package defines,
// <namespace>/<target-id>-<catalog-id>:<version>-<run timestamp>. Namespace
// is the target owner's org, never the plugin publisher's. metadata.id and
// metadata.version are stamped here so every plugin lands at the same
// coordinate shape; metadata.author is left exactly as the plugin wrote it,
// and must name the plugin that was actually installed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gemaraproj/go-gemara"
	"github.com/gemaraproj/grc-store-clientkit/bundle"
	"github.com/gemaraproj/grc-store-clientkit/hub"
	"github.com/gemaraproj/grc-store-clientkit/keyless"
	"github.com/gemaraproj/grc-store-clientkit/provenance"
	"github.com/goccy/go-yaml"
	"github.com/opencontainers/go-digest"
	"github.com/revanite-io/grc-store-protocol/slug"
	"github.com/revanite-io/grc-store-protocol/spdx"
	"oras.land/oras-go/v2/registry"
)

// pvtr run exit codes (privateer-sdk shared/exitcodes.go). Pass and fail both
// leave a real log, and failing results are the honest record of the target;
// any other code means the plugin did not complete.
const (
	testPass = 0
	testFail = 1
)

func main() {
	var (
		p                    params
		rawTarget, startedAt string
	)
	flag.StringVar(&p.hubURL, "hub-url", "https://hub.grc.store", "grc.store hub base URL")
	flag.StringVar(&p.writeDir, "write-directory", "evaluation_results", "pvtr run's write directory")
	flag.StringVar(&p.evaluator.Coordinate, "evaluator-coordinate", "", "grc.store <namespace>/<plugin-id> of the plugin that ran, read from pvtr's install manifest before the run")
	flag.StringVar(&p.evaluator.IndexDigest, "evaluator-digest", "", "sha256:<hex> index digest of that plugin, from the same manifest")
	flag.StringVar(&rawTarget, "target", "", "<namespace>/<id>@<version> the results describe")
	flag.StringVar(&p.license, "license", "", "SPDX expression the logs are published under")
	flag.StringVar(&startedAt, "started-at", "", "RFC 3339 time the pvtr run started; older output is refused")
	flag.IntVar(&p.runExitCode, "run-exit-code", 0, "exit code of pvtr run")
	flag.StringVar(&p.dryRun, "dry-run", "", "write the bundles to OCI layouts under this directory instead of publishing; no credentials, no signing, no network")
	flag.Parse()

	var err error
	if p.target, err = parseTarget(rawTarget); err == nil {
		p.startedOn, err = time.Parse(time.RFC3339, startedAt)
	}
	if err == nil {
		err = publish(context.Background(), os.Stdout, p)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// target is the `<namespace>/<id>@<version>` the results describe.
type target struct{ Namespace, ID, Version string }

// parseTarget requires all three parts. Namespace and id must already be hub
// slugs, so what the caller asked for is exactly where the hub files it, and
// the version must make a legal OCI tag.
func parseTarget(raw string) (target, error) {
	coord, version, ok := strings.Cut(strings.TrimSpace(raw), "@")
	ns, id, ok2 := strings.Cut(coord, "/")
	if !ok || !ok2 || ns == "" || id == "" || version == "" {
		return target{}, fmt.Errorf("want <namespace>/<id>@<version>, got %q", raw)
	}
	for _, part := range []struct{ what, v string }{{"namespace", ns}, {"id", id}} {
		if !slug.IsSlug(part.v) {
			return target{}, fmt.Errorf("target %s %q is not a hub slug; did you mean %q?", part.what, part.v, slug.Slugify(part.v))
		}
	}
	if err := (registry.Reference{Repository: ns + "/" + id}).ValidateRepository(); err != nil {
		return target{}, fmt.Errorf("target %q does not make a legal OCI repository: %w", coord, err)
	}
	// The timestamp is fixed-width and tag-legal, so a placeholder validates
	// the tag every real run composes.
	if err := (registry.Reference{Reference: slug.EvaluationLogVersion(version, time.Time{})}).ValidateReferenceAsTag(); err != nil {
		return target{}, fmt.Errorf("target version %q does not make a legal OCI tag: %w", version, err)
	}
	return target{Namespace: ns, ID: id, Version: version}, nil
}

// canonLicense canonicalizes the license; it is required because the hub
// rejects unlicensed bundles at sync.
func canonLicense(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("license is required (an SPDX expression, e.g. CC0-1.0)")
	}
	canon, err := spdx.Canonicalize(raw)
	if err != nil {
		return "", fmt.Errorf("license %q: %w", raw, err)
	}
	return canon, nil
}

// evaluator is the grc.store identity of the plugin that ran. The workflow
// reads it from pvtr's install manifest in the install step, before the
// plugin runs, and hands it over as job outputs: the plugin is third-party
// code and must not be able to attribute its log to another plugin by
// editing the manifest after the fact.
type evaluator struct {
	Coordinate  string
	IndexDigest string
}

// validate requires a verified grc.store install: a hub plugin coordinate
// and a sha256 index digest. A plugin from anywhere else has neither, and
// its results cannot carry the binding the verified tier exists for.
func (e evaluator) validate() error {
	if !slug.IsHubPluginCoordinate(e.Coordinate) {
		return fmt.Errorf("evaluator coordinate %q is not a grc.store <namespace>/<plugin-id>; only a verified install can be published", e.Coordinate)
	}
	if d, err := digest.Parse(e.IndexDigest); err != nil || d.Algorithm() != digest.SHA256 {
		return fmt.Errorf("evaluator digest %q is not a sha256 digest", e.IndexDigest)
	}
	return nil
}

// params drives publish.
type params struct {
	hubURL      string
	writeDir    string
	evaluator   evaluator
	target      target
	license     string // SPDX; canonicalized by publish
	startedOn   time.Time
	runExitCode int
	dryRun      string // OCI layout root; empty publishes for real

	// Test seams: nil selects the real clientkit sequence.
	publish      func(context.Context, bundle.Input, bundle.Target, *keyless.Signer) (*bundle.Published, error)
	resolveCreds func(context.Context, io.Writer, string) (creds, error)
}

type creds struct {
	bearer   string
	registry string
	signer   *keyless.Signer
}

type stamped struct {
	source     string // on-disk log file
	sourceHash string
	log        gemara.EvaluationLog
	repository string
	tag        string
}

// publish stamps and publishes every log of the run. Everything is read,
// stamped, and validated before any credential or network step, so a bad
// input fails closed with nothing pushed. Publishing is then fail-fast: the
// first error stops the loop; bundles already published stay (each is its
// own immutable coordinate).
func publish(ctx context.Context, w io.Writer, p params) error {
	if p.runExitCode != testPass && p.runExitCode != testFail {
		return fmt.Errorf("pvtr run exited %d: the plugin did not complete, so there is no result to publish", p.runExitCode)
	}
	license, err := canonLicense(p.license)
	if err != nil {
		return err
	}
	if err := p.evaluator.validate(); err != nil {
		return err
	}
	logs, err := loadLogs(p.writeDir, p.target, p.startedOn, p.evaluator)
	if err != nil {
		return err
	}

	// A dry run stops here: everything above has been validated, and the
	// same bundles are written to disk (one layout per repository, since
	// every log of a run shares a tag) without touching credentials.
	if p.dryRun != "" {
		return dryRun(ctx, w, p, logs, license)
	}

	resolve, pub := p.resolveCreds, p.publish
	if resolve == nil {
		resolve = resolveCreds
	}
	if pub == nil {
		pub = bundle.Publish
	}
	c, err := resolve(ctx, w, p.hubURL)
	if err != nil {
		return err
	}

	for _, s := range logs {
		in, err := input(p, s, license, c.registry)
		if err != nil {
			return err
		}
		t := bundle.Target{HubURL: p.hubURL, Repository: s.repository, Tag: s.tag, Bearer: c.bearer}
		_, _ = fmt.Fprintf(w, "Publishing %s:%s\n", s.repository, s.tag)
		res, err := pub(ctx, in, t, c.signer)
		if err != nil {
			if errors.Is(err, hub.ErrUnauthorized) || errors.Is(err, hub.ErrNoBearer) || errors.Is(err, bundle.ErrPushDenied) {
				return fmt.Errorf("publishing %s:%s: %w: the hub does not trust %s to publish under namespace %q", s.repository, s.tag, err, os.Getenv("GITHUB_REPOSITORY"), p.target.Namespace)
			}
			return fmt.Errorf("publishing %s:%s: %w", s.repository, s.tag, err)
		}
		_, _ = fmt.Fprintf(w, "Published %s:%s (signed=%t attested=%t)\n", s.repository, s.tag, res.Signed, res.Attested)
	}
	return nil
}

// input builds the bundle for one stamped log: the log body plus the SLSA
// predicate that binds it to the evaluator, target, and run.
func input(p params, s stamped, license, registry string) (bundle.Input, error) {
	body, err := yaml.Marshal(s.log)
	if err != nil {
		return bundle.Input{}, fmt.Errorf("encoding %s: %w", s.log.Metadata.Id, err)
	}
	filename := s.log.Metadata.Id + ".yaml"
	pred := provenance.Build(provenance.Input{
		Tool:           "pvtr-publish-results",
		StartedOn:      p.startedOn,
		ArtifactType:   gemara.EvaluationLogArtifact.String(),
		ArtifactID:     s.log.Metadata.Id,
		ArtifactName:   filename,
		ArtifactDigest: digest.FromBytes(body).String(),
		SourceFiles:    map[string]string{s.source: s.sourceHash},
		Registry:       registry,
		Repository:     s.repository,
		Tag:            s.tag,
		Evaluator: &provenance.Evaluator{
			Coordinate:    p.evaluator.Coordinate,
			IndexDigest:   p.evaluator.IndexDigest,
			TargetID:      p.target.ID,
			TargetVersion: p.target.Version,
			RunID:         p.startedOn.UTC().Format(slug.EvaluationLogVersionTimeLayout),
		},
	})
	return bundle.Input{
		Filename:      filename,
		ArtifactType:  gemara.EvaluationLogArtifact.String(),
		ArtifactID:    s.log.Metadata.Id,
		GemaraVersion: s.log.Metadata.GemaraVersion,
		Body:          body,
		License:       license,
		Provenance:    pred,
	}, nil
}

// dryRun writes each bundle to <root>/<repository> as an OCI image layout.
func dryRun(ctx context.Context, w io.Writer, p params, logs []stamped, license string) error {
	for _, s := range logs {
		in, err := input(p, s, license, "")
		if err != nil {
			return err
		}
		dir := filepath.Join(p.dryRun, s.repository)
		res, err := bundle.PushLocal(ctx, dir, s.tag, in)
		if err != nil {
			return fmt.Errorf("dry run %s:%s: %w", s.repository, s.tag, err)
		}
		_, _ = fmt.Fprintf(w, "Dry run: would publish %s:%s (manifest %s), written to %s\n", s.repository, s.tag, res.ManifestDigest, dir)
	}
	return nil
}

// loadLogs finds the run's single target output (<write-dir>/<service>/
// <service>.yaml, a YAML list of logs, one per catalog) and stamps each log
// with its publish identity. Output that predates the run is a leftover from
// an earlier one, not this run's result, and is refused. So is a log whose
// author is not the plugin that was installed: the hub accepts a log as
// verified only when metadata.author.id names the coordinate the provenance
// binds, so a mismatch would publish an unverified log from a verified run.
func loadLogs(writeDir string, t target, startedOn time.Time, ev evaluator) ([]stamped, error) {
	entries, err := os.ReadDir(writeDir)
	if err != nil {
		return nil, fmt.Errorf("reading results: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 1 {
		return nil, fmt.Errorf("%s holds %d target directories %v; this workflow publishes exactly one target per run", writeDir, len(dirs), dirs)
	}
	svc := dirs[0]
	source := filepath.Join(writeDir, svc, svc+".yaml")
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("reading gemara output: %w", err)
	}
	// Truncate: some filesystems keep whole-second mtimes.
	if info.ModTime().Before(startedOn.Truncate(time.Second)) {
		return nil, fmt.Errorf("%s was written %s, before this run started; the plugin left no new output", source, info.ModTime().UTC().Format(time.RFC3339))
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("reading gemara output: %w", err)
	}
	var logs []gemara.EvaluationLog
	if err := yaml.Unmarshal(raw, &logs); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", source, err)
	}
	if len(logs) == 0 {
		return nil, fmt.Errorf("%s holds no evaluation logs", source)
	}
	tag := slug.EvaluationLogVersion(t.Version, startedOn)
	sourceHash := digest.FromBytes(raw).String()
	out := make([]stamped, 0, len(logs))
	for _, log := range logs {
		// The plugin stamps metadata.id as <service>_<catalog>; the catalog
		// half is what this log is about.
		catalog := strings.TrimPrefix(log.Metadata.Id, svc+"_")
		if catalog == "" || catalog == log.Metadata.Id {
			return nil, fmt.Errorf("log id %q is not <%s>_<catalog-id>", log.Metadata.Id, svc)
		}
		if log.Metadata.Author.Id != ev.Coordinate {
			return nil, fmt.Errorf("log %q names author %q, but the installed plugin is %q; the hub would not rank it verified", log.Metadata.Id, log.Metadata.Author.Id, ev.Coordinate)
		}
		repository := slug.EvaluationLogRepository(t.Namespace, t.ID, catalog)
		if err := (registry.Reference{Repository: repository}).ValidateRepository(); err != nil {
			return nil, fmt.Errorf("catalog id %q does not make a legal OCI repository: %w", catalog, err)
		}
		log.Metadata.Id = t.ID + "_" + catalog
		log.Metadata.Version = tag
		if log.Target.Version == "" {
			log.Target.Version = t.Version
		}
		out = append(out, stamped{source: source, sourceHash: sourceHash, log: log, repository: repository, tag: tag})
	}
	return out, nil
}

// resolveCreds resolves the two independent identities a publish needs: the
// hub bearer, which in this workflow is only ever the job's OIDC token
// (trusted publishing, no stored secret), and the public-good Fulcio signing
// identity. Different issuers, different audiences, never conflated.
func resolveCreds(ctx context.Context, w io.Writer, hubURL string) (creds, error) {
	doc, err := hub.Discover(ctx, hubURL)
	if err != nil {
		return creds{}, err
	}
	host, _, err := hub.Registry(doc)
	if err != nil {
		return creds{}, err
	}
	bearer, inCI, err := hub.CIBearer(ctx, hubURL, doc)
	if err != nil {
		return creds{}, fmt.Errorf("acquiring hub bearer: %w", err)
	}
	if !inCI {
		return creds{}, errors.New("not running in GitHub Actions: the hub bearer is the job's OIDC token, and there is deliberately no other way in")
	}
	idTok, err := keyless.Identity(ctx, keyless.PublicGoodAudience, w)
	if err != nil {
		return creds{}, fmt.Errorf("acquiring signing identity (public-good Fulcio): %w", err)
	}
	return creds{bearer: bearer, registry: host, signer: &keyless.Signer{IDToken: idTok}}, nil
}
