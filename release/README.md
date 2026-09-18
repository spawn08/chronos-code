# Reproducible Release Inputs

Chronos Code currently uses a sibling `../chronos` replacement for local development. Releases do not select whatever happens to be in that directory: `.github/workflows/release.yml` checks out the full commit in `chronos.version` into a clean sibling directory, verifies `HEAD`, and creates a temporary Go workspace containing exactly those two checkouts.

To advance the dependency:

1. Publish the required Chronos commit to `spawn08/chronos`.
2. Replace `chronos.version` with its full 40-character commit ID.
3. Run the release workflow from a clean checkout.
4. Review the generated SPDX SBOM, `SHA256SUMS`, provenance attestation, and Sigstore verification output.

The current value is the explicit sentinel `UNPUBLISHED_COMPATIBLE_REVISION`, so release CI fails before checkout with an actionable error. Local Chronos HEAD `62817f39b00bc8ee860ec8a0dbd37e5d6065dcfd` is not reachable from the upstream repository as of 2026-09-18 and also has required uncommitted request-scoped provider, same-session serialization, workspace-root, and atomic registry changes on top. The reachable `13d62dfed676ffab94414f399049857e7c44d465` revision lacks those APIs and is therefore incompatible. Do not weaken those runtime guarantees merely to make the release checkout pass.
