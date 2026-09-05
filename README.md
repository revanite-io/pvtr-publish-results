# pvtr-publish-results

A reusable GitHub Actions workflow that runs one [pvtr](https://github.com/privateerproj/privateer)
plugin against one target and publishes the resulting Gemara EvaluationLogs to
[grc.store](https://grc.store) as signed OCI bundles.

The Sigstore certificate on every bundle names this workflow, not the caller's.
That is the whole point: the hub can grant a **verified** tier with one string
comparison against the trust root

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

| Input / secret     | Required | Meaning |
|--------------------|----------|---------|
| `target`           | yes      | What the results describe. The hub must trust the calling repository to publish under the namespace. |
| `license`          | yes      | SPDX expression. The hub rejects unlicensed bundles. |
| `config` (input)   | one of   | Path in the caller's checkout to a pvtr config with exactly one target. |
| `config` (secret)  | one of   | The same config inline, for plugins whose vars carry secrets. |
| `dry-run`          | no       | Run everything but the publish; bundles land in the `pvtr-bundles` artifact as OCI layouts. Nothing is signed or sent. |

The hub is not an input. It is part of what "verified" means, so it is fixed
to `https://hub.grc.store` in the workflow.

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
7. Exits with the run's exit code so the job reflects the evaluation.

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
require a pull request and a green `test` check on `main`, forbid force
pushes, and restrict `v*` tags to repository admins. Only GitHub-authored
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
