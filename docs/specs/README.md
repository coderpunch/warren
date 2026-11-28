# Implementation specifications

Status: proposal. The owner of the project must approve each specification before the work starts.

Date: 2026-11-28.

These documents describe the work that closes the gaps between warren and the enterprise orchestrator of Open WebUI.
The gaps come from a review of the code against [feature-parity.md](../feature-parity.md).

## 1. The specifications

| No. | Document | Subject | Size | Depends on |
|---|---|---|---|---|
| 01 | [01-end-to-end-test.md](01-end-to-end-test.md) | A test of warren with a real Open WebUI | Medium | None |
| 02 | [02-helm-chart-and-image.md](02-helm-chart-and-image.md) | The Helm chart and the release of the container image | Medium | None |
| 03 | [03-limit-variables.md](03-limit-variables.md) | Variables that give the CPU limit and the memory limit to a sandbox | Small | None |
| 04 | [04-maximum-lifetime.md](04-maximum-lifetime.md) | A maximum lifetime for a sandbox pod | Small | None |
| 05 | [05-scheduled-reset.md](05-scheduled-reset.md) | A schedule that clears the files of the sandboxes of a policy | Medium | None |
| 06 | [06-writable-templates.md](06-writable-templates.md) | Templates that a user makes and changes in Open WebUI | Large | 01 |

Do the work in the sequence of the numbers.
The owner decided on 2026-11-28 that users must be able to make terminals. Thus specification 06 is planned work, and not an option.
Specification 01 gives facts that the design of specification 06 needs. Do not start 06 before the open questions of 01 have answers.

## 2. Gaps that have no specification

| Gap | Cause |
|---|---|
| More than one active replica | The state of the leases is in the memory of one process. This needs a design of its own. |
| Administrator web interface | The management API, the metrics, and `kubectl` give the same data. |
| Docker backend, operator backend, one Service for each instance | The plan value in `feature-parity.md` is "Won't do". |
| Egress control by domain name | Standard NetworkPolicy has no domain rules. |
| Package installation at the start of a sandbox | It needs root privilege. It conflicts with the restricted Pod Security Standard. |

## 3. Rules for all specifications

1. The clean-room rule of [CONTRIBUTING.md](../../CONTRIBUTING.md), section 1, applies. Use only the sources that it permits.
2. When the work uses a new fact from a permitted source, record the fact in `protocol.md`, `open-terminal-api.md`, or `feature-parity.md`. Give the source.
3. Keep the trust model of `CONTRIBUTING.md`, section 7. Specification 06 moves a trust boundary. It says so.
4. Do not add a database. The state is in Kubernetes objects.
5. Add a test for each change in behaviour.
6. Each specification has a list of open items. An open item needs a decision of the owner, or a fact that this session could not get.

## 4. Facts that the review could not get

The review had no access to `docs.openwebui.com` and to the source code of Open WebUI.
Thus these specifications use only the facts that are already in `docs/`.
Each specification marks a statement that needs a check against a permitted source as an open item.
