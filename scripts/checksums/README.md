# Release checksum pins

One `<version>.txt` file per release, each a copy of the `checksums.txt`
that `.github/workflows/release.yml`'s `build` and `verify` jobs
independently agreed on for that version's archives — committed to the
default branch *after* the release is already published, not before.

Why after, not before: an earlier design tried to commit the pin before
the tag existed, so `release.yml` could verify a build against it prior
to publishing. That cannot work here — goreleaser's `-buildvcs=true`
(see `.goreleaser.yaml`) embeds the commit's own revision into the
binary, so a pin computed at commit X and then committed as a *different*
commit Y describes a build that no longer matches; committing the pin
always changes the very commit it is trying to describe. There is no
commit whose tree can contain its own from-scratch checksums.

What actually gives a checksum pin its trust instead: `release.yml` runs
`build`, then independently reruns the exact same tagged commit in
`verify` on a separate runner, and only proceeds to `publish` if the two
outputs are byte-identical. That is the real verification, and it
happens entirely in-workflow, before anything reaches a public GitHub
Release — no in-tree pin is needed for it. Once `publish` has succeeded,
a `pin-checksums` job commits the checksums both jobs already agreed on
as `scripts/checksums/<version>.txt`, via a normal PR (org policy: no
direct pushes to the default branch). This file exists for one consumer
only: `ensure-binary.sh`'s release-fetch step, which verifies a
downloaded archive against the local checkout's copy of this file — not
against anything fetched from the release host — which is why that is
not trust-on-first-use. See its own comments for the full mechanism.

A checkout that predates this PR merging, or an exact-tag checkout of a
version whose pin PR hasn't merged yet, has no file here for that
version. `ensure-binary.sh`'s eventual release-fetch step (SATA-13 PR B,
not yet implemented) is designed to treat that the same as "no matching
release asset" and fall back to building from source, and to document
that gap in `docs/plugin-install.md` when it lands.
