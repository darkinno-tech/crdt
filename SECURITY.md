# Security Policy

## Supported versions

Security fixes are developed on `beta` and released to `main` through the
reviewed beta-to-main release path. Report an issue against the latest commit
on one of those branches; older tags and forked builds are not supported.

| Version | Supported |
| --- | --- |
| Latest `main` release | Yes |
| Current `beta` pre-release | Yes, before promotion |
| Older releases | No |

## Reporting a vulnerability

Please do not report suspected vulnerabilities in a public issue, discussion,
or pull request. Use GitHub's private vulnerability-reporting flow for this
repository. If that flow is unavailable, contact a repository maintainer by a
private GitHub message and include only the minimum information needed to open
a confidential channel.

Include the affected version or commit, a minimal reproduction, impact,
affected package or protocol boundary, and any suggested mitigation. Do not
include credentials, private keys, production snapshots, or personal data.

## What to expect

Maintainers will acknowledge a report, assess reproducibility and impact, and
coordinate a fix before public disclosure when possible. Fixes preserve the
library's explicit protocol boundaries: frame checksums detect corruption but
do not authenticate peers, and host applications remain responsible for TLS,
identity, authorization, rate limits, and key management.

## Scope notes

Reports are especially useful for unbounded allocation or CPU work, malformed
frame handling, unsafe codec boundaries, transport authentication assumptions,
and invalid state transitions. This repository is a CRDT library and reference
provider collection, not a hosted collaboration service; deployment-specific
credentials and infrastructure must be reported to their respective operator.
