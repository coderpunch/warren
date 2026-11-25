# warren

Warren is a Kubernetes orchestrator for Open Terminal sandboxes.
It gives each Open WebUI user, chat, or model an isolated sandbox pod.
Warren is an independent project with the Apache-2.0 license. It is not an Open WebUI product.

Status: in development. The proxy, the authentication, and the sandbox lifecycle are complete.
Storage, policies with resource limits, network isolation, and the Helm chart are not complete.

## Documents

| Document | Contents |
|---|---|
| [docs/protocol.md](docs/protocol.md) | How Open WebUI calls a terminal server |
| [docs/open-terminal-api.md](docs/open-terminal-api.md) | The Open Terminal routes that warren must proxy |
| [docs/feature-parity.md](docs/feature-parity.md) | Features of the enterprise orchestrator and the warren plan |

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
