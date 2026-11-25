# warren

Warren is a Kubernetes orchestrator for Open Terminal sandboxes.
It gives each Open WebUI user, chat, or model an isolated sandbox pod.
Warren is an independent project with the Apache-2.0 license. It is not an Open WebUI product.

Status: in development. The proxy, the authentication, the sandbox lifecycle, the storage, and the policies are complete.
The network isolation, the pod security settings, and the Helm chart are not complete. Do not use warren with users that you do not trust.

## Documents

| Document | Contents |
|---|---|
| [docs/protocol.md](docs/protocol.md) | How Open WebUI calls a terminal server |
| [docs/open-terminal-api.md](docs/open-terminal-api.md) | The Open Terminal routes that warren must proxy |
| [docs/feature-parity.md](docs/feature-parity.md) | Features of the enterprise orchestrator and the warren plan |
| [docs/policies.md](docs/policies.md) | The policy file, the storage of a sandbox, and the instance operations |

## Tests

```bash
go test -race ./...
```

## Contributions

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a pull request.

## Clean-room rule

Contributors must not read source code from `github.com/open-webui/terminals` or its forks.
Contributors must not copy that source code.
Contributors must not use that source code as a model for warren, in the same or a different programming language.
