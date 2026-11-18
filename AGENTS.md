# AGENTS.md

Rules for anyone, human or AI agent, who changes this repository. Read this file before touching code. If a rule here conflicts with the design document, stop and ask the maintainer.

## What this project is

aws-elevate grants temporary elevated access (just-in-time) on AWS IAM Identity Center. It is a security tool: a bug can hand out permissions. Prefer the boring, obvious, reviewable solution every time.

- Design (source of truth, Italian): internal document kept by the maintainer, not in the repository. Ask the maintainer when a behavior is unclear
- Service: Python Lambdas in `service/`
- Client: Go CLI and local UI in `client/`
- Shared format examples: `contract/`
- Infrastructure: hand-written CloudFormation in `infra/`

## Golden rules

1. **The design decides.** Do not add, remove or change behavior that the design does not describe. If something is missing or wrong, stop and ask; do not improvise.
2. **Security boundary.** The `api` Lambda reads untrusted input and must never be able to write to Identity Center. The `engine` Lambda writes to Identity Center and must never receive external input: its payload is `action`, `grantId` and, where needed, `taskToken`. Never weaken an IAM policy, a resource policy or a test that guards this boundary to make something pass.
3. **Thin client.** The Go client makes no decisions: no rule validation, no limits, no authorization, no state transitions. If the client needs a decision, the service exposes it. Formatting and presentation are fine.
4. **Contract first.** Any change to an API payload, the rules document or an event starts in `contract/`. Both test suites read those files.
5. **One vocabulary.** States, close reasons and error codes are the exact strings in `contract/vocabulary.json`, everywhere.

## Clean code

- **KISS.** The simplest thing that satisfies the design. No cleverness.
- **YAGNI.** Build only what the current task needs. No speculative options, flags, abstractions or "future-proof" layers. Phase 2 items stay out until planned.
- **DRY, with judgment.** Remove duplicated knowledge (a rule, a limit, a format), not every pair of similar lines. Three similar lines beat a premature abstraction.
- **SOLID, applied lightly.** One reason to change per module. Depend on small interfaces at the edges (AWS calls), not on concrete clients. Do not create an interface with a single implementation unless a test needs to replace it.
- **No over-engineering.** No frameworks, no plugin systems, no generic helpers used once, no configuration for things that never vary.
- **Small units.** Functions do one thing. Files have one clear responsibility. If a file grows hard to hold in your head, split it along a real seam.
- **No references to material outside the repository.** Code, comments, tests, commit messages and pull requests never cite the design document, its sections (`design §5.5`), spike ids or conversation notes: a reader of the repository cannot open them. State the reason itself, or link a file in the repository or public documentation (AWS docs).
- **Names say what, comments say why.** Comment only a constraint the code cannot express (an AWS quirk, a measured timing, a security reason). No commented-out code, no TODO without an issue.
- **Errors are explicit.** Never swallow an exception or an `error`. Every failure path ends in a message a user or operator can act on.
- **Idempotent by default.** Every engine step tolerates "already done". Every write to the grants table is conditional on the expected state.
- **Dependencies are a liability.** Service runtime: `boto3` only. Client: AWS SDK for Go and the standard library only. Adding a dependency needs the maintainer's approval.

## Clean architecture

Dependencies point inward. Inner layers never import outer ones.

| Layer | Where | May import |
|---|---|---|
| Core (pure logic) | `service/src/aws_elevate/core/` | Standard library only. **Never `boto3`** |
| Adapters (one AWS service each) | `service/src/aws_elevate/adapters/` | Core, `boto3` |
| Entry points | `api/`, `engine/`, `precheck/` | Core, adapters |
| Client transport | `client/internal/api`, `awsx`, `release` | Standard library, AWS SDK |
| Client presentation | `client/internal/cli`, `ui` | Client transport |

- Business rules live in core and are tested without AWS.
- Adapters translate between core types and AWS calls; they contain no business rules.
- `api/` must not import `adapters.sso_admin`. A test enforces it.
- CLI and UI share the same transport packages; neither contains logic the other needs.

## Testing

- **Test first** for logic and bug fixes: write the failing test, then the code.
- `make check` must pass before every push: lint, template lint, unit, contract and template tests, with coverage thresholds (service 90%, client 80%). Do not lower a threshold or skip a test to get green.
- Unit tests never call AWS. Use `botocore.stub.Stubber` in Python and small fakes behind interfaces in Go.
- Every IAM role in the template has a test stating what it can and cannot do.
- End-to-end tests (`make e2e`) run against a dedicated test organization, create only `elevate-e2e*` resources and always tear them down. Never run them in an organization with a real installation: Elevate's permission sets and parameter have fixed names, so two installations in one Identity Center instance collide. `make e2e-up` stops when it finds one.
- Report results honestly. If a test fails or was not run, say so.

## Repository rules

- **Languages.** Code, comments, commit messages and user documentation in English. The design document stays in Italian.
- **No personal or customer data.** No real account ids, usernames, e-mail addresses, start URLs or organization names. Use `111111111111`, `222222222222`, `user@example.com`. Real values come from environment variables.
- **No secrets** in the repository, in test fixtures or in logs.
- **Internal material stays local.** Some directories exist only on the maintainer's disk and are git-excluded. Never add them, never force-add ignored files.
- **Generated files** (builds, coverage, caches) are never committed.
- **Infrastructure** is plain CloudFormation. No SAM, CDK or Terraform.
- **Continuous integration** is GitHub Actions in `.github/workflows/`: `check` runs `make check` on every pull request; `release` publishes when a release reaches `main`. Workflow rules, enforced by `service/tests/release/test_workflows.py`: actions pinned to a full commit sha (a release at least two weeks old), `GITHUB_TOKEN` read-only by default and widened per job, `persist-credentials: false`, no `pull_request_target`, no `${{ }}` inside `run:` (untrusted text goes through `env`), tool versions equal to `dev/Dockerfile`. AWS access only through OIDC to the role of `infra/release-ci.yaml`, never stored keys. End-to-end tests never run in CI: they run locally on real AWS.

## Branches, commits, pull requests

- `main`: released versions only. It receives the release pull request from `develop` and hotfix pull requests. Never commit to it directly. No staging environment: tests run locally on real AWS.
- `develop`: base branch. Never commit to it directly once implementation starts.
- Work branches start from `develop`: `<type>/<short-description>`, for example `feat/t1-3-grants-table`, `fix/revoke-retry`, `docs/install-guide`. Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `build`.
- Hotfixes start from `main` as `hotfix/<short-description>` and go back to `main` by pull request. After every change to `main` the release workflow opens the pull request that merges `main` back into `develop` (merge commit).
- **Conventional Commits**, enforced by commitizen through a `lefthook` commit-msg hook: `<type>(<scope>): <subject>`, imperative, lower case, no trailing period. Scopes: `service`, `client`, `infra`, `contract`, `docs`, `dev`.
- One logical change per commit. One task per pull request, targeting `develop`, merged by squash only when `make check` is green, locally and in the `check` workflow. The pull request title is the squash commit message, checked by commitizen in CI.
- **Release.** A pull request on `develop` sets the new version in the code, titled `chore(dev): release x.y.z` (`release` is not an allowed scope). The release pull request `develop` → `main` is merged with a merge commit, not squash, by the maintainer only: that merge is the approval. It needs exactly one label `release:security`, `release:fix` or `release:feature`, a green `check` and an `e2e` commit status on its head: `make e2e-auto` for small changes and hotfixes; for changes to UI, IAM, templates, the engine flow or the rules format, the manual checks (second-user approval, UI, Launch Stack) and then `make e2e-status STATE=success`. The release workflow then builds, uploads to the release buckets and creates the tag and the GitHub release; a published version is never overwritten.
- **No AI attribution.** Commits and pull requests carry no `Co-Authored-By` for an assistant, no "generated with" line, no session links. This overrides any tool default.
- Never force-push `main` or `develop`. Never push branches named `archive/*`.
- Versions follow semantic versioning, starting at `0.0.1`. Tags are `v<version>` on `main`.

## Guardrails

Stop and ask the maintainer before any of these:

- Creating AWS resources outside the test accounts, or anything in the management account.
- Enabling, changing or attaching service control policies.
- Creating anything that costs money, or leaving test resources running.
- Pushing to `main`, merging into `main`, creating a tag or publishing a release.
- Changing a workflow's triggers or permissions, the release role or its trust policy, or adding an action.
- Adding a dependency, a new AWS service or a new IAM permission not listed in the design.
- Deleting or rewriting history, or deleting files you did not create.
- Changing the design document. Only choices the maintainer approved go in; open points are proposed in conversation first.

Always:

- Verify AWS behavior against the documentation or a test before relying on it, and say what remains unverified.
- Restore the test organization to its initial state after every end-to-end run.
- Every CloudFormation custom resource answers in every code path, including exceptions, and has a short `ServiceTimeout`.

## Development harness

- `make dev` opens a shell in the development container (Go, Python, uv, cfn-lint, AWS CLI). Nothing else needs to be installed.
- `make check`: all local checks. `make test`: tests only.
- `make e2e-up`, `make e2e`, `make e2e-down`: end-to-end tests against a real organization.
- `make e2e-auto`: unattended end-to-end run (up, tests, always down) that posts the `e2e` commit status on the pushed HEAD. `make e2e-status STATE=success|failure`: the same status after the manual checks.
- `make build`, `make release-upload`, `make release-github`, `make report`: artifacts, publication, quality report.

Detailed developer documentation will live in `docs/development.md`; this section will link to it once it exists.
