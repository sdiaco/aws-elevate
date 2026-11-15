# aws-elevate

Temporary elevated access (just-in-time) for AWS IAM Identity Center: a user requests a permission set for a limited time, and when it expires the credentials stop working.

**Status:** in development, not yet usable. First version: 0.0.1.

- Design (Italian): [`docs/2026-11-14-aws-elevate-design-v2.md`](docs/2026-11-14-aws-elevate-design-v2.md)
- License: Apache-2.0

## Branches

- `main`: released versions.
- `develop`: base for work branches; reaches `main` through a pull request.

## Development

Everything runs in a container; only Docker is needed on the host.

```bash
make dev                          # shell with Go, Python, uv and the AWS CLI
make dev-run CMD="go version"     # run one command in the container
make check                        # lint and tests, as run by the pre-push hook
lefthook install                  # once per clone: commit message and pre-push hooks
```
