# aws-elevate

Accesso elevato temporaneo (just-in-time) per AWS IAM Identity Center: un utente chiede un permission set per un tempo limitato, e alla scadenza le credenziali smettono di funzionare.

**Stato:** fase di design, nessuna implementazione. Versione di partenza: 0.0.1.

- Design: [`docs/2026-11-14-aws-elevate-design-v2.md`](docs/2026-11-14-aws-elevate-design-v2.md)
- Licenza: Apache-2.0

## Branch

- `main`: versioni rilasciate.
- `develop`: base per i branch di lavoro; arriva su `main` tramite pull request.

## Development

Everything runs in a container; only Docker is needed on the host.

```bash
make dev                          # shell with Go, Python, uv and the AWS CLI
make dev-run CMD="go version"     # run one command in the container
```
