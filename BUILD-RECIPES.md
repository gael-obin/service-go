# Image recipe context

## Executable packaging

`Builder.Package` uses the service's `with-workspace` setting for both its
executable and its SBOM. Development builds retain Go's selected `go.work`;
standalone builds explicitly disable it. Source-package lookup and toolchain
selection use the same setting. Cross compilation mounts the local modules
reported by `go list -m -json all`, including filesystem replacements, at their
original paths so the container resolves the same graph as the native build.

The packaging tests execute a binary whose result changes with workspace
selection and check the corresponding SBOM dependency. The opt-in real-CGO suite
also compiles a workspace-only sibling dependency through the cross toolchain.

## Image build context

The Go agent assembles a self-contained build tree in the caller's
`BuildRequest.output_directory`. It carries repository-local Go replacements
under `code/_replace/` and rewrites only the emitted go.mod. The original service
tree remains unchanged.

The recipe explicitly declares `context_root=OUTPUT` and `context="."`. The
executor must build that emitted tree, not the original service: the latter still
contains sibling-relative replacements that cannot resolve in an image.

This declaration requires `codefly.dev/docker-build-recipe/v4` support in the
executor. An older host rejects the plan before building. Upgrade the CLI before
adopting an agent release containing this change; do not edit the source go.mod,
copy siblings by hand, or change the Dockerfile to compensate.

Core owns the protocol and integrity check; CLI owns Docker execution. Producer
tests check the declared root over gRPC and compile the relocated emitted sources
with the real Go toolchain. End-to-end qualification must also run the CLI image
build, because compiling emitted sources alone cannot prove the host selects them.

## Base images and OS packages

A recipe is reproducible when its declared inputs are, not when its output
digest happens to repeat. Two inputs of the emitted Dockerfile used to float:
the base images resolved by mutable tag, and `apk add` resolved against
whatever the Alpine index served that minute.

Both bases are now pinned by tag **and** digest — the digest names the bytes,
the tag keeps the image legible and keeps Dependabot able to move the pair. The
digests are OCI image-index digests, so `linux/amd64` and `linux/arm64`, the
two platforms `pkg/builder` declares, both still resolve.

**Refreshing a digest is Dependabot's job, not a person's.** A hand-edited
constant rots, so the pin is deliberately placed where the automation this
repository already runs can reach it: `.github/dependabot.yml` points its
`docker` ecosystem at `/templates/builder`, Dependabot's Docker updater matches
files by `/dockerfile|containerfile/i` and so fetches `Dockerfile.tmpl`, and its
group carries no `update-types` filter — that filter only recognises semver
bumps and would drop a digest update into an ungrouped pull request of its own.
The weekly bump rewrites tag and digest together and `combine-deps.yml` folds it
in with everything else. `.github/scripts/refresh-base-digests.sh` moves the pin
by hand when you cannot wait a week (`--check` reports drift without writing);
it is deliberately not wired into CI, because a job that goes red the moment
upstream publishes teaches people to ignore CI.

The OS packages split into a part that is fixed and a part that is only
recorded:

- **The runtime stage installs nothing.** `alpine` already ships
  `ca-certificates-bundle`, which owns `/etc/ssl/certs/ca-certificates.crt` —
  the first file `crypto/x509` reads on Linux — so a static binary verifies TLS
  without the full `ca-certificates` package, whose `update-ca-certificates` and
  `c_rehash` nothing here runs. With that install gone the shipped image's
  package set follows from the base digest alone.
- **The builder stage installs `git`**, which the Go base does not carry and
  GOPRIVATE module fetches need. Pinning its version is not an option: Alpine's
  index serves only the current `-rN` of a package, so `git=2.54.0-r0` stops
  resolving the day the branch ships a patch, and every rebuild of an
  already-published recipe fails. Reproducibility bought that way costs the
  ability to rebuild at all. The resolved set is recorded instead.

The records travel with the artifact, in `/etc/codefly/build-inputs/`:
`apk-builder.txt` and `apk-runtime.txt` (each `apk info -v | sort`) and
`go-build-info.txt` (`go version -m`, which also catches the one input the base
digest cannot pin — `GOTOOLCHAIN` defaults to `auto`, so a service whose go.mod
requires a newer Go makes the toolchain download one at build time).
`apk-runtime.txt` is redundant with the digest by construction, which is the
point: two builds that disagree on it are evidence the pin stopped holding.
