# Contributing

Elevate is a security tool: a bug can hand out permissions. Prefer the boring, obvious, reviewable change. The full rules, for people and for AI agents, are in [AGENTS.md](../AGENTS.md); this page is the short version.

- [Set up](#set-up)
- [Repository layout](#repository-layout)
- [Rules that matter most](#rules-that-matter-most)
- [Branches and commits](#branches-and-commits)
- [Tests](#tests)
- [Releases](#releases)

---

## Set up

Everything runs in a container: only Docker is needed on the host.

```bash
lefthook install                  # once per clone: commit message and pre-push hooks
make dev                          # shell with Go, Python, uv, cfn-lint and the AWS CLI
make dev-run CMD="go version"     # one command in the container
make check                        # lint and tests, as run before every push
```

---

## Repository layout

| Folder | What is in it |
|---|---|
| `service/` | Python Lambda functions: `api`, `engine`, `precheck` |
| `client/` | Go command line and local interface |
| `contract/` | API payloads, rules and events: the format both sides test against |
| `infra/` | Hand-written CloudFormation |
| `dev/` | Container, build and release scripts |

---

## Rules that matter most

1. **The design decides.** Do not add behavior that is not agreed. Open an issue first.
2. **The security boundary.** `api` reads untrusted input and must never write to Identity Center. `engine` writes to Identity Center and never receives external input. Never weaken a policy or a test that guards this.
3. **Thin client.** The client makes no decisions: no rule validation, no limits, no authorization.
4. **Contract first.** A change to a payload, the rules or an event starts in `contract/`.
5. **Few dependencies.** Service: `boto3` only. Client: AWS SDK for Go and the standard library only.
6. **No real data.** No real account ids, user names, e-mail addresses or secrets. Use `111111111111` and `user@example.com`.

---

## Branches and commits

- `develop` is the base branch; `main` holds released versions only.
- Work branches start from `develop`: `<type>/<short-description>`, for example `fix/revoke-retry`. Types: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `build`.
- Commits follow [Conventional Commits](https://www.conventionalcommits.org/): `<type>(<scope>): <subject>`, imperative, lower case, no trailing period. Scopes: `service`, `client`, `infra`, `contract`, `docs`, `dev`.
- One task per pull request, to `develop`, merged by squash when `check` is green. The pull request title is the squash commit message.

---

## Tests

- Write the failing test first, then the code.
- Unit tests never call AWS.
- `make check` must pass: lint, unit, contract and template tests, coverage at least 90% for the service and 80% for the client.
- End-to-end tests (`make e2e`) run on real AWS, in a dedicated test organization only, and never in CI.

---

## Releases

The maintainer releases: a pull request on `develop` sets the version, then the release pull request `develop` → `main` is merged with a merge commit after the checks and the end-to-end tests. The release workflow publishes the templates and the code, the tag and the GitHub release. A published version is never overwritten.
