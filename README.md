# warren

Warren is a Kubernetes orchestrator for Open Terminal sandboxes.
It gives each Open WebUI user, chat, or model an isolated sandbox pod.
Warren is an independent project with the Apache-2.0 license. It is not an Open WebUI product.

Status: research phase. There is no implementation at this time.

## Documents

| Document | Contents |
|---|---|
| [docs/protocol.md](docs/protocol.md) | How Open WebUI calls a terminal server |
| [docs/open-terminal-api.md](docs/open-terminal-api.md) | The Open Terminal routes that warren must proxy |
| [docs/feature-parity.md](docs/feature-parity.md) | Features of the enterprise orchestrator and the warren plan |

## Clean-room rule

Contributors must not read source code from `github.com/open-webui/terminals` or its forks.
Contributors must not copy that source code.
Contributors must not change that source code for use in warren.
