# Retained release-branch synchronization

## Scope

Beta, preprod, and main serve different purposes:

- Beta is the moving development branch.
- Preprod is the independently validated release candidate.
- Main is the stable published release.

The protected release train accepts only beta to preprod and preprod to main
pull requests. A normal pull-request merge cannot reproduce the existing main
commit SHA, so after a successful stable publication the release workflow may
fast-forward the retained branches to that exact SHA.

## Admission and ordering

The sync-release-branches job runs only when all of the following are true:

1. A same-repository preprod to main pull request was merged.
2. Create-stable-tag completed successfully.
3. The corresponding GitHub Release was created or already existed.
4. Main still equals the merged candidate SHA.

The job shares the release-tag concurrency group. It fetches exactly main,
preprod, and beta, then performs an ordinary push only when the branch tip is
an ancestor of the candidate. It never uses force push.

This gives the following outcomes:

| Branch state at synchronization time | Result |
| --- | --- |
| Branch already equals candidate | No-op |
| Branch is an ancestor of candidate | Fast-forward to candidate |
| Branch has new work after candidate | Leave unchanged and report it |
| Main changed before or during work | Fail closed; do not claim synchronization |

Therefore a concurrent beta commit cannot be overwritten or accidentally
published. A newly advanced preprod branch is also left untouched.

## Security boundary

The workflow has contents read permission; branch writes use a dedicated
repository Deploy key stored as the RELEASE_BRANCH_SYNC_DEPLOY_KEY environment
secret. The release-branch-sync environment permits deployments from main only.
The active preprod-release-train ruleset preserves pull-request, status check,
deletion, and non-fast-forward protections while allowing the Deploy-key actor
to execute this narrow post-release fast-forward.

Operational controls:

- Keep exactly one write-enabled Deploy key in this repository: the dedicated
  release-sync key.
- Do not expose the environment to beta, feature branches, forks, or
  pull-request workflows.
- Rotate the key by replacing both the repository Deploy key and environment
  secret, then verify the ruleset remains active.
- Investigate any ruleset-bypass audit event; a bypass must correspond to a
  successful formal preprod to main promotion.

## Validation

The workflow is statically checked with actionlint and YAML parsing. Its
fast-forward predicate is additionally exercised against local bare-repository
fixtures for already-synchronized, ancestor, advanced-branch, and stale-main
states. Remote acceptance is observed on the next formal promotion, where the
job must report the exact candidate SHA and branch outcomes.
