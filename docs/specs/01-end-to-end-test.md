# Specification 01: end-to-end test with Open WebUI

Status: proposal. The owner of the project must approve it.

## 1. Purpose

The tests of warren use a simulated Open WebUI. The cluster checks in `test/cluster/` call warren directly.
No test shows that a real Open WebUI can use warren.

This work adds a test that sends each request through Open WebUI 0.13.4.
The test also gives answers to the questions of section 3. The current documents do not answer them.

## 2. Scope

In scope:

- A test environment with Open WebUI, warren, and a simulated model server on the kind cluster of `test/cluster/`.
- A test program that calls only the public HTTP and WebSocket routes of Open WebUI.
- A record of each new fact in `protocol.md`.

Not in scope:

- A test of the browser user interface with a browser program.
- A test with a real model.
- Open WebUI versions before 0.13.4 (`protocol.md`, section 9.5, decision 1).

## 3. Questions that the test must answer

| No. | Question | Why it is important |
|---|---|---|
| Q1 | Open WebUI makes the ID of a template and keeps the access grants of a template in its database (`protocol.md`, section 4.6). A warren policy has no record in that database. Which users see such a terminal: all users, only an administrator, or no user? | If only an administrator sees it, the read-only design of warren does not work for users. |
| Q2 | Can an administrator give access grants to such a terminal, when the `PATCH` call to warren gets the status 403? | Access grants are the primary method to give a policy to users and groups (`feature-parity.md`, section 12). |
| Q3 | What does Open WebUI show when the `POST`, `PATCH`, or `DELETE` call for a template gets the status 403? | The person must see the text of warren, and not a general failure. |
| Q4 | Does a WebSocket upgrade request of Open WebUI have an `Origin` header on a path B route or on the path G route? | Warren refuses an upgrade request that has an `Origin` header (`internal/proxy/ws.go`, `upgrader`). |
| Q5 | What is the default of `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER`? | Warren holds a tool call for a maximum of 50 seconds while a sandbox starts (`internal/proxy/proxy.go`, `startTimeout`). |
| Q6 | Which instance `status` texts does Open WebUI show, and does it call the status route before each terminal opens? | Warren sends `running` or a different text. Only `running` is defined (`protocol.md`, section 4.6). |

Record each answer in `protocol.md` with the source file and the line, or with the test case that shows it.
If the answer to Q1 or Q2 is "no", stop and tell the owner. Specification 06 then becomes the primary work.

## 4. Test environment

### 4.1 Parts

| Part | Source | Notes |
|---|---|---|
| Cluster | `test/cluster/setup.sh` | The kind cluster `warren-dev` with Calico. No change. |
| Warren | A build from the repository | Installed with the chart of specification 02 when that chart exists. Before that, with `test/cluster/manifests.yaml`. |
| Open WebUI | The published image of version 0.13.4, with its digest | One pod in the namespace `openwebui`. The database is a file in an `emptyDir` volume. |
| Model server | `test/e2e/model.py` | A small HTTP server with the OpenAI chat completion API. Refer to section 4.3. |

### 4.2 Configuration of Open WebUI

- Set the connection with the variable `TERMINAL_SERVER_CONNECTIONS` (`protocol.md`, section 3).
- The connection has `url` = the Service of warren, `key` = the connection key, `auth_type` = `bearer`, and `enabled` = `true`.
- Do not set `server_type` in the variable. The test calls the "verify" operation, and Open WebUI finds the type (`protocol.md`, section 3.1). This is test case E1.
- Set `ENABLE_TOOL_SERVERS` to true (`protocol.md`, section 3).
- Point Open WebUI to the model server. Record the names of the variables for this in `protocol.md`.
- The NetworkPolicy of the warren namespace permits connections to warren only from the Open WebUI pod.

### 4.3 Model server

The model server makes the tool path (path C) deterministic.

- It has one model with the name `e2e`.
- When the last message of a request is a user message, it replies with one tool call. The tool name and the arguments come from the text of the user message, which is a JSON object.
- When the last message is a tool result, it replies with the text of the result.
- It supports the stream form and the plain form of the reply.
- It writes each request to its log. The test reads the log to see the tool list and the system prompt that Open WebUI sent.

### 4.4 Users

The test makes three accounts with the public sign-up and administrator routes of Open WebUI:

| Account | Role | Use |
|---|---|---|
| `admin` | Administrator | Makes the connection. Manages the terminals. |
| `alice` | User | Uses the terminals. Is in the `users` list of the policy `tuned`. |
| `bob` | User | Shows the separation from `alice`. |

Record the routes that the test uses in a new section 10 of `protocol.md`.

### 4.5 Policies

Use the policy file of `test/cluster/manifests.yaml`. It has one policy for each scope and one policy with a user list.

## 5. Test cases

Each case prints one line that starts with `PASS` or `FAIL`, as the cluster checks do.
"The log" is the audit log of warren (`"log":"audit"`).

### 5.1 Connection and templates (path F)

| Case | Steps | Expected result |
|---|---|---|
| E1 | `admin` calls the "verify" operation for the connection. | Open WebUI reports the type `orchestrator`. The log has `GET /api/v1/terminals/templates` with the status 200. |
| E2 | `admin` gets the terminal list. | The list has one terminal for each policy. Each ID has the form `template:{connection_id}:{template_id}`. |
| E3 | `alice` gets the terminal list. | Answers Q1. |
| E4 | `admin` gives `alice` and `bob` access to the terminals. | Answers Q2. The cases below need this access. |
| E5 | `admin` sends the make, change, and delete calls for a template. | Warren replies with the status 403. Answers Q3. With specification 06, this case changes: the calls are successful. |
| E6 | `admin` gets the limits of the server. | The reply has `network_profiles` with `internet`, `none`, and `custom`. |

### 5.2 HTTP proxy (path A)

| Case | Steps | Expected result |
|---|---|---|
| E10 | `alice` sends `GET files/cwd` to the terminal `personal`. | Status 200. The log has the user ID of `alice`, the template `personal`, and the context `user:<id of alice>`. One pod exists. |
| E11 | `alice` writes a file. `bob` reads the same path in the same terminal. | `bob` gets a "not found" reply. Two pods exist. |
| E12 | `alice` uses the terminal `by-chat` with a saved chat, then with a temporary chat. | The saved chat gets the status 200. The temporary chat gets the status 409 from Open WebUI. |
| E13 | `alice` and `bob` use the terminal `team`. | The two users see the same file. One pod exists, with the context `default`. |
| E14 | `alice` uses a terminal of the scope `model` with a model ID. | Add a policy with the scope `model` to the file for this case. The context is `model:<id>`. |
| E15 | `bob` uses the terminal `tuned`. | Status 403 with the text of warren. No pod starts. |
| E16 | `alice` adds `user_id=<id of bob>` to the query of a request. | The log has the user ID of `alice`. |
| E17 | `alice` uploads a file of 64 MiB and downloads it. | The content is the same. The memory of warren does not increase by the size of the file. |
| E18 | `alice` starts an HTTP server in the sandbox and gets a page through `proxy/{port}/`. The page sets a cookie and sends a redirect to a different host. | The reply has no `Set-Cookie` header. The `Location` header follows the rules of `internal/proxy/response.go`. |

### 5.3 WebSockets (path B)

| Case | Steps | Expected result |
|---|---|---|
| E20 | `alice` makes a terminal session and opens its WebSocket. She sends the first message with her session token. She types a command. | The output of the command comes back. Answers Q4 for path B. |
| E21 | `alice` opens the `events` WebSocket and changes a file. | A file change message comes. |
| E22 | `bob` opens a WebSocket of the terminal `tuned`. | Open WebUI closes it. Record the close code. |
| E23 | `alice` opens a WebSocket of a sandbox that is stopped. | The WebSocket opens after the start, or closes with the code 1013 and opens at the second attempt. |
| E24 | The test starts warren again while `alice` has an open terminal. | The WebSocket closes. A new WebSocket opens, and the shell session continues. |
| E25 | `alice` makes a browser session and opens its view WebSocket. | The token in the reply is the token of Open WebUI, and the WebSocket opens (`protocol.md`, section 8.1). |

### 5.4 Tool calls and helper calls (paths C and D)

| Case | Steps | Expected result |
|---|---|---|
| E30 | `alice` sends a chat message with the terminal `personal` and the model `e2e`. The message asks for the tool that runs a command. | The chat reply has the output of the command. The log of the model server shows the tool list of warren. |
| E31 | The same as E30, with a sandbox that is stopped. | The chat turn does not fail. The tool list is the complete list of warren. Answers Q5. |
| E32 | `alice` adds a file to a chat that uses the terminal. | The file is in the sandbox (`files/upload`, path D). |

### 5.5 Agent sessions (path G)

| Case | Steps | Expected result |
|---|---|---|
| E40 | With an image that has an agent program: `alice` starts an agent session. | The WebSocket of the Open WebUI backend opens. Answers Q4 for path G. If no image has an agent program, the case prints `SKIP` with the cause. |

### 5.6 Instance operations (path F)

| Case | Steps | Expected result |
|---|---|---|
| E50 | `admin` gets the instance list of the terminal `personal`. | The list has the instances of `alice` and `bob` with the `context_id` values `user:<id>`. Answers Q6. |
| E51 | `admin` stops the instance of `alice`. `alice` then sends a request. | The pod goes away. The request starts a new pod. The file of `alice` is there. |
| E52 | `admin` restarts the instance of `alice`. | A new pod operates. The status is `running`. |
| E53 | `admin` deletes the instance of `alice`. `alice` then sends a request. | The volume goes away. The home directory of the new sandbox is empty. |

### 5.7 Lifecycle

| Case | Steps | Expected result |
|---|---|---|
| E60 | `alice` uses a terminal and then waits for the idle time. She sends a request. | The pod stops. The request starts a new pod with the same files. |
| E61 | The test replaces the connection key in the Secret of warren and in the connection of Open WebUI. | After the two changes, requests are successful. No restart of warren occurs. |

## 6. Files

| File | Content |
|---|---|
| `test/e2e/README.md` | How to do the test, as `test/cluster/README.md` |
| `test/e2e/manifests.yaml` | The namespace, the Deployment, and the Service of Open WebUI and of the model server |
| `test/e2e/model.py` | The model server of section 4.3. It uses only the standard library of Python. |
| `test/e2e/openwebui.py` | The test program. It uses the package `websockets`, as `lifecycle.py` does. |
| `test/e2e/run.sh` | Builds the image, installs the parts, runs the test program, and writes the output to a file |
| `.github/workflows/e2e.yml` | Refer to section 7 |

Pin the Open WebUI image with its digest. Renovate must propose a new version, and a person must approve it: a new version can change the protocol.

## 7. Continuous integration

- The workflow `e2e.yml` runs each night and on a manual start. It does not run for each pull request: the test takes too long.
- It makes the kind cluster on the runner, runs `test/cluster/run.sh`, and then runs `test/e2e/run.sh`.
- It keeps the output files and the logs of warren and Open WebUI as an artifact.
- The release workflow of specification 02 needs a successful run of this workflow for the same commit.
- The workflow has only the permission `contents: read`. Each action has a full commit ID, as in `ci.yml`.

## 8. Documentation

- `protocol.md`: the answers to Q1 to Q6, and section 10 with the routes that the test uses.
- `README.md`: remove "the end-to-end test with Open WebUI" from the list of work that is not complete.
- `feature-parity.md`, section 17: say that the test proves version 0.13.4.

## 9. Open items

| No. | Item |
|---|---|
| O1 | The routes of Open WebUI for the sign-up, the terminal list, the access grants, and the chat are not in `protocol.md`. Read them from the source code of Open WebUI, and record facts only. |
| O2 | The names of the variables that point Open WebUI to a model server. |
| O3 | Which published Open Terminal image has an agent program for case E40. |

## 10. Acceptance

1. `test/e2e/run.sh` ends with no `FAIL` line on a new kind cluster.
2. `protocol.md` has an answer to each question of section 3.
3. The nightly workflow is green for three nights.
4. Each case fails when the function that it examines is off. Show this for E11, E15, and E16 with a temporary change of warren.
