# pvtr-publish-results

A reusable GitHub Actions workflow that runs one [pvtr](https://github.com/privateerproj/privateer)
plugin against one target and publishes the resulting Gemara EvaluationLogs to
[grc.store](https://grc.store) as signed OCI bundles.

The Sigstore certificate on every bundle names this workflow, not the caller's.
That is the whole point: the hub can accept a log with one string comparison
against the trust root

```text
revanite-io/pvtr-publish-results/.github/workflows/publish.yml@refs/tags/v1
```

instead of linting the caller's workflow the way Scorecard does. A signature
minted on a developer's machine proves who published; a signature minted here
also proves that an unmodified, grc.store-verified plugin produced the log.

## Calling it

```yaml
jobs:
  results:
    permissions:
      contents: read
      id-token: write
    uses: revanite-io/pvtr-publish-results/.github/workflows/publish.yml@v1
    with:
      config: .pvtr/config.yml      # committed pvtr config naming exactly one target
      target: acme/my-repo@1.4.0    # <namespace>/<id>@<version>; namespace is the target owner's org
      license: CC0-1.0
```

Plugin vars are read from the config file only, so a plugin that needs a
secret gets its config inline instead:

```yaml
    with:
      target: acme/my-repo@1.4.0
      license: CC0-1.0
    secrets:
      config: |
        targets:
          my-repo:
            plugin: ossf/pvtr-github-repo-scanner
            vars:
              owner: acme
              repo: my-repo
              token: ${{ secrets.GITHUB_TOKEN }}
```

Reference the workflow by tag, not by commit SHA. A SHA-pinned call puts the SHA
in the certificate instead of `refs/tags/v1`, and the hub's identity check fails.
The hub is configured with exactly one accepted tag per environment: production
accepts `v1`, and `hub.preview.grc.store` accepts the pre-release tag `v1.0.0-rc`.
Callers publishing to preview use `@v1.0.0-rc` with
`hub: https://hub.preview.grc.store`; each environment rejects the other's tag.

| Input / secret     | Required | Meaning |
|--------------------|----------|---------|
| `target`           | yes      | What the results describe. The hub must trust the calling repository to publish under the namespace. |
| `license`          | yes      | SPDX expression. The hub rejects unlicensed bundles. |
| `config` (input)   | one of   | Path in the caller's checkout to a pvtr config with exactly one target. |
| `config` (secret)  | one of   | The same config inline, for plugins whose vars carry secrets. |
| `hub`              | no       | `https://hub.grc.store` (default) or `https://hub.preview.grc.store`. Any other value fails before pvtr runs. |
| `dry-run`          | no       | Run everything but the publish; bundles land in the `pvtr-bundles` artifact as OCI layouts. Nothing is signed or sent. |
| `upload-sarif`     | no       | Also send the results to the caller's code scanning alerts. Needs `security-events: write` on the calling job. Skipped on a dry run. |

The hub is not free-form. It is part of what "verified" means: the plugin is
installed from it and the result is published to it, so an arbitrary hub could
feed the run a plugin of its own choosing and collect a bundle signed by this
workflow. Only the two grc.store hubs are accepted.

## What the workflow does

Two jobs. The plugin is third-party code, so the job that runs it holds no
token that could mint this workflow's identity, and nothing it can write
reaches the publisher except the results themselves.

**`run`** (permissions: `contents: read` only)

1. Installs a pinned pvtr release, verified by checksum and by its GitHub
   artifact attestation.
2. `pvtr install --from-config` into a fresh directory: the plugin is pulled
   from grc.store and verified (signature, signer identity, digest chain)
   before it is written.
3. Records the installed plugin's coordinate and index digest as step
   outputs, before the plugin runs, so it cannot attribute its log to another
   plugin afterwards.
4. `pvtr run` as a throwaway user with no sudo, so the plugin cannot reach
   the runner process or the code of later steps. Output is forced to gemara
   through `PVTR_*` env, which outranks the caller's config file. The exit
   code is captured, not acted on.
5. Uploads the results directory as an artifact.

**`publish`** (permissions: `contents: read`, `id-token: write`), on a fresh runner

6. Downloads the results, checks out this repo at the commit of the workflow
   file (`job.workflow_sha`, refused if empty), and runs the publisher
   (`main.go`) with the evaluator binding from job outputs. Pass and fail
   both publish; abort, error, and usage failures publish nothing.
7. Writes the job summary and the SARIF (see [Reports](#reports)), then succeeds when the log is
   published. **The job's status reports publication, not the
   evaluation**: a failing baseline is an honest result and still exits 0. Only a failure to
   produce or land the log is red — a plugin that aborted (any exit code other than pass or
   fail), a rejected publish, a hub that refused the bundle. Read the verdict from the log on
   the hub, not from this job's colour.

## What the publisher does

All of this is validated before the first network call, so a bad input fails
with nothing pushed:

- One target per run: `<write-dir>` must hold exactly one service directory.
- Each log in the run becomes one bundle at the coordinate
  [grc-store-protocol](https://github.com/revanite-io/grc-store-protocol)'s
  `slug` package defines:
  `<namespace>/<target-id>-<catalog-id>:<version>-<UTC run timestamp>`.
- `metadata.id` becomes `<target-id>_<catalog-id>` and `metadata.version` the
  tag, stamped here so every plugin lands at the same coordinate shape. The
  log is handled as ordered YAML and never decoded into a Gemara struct, so a
  plugin's output passes through on whatever go-gemara it was built with. The
  binding data rides in the provenance referrer.
- `metadata.author` is left as the plugin wrote it, and must equal the
  coordinate of the plugin that was installed. The hub ranks a log verified
  only when its author names the coordinate the provenance binds, so a
  mismatch is refused rather than published unverified.
- The SLSA provenance carries an `evaluator` binding: the plugin's grc.store
  coordinate and released index digest, recorded before the plugin ran, the
  target, and the run id. Both must parse as a hub coordinate and a sha256
  digest.
- Output older than the run start is refused as a leftover.

The publish sequence itself (mint, pack, push, sign, provenance, sync) is
[grc-store-clientkit](https://github.com/gemaraproj/grc-store-clientkit)'s
`bundle.Publish`. Two independent tokens: the hub bearer is the job's OIDC
token (trusted publishing, no stored secret), the signing identity is a
separate OIDC token for public-good Fulcio.

## Reports

Every run writes two views of the same stamped log it publishes, so a report
can never describe something other than what landed:

- **A job summary** — the coordinate, the verdict, the counts, and a row per
  requirement — on the `publish` job, always.
- **SARIF 2.1.0**, one document per log, in the `pvtr-sarif` artifact, always.
  With `upload-sarif: true` a separate `report` job also sends it to the
  caller's code scanning alerts.

Both are derived, never load-bearing. Publishing passes the log through as
ordered YAML on whatever go-gemara the plugin was built with; only these views
decode it into this publisher's structs, and that is the one step here that a
version gap can break. When it does, the run says so with a `::warning::` and
publishes anyway.

Code scanning is opt-in because it needs `security-events: write`, which the
calling job must grant:

```yaml
jobs:
  publish:
    permissions:
      contents: read
      id-token: write
      security-events: write   # only for upload-sarif
    uses: revanite-io/pvtr-publish-results/.github/workflows/publish.yml@v1
    with:
      target: my-org/my-repo@1.0.0
      license: CC0-1.0
      upload-sarif: true
```

The permission is granted to a job of its own that runs after the publish, not
to the job holding the signing identity, and a caller that leaves the input
`false` never dispatches that job and never has to grant it. A SARIF carrying
no results is not uploaded: code scanning rejects it.

## What verified does not prove

It proves *this plugin ran unmodified on a GitHub-hosted runner*. It does not
prove *these results describe the target named*: a plugin evaluating a cloud
account through credentials can be pointed at a decoy by the caller's config.
That is a hub policy and namespace-ownership question, not a signing one.

## Not here yet

- Hub-side: accept a bundle as verified only when the certificate identity
  matches the trust root; cross-check the claimed evaluator digest against the
  coordinate's released digests; decide how tiers are displayed.
- An org admin must bind the *calling* repository to the target namespace as
  a CI publisher (hub ADR-0032). The hub reads the caller's repository from
  the OIDC token; this workflow's own repository needs no binding.

## Repository governance

The repo is the trust root, so its settings are part of the design. Rulesets
require a pull request with one approving review and a green `test` check on
`main`, forbid force pushes, and restrict `v*` tags to repository admins;
repository admins may bypass. Only GitHub-authored
actions may run, the default workflow token is read-only, fork pull requests
from outside collaborators need approval before their workflows run, and
secret scanning with push protection is on. Dependabot keeps the pinned
action SHAs and Go modules current. Report vulnerabilities privately through
the repository's Security tab.

## Self-test

`selftest.yml` calls this workflow against this repository with the
`openssf/github-repo` plugin in dry-run mode, on every push to `main` and on
demand. It rehearses everything except the hub push and the Sigstore signing:
the verified plugin install, the unprivileged run, the pre-run binding, the
artifact handoff, the pinned publisher checkout, and the cold build.

## Development

```sh
go test -race ./... && go vet ./... && test -z "$(gofmt -l .)"
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -ignore 'property "workflow_sha" is not defined'
```

Every commit carries a DCO `Signed-off-by` trailer (`git commit -s`).
