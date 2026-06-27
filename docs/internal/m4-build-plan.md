# M4 build plan

> **✅ Accepted — M4 declared done 2026-06-27.** The deliverability machinery is built and **proven end-to-end**: cutting `v0.1.0` produced a signed draft release (cosign keyless via GitHub OIDC → `checksums.txt.{sig,pem}`) and pushed the formula to `orchestra-hq/homebrew-tap` at `Formula/atlas.rb` via the dedicated "Atlas Release" App token; [`install.sh`](../../install.sh) was validated locally (detect → download → checksum-verify → install → run). What remains is a **non-engineering owner flip** — take the `atlas` repo public and publish the draft release — which lights up `brew install orchestra-hq/tap/atlas` and `curl -fsSL …/install.sh | sh` for the public (the release binaries become anonymously downloadable). A true anonymous clean-machine install was **not** independently verified pre-public; that is the accepted go-live check. Steady-state release ritual: `git tag vX.Y.Z && git push origin vX.Y.Z` → CI builds, signs, pushes the formula, and creates a draft → publish it.

The path from "installable enough to prove it" to **frictionless public install**: a newcomer gets a working `atlas` on their machine in one command. M4 refines the M4 milestone in [roadmap.md](../roadmap.md); the packaging/distribution decisions it builds on live in [ADR-0006](decisions/0006-packaging-and-deployment.md).

M4 is deliberately small. The artifacts already ship from **M0.5** ([ADR-0006](decisions/0006-packaging-and-deployment.md)): GoReleaser publishes static binaries (linux/darwin × amd64/arm64) as tar.gz archives with `checksums.txt` to GitHub Releases on `v*` tags, and the GHCR Docker images are published. M4 adds the two **polished acquisition channels** on top of that binary — a Homebrew tap and a one-line installer — plus the signing that lets both verify what they download. It does not change the artifact, the release pipeline's shape, or the deployment story (that is M5).

## Build-time technical decisions

Choices recorded here so they don't get re-litigated mid-build:

1. **Scope is the two acquisition channels, no vanity domain.** M4 ships (a) a **Homebrew tap** and (b) a **one-line `install.sh`**. The owned-domain installer (`get.atlas.dev | sh`) from the original roadmap bullet is **dropped** — the project owner's call (2026-06-26): not registering a custom domain. The installer is served from the repo / GitHub Releases directly (`curl -fsSL https://raw.githubusercontent.com/orchestra-hq/atlas/main/install.sh | sh`); a vanity URL can be bolted on later with no code change if that changes. Linux native packages (`.deb`/`.rpm` via GoReleaser nfpm) stay **optional / deferred** until there is demand — a hosted apt/yum repo is explicitly out.
2. **The Homebrew tap is a separate, public, reusable repo.** A tap must be a repo named `homebrew-<name>` (brew convention); it cannot be the code repo. It is named generically — **`orchestra-hq/homebrew-tap`** — so it is an **org-wide registry** that can hold many formulae (`brew install orchestra-hq/tap/atlas`, and any future tool through the same repo + same auth, set up once). It is **public** even while `atlas` is private, so `brew install` never requires code access. GoReleaser commits the generated formula there on each release (keeping per-release formula churn out of the code repo's history). The artifact is a **formula** (GoReleaser `brews:`), not a cask: a formula's `bin.install` of the prebuilt binary avoids the macOS Gatekeeper **quarantine** an un-notarized binary hits via a cask. GoReleaser has deprecated `brews:` in favor of casks, so the GoReleaser version is **pinned** (`2.16.0` in `release.yml` + `ci.yml`, bumped deliberately) so the stanza can't be dropped from under us; revisit casks if/when the binary is Apple-notarized.
3. **The formula push authenticates via a dedicated GitHub App, not a PAT.** GitHub Actions' built-in `GITHUB_TOKEN` is scoped to the running repo only, so it cannot push the formula to `homebrew-tap`. Rather than a long-lived personal token, a **dedicated "Atlas Release" GitHub App** (separate from the existing nightly-runner App, for clean blast-radius) with a single permission — **Contents: read & write** — installed on the `homebrew-tap` repo only. The release workflow mints a **~1-hour installation token** at run time via `actions/create-github-app-token` (the same action the nightly already uses) and passes it to GoReleaser as the tap's `repository.token`. The release's own GitHub-release creation keeps using the default `GITHUB_TOKEN`. Net: no standing secret with write access to anything beyond the tap, auto-expiring, not tied to a person.
4. **Releases are signed with cosign keyless.** GoReleaser's `signs` block signs the archives + checksums with **cosign in keyless mode** (GitHub OIDC — no signing key to generate, store, or rotate). The installer and the documented manual path verify the signature, not just the checksum, making ADR-0006's "signed release" literally true. `cosign` verification is documented; checksum-only remains a fallback for users without cosign.
5. **Public install go-live is gated on public artifacts → the repo going public.** Both channels need the release **binaries** to be anonymously downloadable. While `atlas` is private, the tarballs (and `raw.githubusercontent.com/.../install.sh`) are auth-gated, so a stranger's `brew install` / `curl | sh` would 404. Therefore M4 splits cleanly in two: **build + snapshot-validate the machinery now** (works for anyone with repo access), and **flip public when the `atlas` repo goes public** (the binaries become public for free; the already-public tap then serves everyone). No public artifact bucket is introduced — go-live aligns with repo-public rather than adding owner infra.

## Phases

| Phase | Deliverable                                                             | Exit criterion                                                                                                                                                                                                   |
| ----- | ----------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1     | `install.sh` + cosign keyless signing in the release pipeline           | `goreleaser release --snapshot` produces signed archives; `install.sh` installs the right OS/arch binary from a real release and verifies checksum + signature; non-interactive + `--version`/PATH UX documented |
| 2     | Homebrew tap via GoReleaser `brews:` + the dedicated release App wiring | `goreleaser release --snapshot` generates a valid `atlas.rb` formula into `dist/`; the release workflow is wired to mint the App token and push to `orchestra-hq/homebrew-tap` (exercised on the next real tag)  |
| 3     | Public go-live                                                          | with `atlas` public + the tap repo + App in place, a clean machine runs `brew install orchestra-hq/tap/atlas` **and** `curl -fsSL …/install.sh \| sh` and ends up serving a model — **M4 done**                  |

Exit criteria are cumulative. Phases 1 and 2 are fully buildable + verifiable **now** with no owner action (snapshot mode neither publishes nor needs a secret); phase 3 is the owner-gated flip.

## Phase notes

**Phase 1 — installer + signing.** A POSIX `sh` `install.sh` at the repo root: detect OS/arch, query the GitHub Releases API for the latest (or a pinned `ATLAS_VERSION`) tag, download the matching `atlas_<ver>_<os>_<arch>.tar.gz`, verify it against `checksums.txt` and the cosign signature, extract `atlas` to a PATH dir (`/usr/local/bin`, or `~/.local/bin` without sudo), and print a "you're ready" line. It must be non-interactive-friendly (env-driven: `ATLAS_VERSION`, `ATLAS_INSTALL_DIR`) and idempotent. Cosign keyless signing is added to `.goreleaser.yaml` (`signs:`) so the artifacts the script verifies are signed. Validated locally against a published release before wiring anything owner-gated. Also folds in the small install/upgrade UX: an `atlas --version` upgrade hint and documented verification.

**Phase 2 — Homebrew tap.** A `brews:` stanza in `.goreleaser.yaml` (formula name `atlas`, homepage, description, the `bin.install` + a smoke `test do atlas --version`), with `repository: orchestra-hq/homebrew-tap` and `repository.token` templated from the App-minted token env. The release workflow gains a `create-github-app-token` step (dedicated Release App id/key secrets) before GoReleaser, exposing the token to the tap push only. `goreleaser release --snapshot` renders the formula into `dist/` for inspection without pushing or any secret. The actual push + a real `brew install` are exercised on the next tag once the owner has created the repo/App.

**Phase 3 — public go-live.** When the `atlas` repo goes public the release binaries become anonymously downloadable, which is the one thing both channels were waiting on. Verify end to end on a clean machine (a fresh VM / container): `brew install orchestra-hq/tap/atlas` resolves the public tap → downloads the public binary → `atlas up` serves a model; and `curl -fsSL …/install.sh | sh` does the same. Reconcile the now-real install command into the docs that currently reference the dropped `get.atlas.dev` (`vision.md`, `deployment-aws.md`, `roadmap.md`).

## Acceptance — what "M4 done" means

M4 has no new conformance G-group (it is delivery, not API surface). "Done" is the phase-3 demo observed on a **clean machine**: a newcomer with neither Go nor the repo installs `atlas` in one command — via **both** `brew install orchestra-hq/tap/atlas` and `curl -fsSL <install.sh> | sh` — with the downloaded binary signature-verified, and reaches a served model. The machinery (phases 1–2) is proven continuously by the existing per-PR `goreleaser release --snapshot` dry-run (it already runs in CI), extended to cover the new `signs:`/`brews:` blocks.

## Who does what

**Owner (account/repo/decision steps only):**

1. Create the public repo **`orchestra-hq/homebrew-tap`** (empty; generic name, reused org-wide).
2. Create a dedicated **"Atlas Release" GitHub App** — one permission, **Contents: read & write** — generate a private key, and **install it on `homebrew-tap` only**. Add its id + private key as Actions secrets on `orchestra-hq/atlas` (e.g. `RELEASE_APP_ID`, `RELEASE_APP_PRIVATE_KEY`).
3. **Take the `atlas` repo public** — the trigger that makes both channels work for the world.
4. Cut/publish the next `v*` release tag (releases are draft → published manually) once the above are in place.

**Claude (everything in-repo, no secrets needed to build + validate):**

- `install.sh` + its local test; `signs:` (cosign keyless) and `brews:` in `.goreleaser.yaml`; the `create-github-app-token` + tap-token wiring in `release.yml`.
- Snapshot-validate the whole pipeline (`goreleaser release --snapshot`) so the formula, signatures, and archives are inspectable without publishing.
- Install/upgrade docs, `atlas --version` hint, README install section, and reconciling the dropped-domain references.
- `docs/m4-build-plan.md` (this doc).

## Out of scope for M4

- **Custom/owned domain** for the installer (`get.atlas.dev`) — dropped (decision 1); revisit only if a vanity URL is wanted later (doc-only change).
- **Hosted apt/yum repo** and unconditional `.deb`/`.rpm` — Linux native packages are optional/deferred (decision 1).
- **homebrew-core submission** (plain `brew install atlas`, no tap prefix) — a "when popular" PR to `homebrew/core` after the repo is public and notable; the self-hosted tap is the right move regardless and stays useful afterward.
- **Deploy recipes / packaging** (compose, systemd, k8s manifests, IaC) — that is [M5](../roadmap.md), not M4.
