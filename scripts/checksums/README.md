# Release checksum pins

One `<version>.txt` file per release, each a copy of the `checksums.txt`
goreleaser produces for that version's archives — committed *before* the
matching `vX.Y.Z` tag exists, not derived from it.

Why: goreleaser only produces checksums.txt *from* a build at the tagged
commit, after that commit already exists. The tagged commit's own tree
can therefore never contain its own release's checksums — nothing could
have written the file there first. `.github/workflows/release.yml`
verifies its build against the pin here before publishing anything, so a
non-reproducible or tampered build fails the workflow instead of reaching
a public GitHub Release. `ensure-binary.sh`'s eventual release-fetch step
verifies a downloaded archive against this same file, read from the
local checkout, not from anything fetched from the release host — see
its own comments for why that is not trust-on-first-use.

Pins are produced by `.github/workflows/pin-release-checksums.yml`,
triggered manually by the operator against the commit that is about to
be tagged, before it is tagged. See that workflow's own comments for the
full mechanism and why it opens a PR instead of pushing to the default
branch directly.
