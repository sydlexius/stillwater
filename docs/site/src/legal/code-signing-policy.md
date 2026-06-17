---
description: How Stillwater releases are signed, who approves them, and SignPath Foundation attribution.
---

# Code Signing Policy

Stillwater binaries are code-signed through the
[SignPath Foundation](https://signpath.org/) free code-signing program for open-source projects.

> Free code signing provided by [SignPath.io](https://signpath.io/), certificate by
> [SignPath Foundation](https://signpath.org/).

---

## Signing roles

The following roles apply to Stillwater's signing process, as defined by SignPath's role model.

| Role | Responsibility | Member |
|---|---|---|
| **Author** | Commits code to the repository without requiring additional review | Jesse Slaton (@sydlexius) |
| **Reviewer** | Reviews changes from non-committers and provides code review feedback | Jesse Slaton (@sydlexius) |
| **Approver** | Approves each signing request before a signed binary is released | Jesse Slaton (@sydlexius) |

Automated review tools (CodeRabbit, Codoki) assist the human review process but do not hold any signing role. All signing decisions are made by the human Approver above.

---

## Release and signing process

Every release follows this process:

1. A release build is produced by CI (GitHub Actions) from a tagged commit on the `main` branch.
2. Checksums are generated for each build artifact.
3. A signing request is submitted to SignPath.io.
4. **A human Approver (listed above) manually reviews and approves the signing request.** No release is signed or published automatically without this step.
5. Signed artifacts are published to the GitHub Releases page.

---

## Verifying a release

Each release ships with Sigstore-signed checksums and SLSA build provenance. See the
[binary install guide](../getting-started/install-binary.md) for verification instructions.

---

## Privacy

See the [Privacy Policy](privacy-policy.md) for a full description of what Stillwater transmits over the network and what it does not collect.
