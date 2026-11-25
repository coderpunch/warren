# Open Terminal API: the endpoints that warren must proxy

Status: research record. This document contains no design decisions that are final.

## 1. Sources and rules

| Item | Value |
|---|---|
| Repository | `github.com/open-webui/open-terminal` |
| Version | `0.18.1` (`pyproject.toml:3`) |
| Commit | `3e8ffa2fee3a1701d69b5f232b5145dda76aa02c` (2026-11-25) |
| License | MIT (`pyproject.toml:5`) |

All file paths in this document are relative to the root of that repository.
A reference such as `main.py:489` means line 489 of `open_terminal/main.py`. A reference with a full path gives the full path.

Sentences that start with "Inference:" are conclusions from framework behaviour. The repository does not state them. A test must confirm them before the implementation relies on them.

The checkout refers to the enterprise orchestrator only as a recommendation for one container for each user (`README.md:138`, `SECURITY.md:41`, `docs/deployment.md:203-208`). It does not describe the API of that product.

## 2. Summary

| Item | Value | Reference |
|---|---|---|
| Listen address | `0.0.0.0:8000`. Only the flags `--host` and `--port` or the TOML file can change it. | `cli.py:27-28`, `cli.py:109-110` |
| API key variable | `OPEN_TERMINAL_API_KEY`, or `OPEN_TERMINAL_API_KEY_FILE` (a file path). To set the two is an error. | `env.py:18-43`, `cli.py:117-131` |
| HTTP authentication | `Authorization: Bearer <key>`. No query parameter and no cookie is possible. | `dependencies.py:14-31` |
| WebSocket authentication | The first text message: a JSON object with `type` = `auth` and `token` = the key. Time limit 10 seconds. | `main.py:747-762` |
| Health check | `GET /health`. No authentication. Status 200, body `{"status":"ok"}`. | `main.py:489-497` |
| Capability data | `GET /api/config`. No authentication. | `main.py:505-522` |
| Identity headers that the server reads | `X-User-Id`, `X-Session-Id`, `X-Workspace-Id`, `X-Workspace-Directory` | Section 5 |
| Identity inputs that the server ignores | The `user_id` query parameter and all `X-Terminal-*` headers | Section 5 |
| Number of routes | 81 registrations: 71 HTTP and 10 WebSocket | Section 3 |
| Paths that must be writable | The home directory and the temporary directory | Section 8.4 |

## 3. Route inventory

The count of 81 includes each `@app`, `@router`, and `add_api_route` registration in the package.
Three registrations accept more than one method. Thus there are 85 pairs of HTTP method and path, and 10 WebSocket paths.
The count does not include the FastAPI routes `/docs`, `/redoc`, and `/openapi.json`. These routes are public (`main.py:223-228`).

Columns:

- "Auth": `Bearer` is the API key header. `First message` is the WebSocket message of section 4.3. `Path token` is the token of section 7.2. `None` means no authentication.
- "Tool": `Yes` means that the route is in the OpenAPI document. Open WebUI gives these routes to a model as tools.

Some routes exist only when a setting is on:

| Routes | Condition | Reference |
|---|---|---|
| `/system` | `OPEN_TERMINAL_ENABLE_SYSTEM_PROMPT` (default on) | `main.py:536` |
| `/info` | `OPEN_TERMINAL_INFO` is not empty (default empty) | `main.py:548` |
| `/api/terminals...` | `OPEN_TERMINAL_ENABLE_TERMINAL` (default on) | `main.py:2139` |
| `/notebooks...` | The setting is on and the Jupyter packages are installed | `main.py:2740-2743` |
| `/browser...` | `OPEN_TERMINAL_ENABLE_BROWSER` (default on) | `main.py:2746-2748` |
| `/desktop...` | `OPEN_TERMINAL_ENABLE_DESKTOP` (default on) | `main.py:2750-2752` |

### 3.1 Health and metadata (5 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| GET | `/health` | None | No | Liveness | `main.py:489-497` |
| GET | `/api/config` | None | No | Feature flags | `main.py:505-522` |
| GET | `/system/info` | Bearer | No | Metrics and the state of the setup script | `main.py:525-533` |
| GET | `/system` | Bearer | No | System prompt for a model | `main.py:536-545` |
| GET | `/info` | Bearer | Yes | Operator text | `main.py:548-558` |

### 3.2 Command execution (7 routes)

| Method | Path | Auth | Tool | Function | Proxy notes | Reference |
|---|---|---|---|---|---|---|
| GET | `/execute` | Bearer | Yes | List processes | | `main.py:1707-1730` |
| POST | `/execute` | Bearer | Yes | Start a command | Query `wait` (0 to 300 seconds), `tail`. Long poll. | `main.py:1822-1896` |
| GET | `/execute/{process_id}/status` | Bearer | Yes | Output and state | Query `wait`, `offset`, `tail`. Long poll. | `main.py:1899-1948` |
| POST | `/execute/{process_id}/input` | Bearer | Yes | Write to standard input | | `main.py:1951-1983` |
| DELETE | `/execute/{process_id}` | Bearer | Yes | Stop the process | | `main.py:1986-2009` |
| WebSocket | `/execute` | First message | No | Process list updates | Server to client only | `main.py:1772-1788` |
| WebSocket | `/execute/{process_id}/status` | First message | No | Output updates | Query `offset`, `tail`. Close code 4004 if the process is not found. | `main.py:1791-1819` |

### 3.3 Files (19 routes)

| Method | Path | Auth | Tool | Function | Proxy notes | Reference |
|---|---|---|---|---|---|---|
| GET | `/files/cwd` | Bearer | No | Working directory and home | | `main.py:601-618` |
| POST | `/files/cwd` | Bearer | No | Set the working directory | | `main.py:621-636` |
| GET | `/files/list` | Bearer | Yes | Directory list | | `main.py:699-721` |
| GET | `/files/read` | Bearer | Yes | Read text, or extract text from a document | Streamed response | `main.py:894-951` |
| GET | `/files/display` | Bearer | Yes | Signal for the user interface | | `main.py:954-980` |
| GET | `/files/view` | Bearer | No | File bytes | Download of any size | `main.py:983-1020` |
| GET | `/files/serve/{path}` | Bearer | No | Same as `/files/view`, with the path in the URL | Download of any size | `main.py:1023-1032` |
| POST | `/files/write` | Bearer | Yes | Write text | | `main.py:1035-1066` |
| POST | `/files/mkdir` | Bearer | No | Make a directory | | `main.py:1069-1080` |
| DELETE | `/files/delete` | Bearer | No | Delete a file or a directory tree | | `main.py:1083-1100` |
| POST | `/files/move` | Bearer | No | Move or rename | | `main.py:1103-1126` |
| POST | `/files/replace` | Bearer | Yes | Find and replace | | `main.py:1129-1188` |
| GET | `/files/grep` | Bearer | Yes | Content search | | `main.py:1191-1302` |
| GET | `/files/matches` | Bearer | Yes | Name and content search | | `main.py:1305-1388` |
| GET | `/files/search` | Bearer | Yes | File name search | | `main.py:1391-1441` |
| GET | `/files/glob` | Bearer | Yes | Glob search | | `main.py:1444-1543` |
| POST | `/files/upload` | Bearer | No | Multipart upload, field `file` | Request body of any size | `main.py:1561-1622` |
| POST | `/files/archive` | Bearer | No | ZIP download of more than one path | Download of any size | `main.py:1632-1699` |
| POST | `/files/compare` | Bearer | No | Difference between two files | Limit 60 seconds | `main.py:887-891` |

File downloads do not support HTTP range requests (`open_terminal/utils/fs.py:31-41`).

### 3.4 Interactive terminals (7 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| POST | `/api/terminals` | Bearer | No | Make a shell session. The response has `id`, `created_at`, `pid`. | `main.py:2209-2330` |
| GET | `/api/terminals` | Bearer | No | List the sessions of this user and chat | `main.py:2446-2464` |
| GET | `/api/terminals/{session_id}` | Bearer | No | Session data | `main.py:2467-2480` |
| DELETE | `/api/terminals/{session_id}` | Bearer | No | Stop the session | `main.py:2483-2489` |
| GET | `/api/terminals/user/output` | Bearer | Yes | Recent output of the shell of the user | `main.py:2423-2429` |
| POST | `/api/terminals/user/input` | Bearer | Yes | Type into the shell of the user | `main.py:2432-2443` |
| WebSocket | `/api/terminals/{session_id}` | First message | No | Connect to the shell | `main.py:2492-2733` |

Facts about the terminal WebSocket:

- Keys and output are binary messages. Control data is JSON text messages (`main.py:2681-2709`).
- The session ID has 8 characters (`main.py:2209-2330`). It is an identifier. It is not a secret.
- If the session ID is unknown, the server closes with code 4004 before it accepts the WebSocket (`main.py:2532-2540`). Inference: the client sees a failed HTTP upgrade and not a close message.
- After authentication, the server compares `X-User-Id` and the chat ID with the values of the session. If they are different, it closes with code 4004 (`main.py:2545-2550`).
- A shell continues to operate when no viewer is connected (`main.py:2523-2527`).

### 3.5 Notebooks (4 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| POST | `/notebooks` | Bearer | No | Start a kernel for a notebook file | `open_terminal/utils/notebooks.py:158-217` |
| POST | `/notebooks/{session_id}/execute` | Bearer | No | Run one cell. This can take 120 seconds. | `open_terminal/utils/notebooks.py:220-293` |
| GET | `/notebooks/{session_id}` | Bearer | No | Session state | `open_terminal/utils/notebooks.py:296-311` |
| DELETE | `/notebooks/{session_id}` | Bearer | No | Stop the kernel | `open_terminal/utils/notebooks.py:314-322` |

### 3.6 Ports and port proxy (2 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| GET | `/ports` | Bearer | No | TCP ports that listen in the container | `main.py:2019-2061` |
| GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS | `/proxy/{port}/{path}` | Bearer | No | Reverse proxy to `http://localhost:{port}/{path}` | `main.py:2079-2132` |

Refer to section 7.1 for the port proxy.

### 3.7 Browser (13 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| POST | `/browser/sessions` | Bearer | No | Make a tab session. The response has `id` and `token`. | `open_terminal/browser/router.py:182-206` |
| GET | `/browser/sessions/{sid}` | Bearer | No | Session data, with the token | `open_terminal/browser/router.py:209-211` |
| PATCH | `/browser/sessions/{sid}` | Bearer | No | Change the session | `open_terminal/browser/router.py:214-252` |
| DELETE | `/browser/sessions/{sid}` | Bearer | No | Close the session | `open_terminal/browser/router.py:255-259` |
| GET | `/browser/tabs` | Bearer | Yes | List tabs | `open_terminal/browser/router.py:568-595` |
| GET | `/browser/snapshot` | Bearer | Yes | Accessibility tree | `open_terminal/browser/router.py:598-612` |
| GET | `/browser/screenshot` | Bearer | Yes | PNG image | `open_terminal/browser/router.py:615-630` |
| POST | `/browser/action` | Bearer | Yes | Input and navigation actions | `open_terminal/browser/router.py:861-907` |
| GET | `/browser/view/{sid}/{token}/runtime.js` | Path token | No | Script for proxied pages | `open_terminal/browser/router.py:300-307` |
| GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS | `/browser/view/{sid}/{token}/frame/{scheme}/{host}/{path}` | Path token | No | Get a remote page from inside the container | `open_terminal/browser/router.py:310-405` |
| WebSocket | `/browser/view/{sid}/{token}/ws` | Path token | No | Relay to a remote WebSocket | `open_terminal/browser/router.py:408-488` |
| WebSocket | `/browser/view/{sid}/{token}/stream` | Path token | No | Screen stream and input | `open_terminal/browser/router.py:491-510` |
| WebSocket | `/browser/view/{sid}/{token}/vnc` | Path token | No | VNC byte relay | `open_terminal/browser/router.py:491-510` |

### 3.8 Desktop (5 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| GET | `/desktop` | Bearer | No | Start the desktop and give its data. The first call can be slow. | `open_terminal/desktop/router.py:455-466` |
| GET | `/desktop/screenshot` | Bearer | Yes | PNG image | `open_terminal/desktop/router.py:469-483` |
| POST | `/desktop/action` | Bearer | Yes | Pointer and keyboard actions | `open_terminal/desktop/router.py:592-637` |
| WebSocket | `/desktop/stream` | First message | No | Display stream (binary) | `open_terminal/desktop/router.py:640-665` |
| WebSocket | `/desktop/audio` | First message | No | Audio stream (binary) | `open_terminal/desktop/router.py:640-665` |

### 3.9 Agents (5 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| GET | `/agents` | Bearer | No | Find installed agent programs. This can take 20 seconds for each agent. | `open_terminal/agents/router.py:16-22` |
| POST | `/agents/{provider}/sessions` | Bearer | No | Start an agent. `X-Session-Id` is mandatory. | `open_terminal/agents/router.py:50-98` |
| GET | `/agents/sessions/{sid}` | Bearer | No | Session state | `open_terminal/agents/router.py:101-105` |
| DELETE | `/agents/sessions/{sid}` | Bearer | No | Stop the agent | `open_terminal/agents/router.py:108-113` |
| WebSocket | `/agents/sessions/{sid}/connect` | First message | No | JSON line relay to the agent. One message can have 16 MiB. | `open_terminal/agents/router.py:116-202` |

### 3.10 Skills (2 routes)

| Method | Path | Auth | Tool | Function | Reference |
|---|---|---|---|---|---|
| GET | `/skills` | Bearer | No | List skills in the home directory | `main.py:639-669` |
| GET | `/skills/read` | Bearer | No | Read one skill | `main.py:672-696` |

### 3.11 Git (11 routes)

All Git routes use Bearer authentication. They are not tools. They start `git` or `gh` as a subprocess.

| Method | Path | Function | Reference |
|---|---|---|---|
| GET | `/git/status` | State of the working tree | `main.py:765-768` |
| GET | `/git/diff` | Difference of one file | `main.py:771-775` |
| GET | `/git/log` | History | `main.py:778-781` |
| GET | `/git/show` | Show a reference | `main.py:784-787` |
| GET | `/git/branches` | Branch list | `main.py:790-793` |
| GET | `/git/worktrees` | Worktree list | `main.py:796-799` |
| GET | `/git/pr/{view}` | Pull request views | `main.py:802-806` |
| POST | `/git/pr/{action}` | Pull request actions | `main.py:809-829` |
| GET | `/git/issue/{view}` | Issue views | `main.py:832-836` |
| POST | `/git/issue/{action}` | Issue actions | `main.py:839-850` |
| POST, PATCH, DELETE | `/git/{action}` | Stage, commit, branch, fetch, pull, push, and other actions | `main.py:853-884` |

The `fetch`, `pull`, and `push` actions use the network and can take a long time.

### 3.12 Events (1 route)

| Method | Path | Auth | Function | Reference |
|---|---|---|---|---|
| WebSocket | `/events` | First message | File change and port change messages | `main.py:724-744` |

After authentication, the client must send a `subscribe` message in 10 seconds (`open_terminal/utils/events.py:142-161`).

### 3.13 MCP

The HTTP server has no MCP route. MCP is a different command, `open-terminal mcp` (`cli.py:269-321`).
The container images do not install the MCP package (`Dockerfile:86`). Warren has no MCP work to do.

## 4. Authentication

### 4.1 The API key

- If no key is set, the `run` command makes a random key and prints it to standard output (`cli.py:138-140`, `cli.py:173-175`). Thus the key is in the pod log.
- The server does not start without a key (`main.py:59-65`). There is no mode without authentication.
- The comparison of the key uses constant time (`dependencies.py:17-22`).
- A bad key gives status 401 with the body `{"detail":"Invalid API key"}` (`dependencies.py:30-31`).

### 4.2 Routes that do not use the API key

- `/health` and `/api/config`.
- `/docs`, `/redoc`, and `/openapi.json`. The OpenAPI description contains the operating system, kernel, host name, user, and shell (`main.py:78-86`, `main.py:140-143`).
- All routes below `/browser/view/{sid}/{token}/`. The token in the path is the credential.

### 4.3 WebSocket authentication

For `/events`, `/execute`, `/execute/{process_id}/status`, and `/api/terminals/{session_id}` (`main.py:747-762`):

1. The server accepts the WebSocket. It does not examine the headers for a credential.
2. The server waits a maximum of 10 seconds for the first message.
3. The message must be a JSON object with `type` = `auth` and `token` = the API key.
4. If the message is incorrect or late, the server closes with code 4001.
5. If the session ID is not a string, the server closes with code 4000.
6. The server sends no reply when the authentication is correct. The stream starts.

The first message can also contain `chat_id`. The server uses it as the session ID when the `X-Session-Id` header is absent (`main.py:732`, `main.py:1766-1769`, `main.py:2547`).
The terminal WebSocket also reads `resume`, `cursor`, `flow_control`, and `process_info` from the first message (`main.py:2574-2596`).

Close codes for each WebSocket:

| WebSocket | Codes | Reference |
|---|---|---|
| `/events` | 4001 authentication, 4000 bad subscription, 4003 path denied, 1011 failure | `open_terminal/utils/events.py:131-168` |
| `/execute` | 4001, 4000, 1000, 1011 | `main.py:1733-1788` |
| `/execute/{process_id}/status` | 4001, 4000, 4004 not found, 1000, 1011 | `main.py:1791-1819` |
| `/api/terminals/{session_id}` | 4004 before accept, 4001, 4000, 4004 owner mismatch | `main.py:2532-2550` |
| `/agents/sessions/{sid}/connect` | 4401 authentication, 4409 a second client, 4400 other errors, 4004 session removed | `open_terminal/agents/router.py:116-134`, `:193-195` |
| `/desktop/stream`, `/desktop/audio` | 4001 authentication, 4004 no audio, 1012 desktop closes, 1011 other | `open_terminal/desktop/router.py:640-665` |
| `/browser/view/.../ws`, `/stream`, `/vnc` | 4004 before accept, 4000 a different viewer took the stream, 1011 failure | `open_terminal/browser/router.py:408-510` |

Result for warren: a key in the upgrade request header has no effect. To hide the key of a pod from the client, warren must replace the `token` value in the first message.

## 5. Identity headers

### 5.1 Inputs that the server reads

| Input | Read | Use |
|---|---|---|
| `X-User-Id` header | Yes | Owner key. Refer to section 5.2. |
| `X-Session-Id` header | Yes | Owner key and working directory key. Refer to section 5.3. |
| `X-Workspace-Id` header | Yes | Lets a chat use the terminals and browser tabs of a workspace of the same user (`main.py:2388-2395`). |
| `X-Workspace-Directory` header | Yes | A second root directory for skills (`main.py:650-653`). |
| `chat_id` in the first WebSocket message | Yes | Replacement for `X-Session-Id` on a WebSocket. |
| `user_id` query parameter | No | No handler reads it. |
| `X-Terminal-*` headers | No | The package does not contain these names. |

The security policy of the project says that the caller gives `X-User-Id`, and that Open Terminal does not authenticate users. A caller that has the API key can give any user ID (`SECURITY.md:25`).

### 5.2 `X-User-Id`

- In single-user mode (the default), the header is optional. It does not select an operating system user (`dependencies.py:40-41`).
- In the two modes, the server keeps the value as the owner of each process, terminal, notebook, browser session, agent session, and desktop. A request with a different value gets status 404 (`main.py:473-481`, `main.py:2372-2376`).
- When the header is absent, browser sessions and agent sessions use the owner name `single` (`open_terminal/browser/router.py:118-119`, `open_terminal/agents/router.py:29`).

### 5.3 `X-Session-Id`

- It is the key of the working directory for each chat. The default life is 7 days (`main.py:396-423`, `env.py:247-252`).
- It is the second part of the owner key for processes, terminals, browser sessions, and agent sessions.
- It is mandatory for `POST /agents/{provider}/sessions` (`open_terminal/agents/router.py:65-67`).

### 5.4 Multi-user mode

- `OPEN_TERMINAL_MULTI_USER` turns the mode on. The default is off (`env.py:199-202`).
- In this mode, the server makes one Linux account for each `X-User-Id` value with `useradd` and `sudo` (`open_terminal/utils/user_isolation.py:82-140`).
- The account name uses only the first 8 characters of the ID. Two IDs with the same first 8 characters get the same account (`open_terminal/utils/user_isolation.py:58-79`).
- The project says that this mode is not a security boundary (`SECURITY.md:30-39`, `docs/deployment.md:185-187`).
- The mode needs the full image and root or `sudo` (`open_terminal/utils/user_isolation.py:35-55`).

### 5.5 Results for warren

1. Warren must keep multi-user mode off. One pod for each scope key gives the isolation.
2. Warren must set `X-User-Id` from the identity that it authenticated. It must not copy a client value.
3. Warren must send the same `X-User-Id` and `X-Session-Id` on the create request, on subsequent requests, and on the WebSocket upgrade. If not, the server replies with status 404 or close code 4004.
4. Warren can pass `X-Session-Id`, `X-Workspace-Id`, and `X-Workspace-Directory` without changes. They select data only inside one pod.
5. Warren uses `X-Terminal-Template-Id`, `X-Terminal-Chat-Id`, `X-Terminal-Model-Id`, and `user_id` for routing. Open Terminal ignores them. Warren can remove them before it calls the pod.

## 6. Health and readiness

- `GET /health` is the only health route. It needs no authentication (`main.py:489-497`).
- There is no readiness route. Inference: the route accepts GET only, and a HEAD probe gets status 405.
- The server does not listen before the setup script is complete (`cli.py:195-208`, `cli.py:222-225`). A start-up probe must allow for this time.
- A setup script failure does not stop the server. `GET /system/info` shows the failure (`main.py:530-532`).

## 7. Paths that need special care

### 7.1 Port proxy: `/proxy/{port}/{path}`

Reference: `main.py:2064-2132`.

- The port must be in the list of ports that the caller can see. If not, the status is 404.
- The target is always `http://localhost:{port}` with the same query string.
- The server removes `host`, `transfer-encoding`, `connection`, and `authorization` from the request. It passes all other headers, and thus also cookies and `X-User-Id`, to the application of the user.
- The server sends the request body and the response body as streams.
- The server does not follow redirects and does not change the `Location` header.
- Time limits: 5 seconds to connect, 300 seconds for the response.
- The route does not support WebSockets.
- The route needs a `/` after the port number.

Result for warren: an application in a sandbox gets the headers that warren sends to the pod. Warren must not send a credential of the user or of Open WebUI to the pod.

### 7.2 Browser view: `/browser/view/{sid}/{token}/...`

Reference: `open_terminal/browser/router.py:142-147`, `:282-510`.

- `sid` has 32 hexadecimal characters. `token` is a random string. `POST /browser/sessions` returns the two values.
- The path token is the only credential. The server does not examine the `Authorization` header.
- The `frame` route gets any `http` or `https` URL from inside the pod (`open_terminal/browser/rewrite.py:30-48`). It can connect to localhost and to cluster addresses unless a network policy prevents this.
- The server removes cookies, `authorization`, and all `x-*` headers from the request to the remote site (`open_terminal/browser/router.py:335-350`).
- The server returns a remote redirect with a relative `Location` (`open_terminal/browser/router.py:366-367`). A proxy must pass status 3xx and must not follow it.
- The server adds its own content security policy to responses (`open_terminal/browser/router.py:289-297`). A proxy must not replace it.
- The CORS code examines the literal path prefix `/browser/view/` (`open_terminal/browser/router.py:64-69`). The path that the pod gets must start at the root.
- The `/vnc` and `/audio` routes are selected by the end of the path (`open_terminal/browser/router.py:502`, `open_terminal/desktop/router.py:654`).

Results for warren:

1. A proxy that logs full URLs also logs these tokens. Warren must remove the token from log lines.
2. Warren must not change, merge, or encode again the path segments after `/frame/`.
3. Open WebUI replaces the token with its own token before the browser sees it (`protocol.md`, section 8.1). Warren gets the initial token from Open WebUI, together with the connection key or session credential.

### 7.3 Redirects

- No handler in the package sends a redirect.
- Inference: FastAPI sends status 307 for a path with a missing or extra final `/`. The `Location` value is an absolute URL from the `Host` header. Warren must change this header or prevent the redirect.

## 8. Container facts

### 8.1 Images

Image name: `ghcr.io/open-webui/open-terminal` (`README.md:22`). Tags: `latest`, `agents`, `cuda`, `slim`, `alpine`, `openshift` (`docs/deployment.md:12-19`).

| Property | Full (`latest`, `agents`, `cuda`) | `slim` | `alpine` | `openshift` |
|---|---|---|---|---|
| Base image | `python:3.12.16` | `python:3.12.16-slim` | `python:3.12.16-alpine` | `python:3.12.16-slim` |
| Default user | `user`, by name (`Dockerfile:93`) | None. Starts as root. | None. Starts as root. | `1001`, by number (`Dockerfile.openshift:52`) |
| User ID | Not set in the file (`Dockerfile:91`) | 1000 | 1000 | 1001, group 0 |
| `sudo` | Yes, with no password (`Dockerfile:91`) | No. Uses `gosu`. | No. Uses `su-exec`. | No |
| Working directory | `/home/user` | `/app` | `/app` | `/home/user` |
| Port | 8000 | 8000 | 8000 | 8000 |
| Entry point | `tini`, then `entrypoint.sh` | `tini`, then `entrypoint-slim.sh` | `tini`, then `entrypoint-slim.sh` | `tini`, then `entrypoint-openshift.sh` |
| Browser, desktop, notebooks | Yes | No | No | No |
| Multi-user mode | Yes | No | No | No |

### 8.2 What the entry points do

- Full image: the entry point runs as `user`. Without the optional variables, the only privileged call is a `sudo chown` of the home directory. A failure of this call is permitted (`entrypoint.sh:28-35`). The subsequent `mkdir` in the home directory must succeed (`entrypoint.sh:37-45`).
- `slim` and `alpine`: the entry point is written to start as root and then change to user 1000 (`entrypoint-slim.sh:61-70`). Inference: it fails when the container starts as a user that is not root.
- `openshift`: the entry point needs no privilege and no network (`entrypoint-openshift.sh:41-61`).

### 8.3 Variables that need privilege

| Variable | What it needs | Reference |
|---|---|---|
| `OPEN_TERMINAL_PACKAGES`, `OPEN_TERMINAL_PIP_PACKAGES`, `OPEN_TERMINAL_NPM_PACKAGES` | `sudo`, a writable root file system, and network access at each start | `entrypoint.sh:57-82` |
| `OPEN_TERMINAL_ALLOWED_DOMAINS` | Root and `NET_ADMIN` for `iptables` and `dnsmasq` | `entrypoint.sh:84-182` |
| `OPEN_TERMINAL_MULTI_USER` | Root or `sudo` | `open_terminal/utils/user_isolation.py:35-55` |
| A mounted Docker socket | `sudo` | `entrypoint.sh:47-55` |

Warren must reject a policy that sets these variables.

### 8.4 Paths that the server writes

| Path | Cause | Reference |
|---|---|---|
| Home directory: `.bashrc`, `.profile`, `.local/bin` | Entry point, at each start | `entrypoint.sh:39-45` |
| Home directory: `.local/state/open-terminal/logs` | Each `POST /execute` | `main.py:1869`, `env.py:48-61` |
| Home directory: `.open-terminal/setup.json` and `setup.log` | Setup script | `open_terminal/utils/setup.py:46-58` |
| Home directory: browser profile | Browser in `chrome` mode | `open_terminal/browser/chrome.py:323-327` |
| Temporary directory: response bodies | `/files/read`, `POST /execute`, `GET /execute/{id}/status` | `open_terminal/utils/fs.py:121`, `open_terminal/utils/log.py:242` |
| Temporary directory: ZIP files, previews, search data | File routes | `main.py:1660`, `open_terminal/utils/search.py:36` |
| Temporary directory: desktop data and X lock files | Desktop and browser | `open_terminal/desktop/router.py:114` |

In the default configuration the server writes to no other path.

In single-user mode, "home" is the working directory of the process and not `$HOME` (`open_terminal/utils/fs.py:160-162`). The pod must set the working directory to the mount path of the volume.

### 8.5 Results for the restricted Pod Security Standard

1. Writable mounts: the home directory (PVC) and `/tmp` (`emptyDir`). The root file system can be read-only.
2. The `openshift` image is the best fit. It has a numeric user, no `sudo`, and an entry point that needs no privilege. It has no browser and no desktop.
3. Full image: Inference: Kubernetes cannot verify `runAsNonRoot` for a user name. The pod must set `runAsUser`. The implementation must read the real user ID from the image.
4. Full image: the Python interpreter of the launcher has a file capability (`Dockerfile:83-89`). Inference: with `allowPrivilegeEscalation: false` the launcher starts normally. A test must confirm this.
5. Full image: `sudo` stays in the image. `allowPrivilegeEscalation: false` makes it ineffective.
6. `slim` and `alpine`: the pod must replace the entry point and set the working directory.
7. The volume must be writable for the user ID of the pod. `fsGroup` does this.
8. The API key comes from a Secret. Inference: a shell in the pod operates as the same user as the server and can read the key of its own pod. Thus each pod must have a different key, and the key gives access to that pod only.
9. The browser starts with `--no-sandbox` in all images (`Dockerfile:95`).

## 9. Proxy requirements

| Subject | Requirement | Reference |
|---|---|---|
| Path prefix | The server expects to be at `/`. It has no setting for a path prefix or for forwarded headers. Warren must send root paths to the pod. | `cli.py:210-225` |
| Origin and Host | The server does no Origin check and no Host check. | `main.py:231-237` |
| CORS | The default is `*` with credentials. This is not important when only warren can connect to the pod. | `env.py:44-47` |
| Long requests | `/execute` routes wait a maximum of 300 seconds. | `main.py:1835-1840` |
| WebSockets | They stay open with no time limit, and can be silent for a long time. | Section 3 |
| Message size | A maximum of 16 MiB for agent and browser relay messages. | `open_terminal/agents/sessions.py:45`, `open_terminal/browser/router.py:448` |
| Body size | Uploads and downloads have no size limit. Warren must not buffer them. | Section 3.3 |
| Server-sent events | Open Terminal sends none. An application behind the port proxy can send them. | Section 7.1 |
| Keep-alive | Inference: the keep-alive time of the server is short by default. Warren must set `OPEN_TERMINAL_UVICORN_TIMEOUT_KEEP_ALIVE` or not use a connection again after an idle period. | `env.py:214-220` |

## 10. Differences between the Open Terminal routes and the Open WebUI proxy

Open WebUI does not give a browser route for each Open Terminal WebSocket. Refer to `protocol.md`, section 4.2.

| Open Terminal WebSocket | Browser route in Open WebUI | Other caller |
|---|---|---|
| `/events` | Yes | |
| `/execute` | Yes | |
| `/execute/{process_id}/status` | Yes | |
| `/api/terminals/{session_id}` | Yes | |
| `/desktop/stream`, `/desktop/audio` | Yes | |
| `/browser/view/{sid}/{token}/ws`, `/stream`, `/vnc` | Yes | |
| `/agents/sessions/{sid}/connect` | No | The Open WebUI backend connects directly. Refer to `protocol.md`, section 4.7. |
