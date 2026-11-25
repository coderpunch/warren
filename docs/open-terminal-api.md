# Open Terminal API: the routes that warren must proxy

Status: research record. This document contains no design decisions that are final.

## 1. Sources and rules

| Item | Value |
|---|---|
| Repository | `github.com/open-webui/open-terminal` |
| Version | `0.18.1` (`pyproject.toml:3`) |
| Commit | `3e8ffa2fee3a1701d69b5f232b5145dda76aa02c` (2026-11-25) |
| License | MIT (`pyproject.toml:5`) |

All file paths in this document are relative to the root of that repository.
A reference such as `main.py:489` means line 489 of `open_terminal/main.py`. The same rule applies to `cli.py`, `env.py`, `config.py`, and `dependencies.py`.
All other references give the full path.

This document uses two labels for facts that the repository does not state:

- "Test result:" a small test program gave this result. The test used the FastAPI and uvicorn versions that `pyproject.toml` permits. The test did not start Open Terminal.
- "Inference:" a conclusion from the behaviour of Linux or Kubernetes. A test on a cluster must make sure that it is correct before the implementation uses it.

The repository refers to the enterprise orchestrator only as a recommendation for one container for each user (`README.md:138`, `SECURITY.md:41`, `docs/deployment.md:203-208`).
It does not describe the API of that product.

In this document, "the server" is the Open Terminal process. "The pod" is the Kubernetes pod that contains it.

## 2. Summary

| Item | Value | Reference |
|---|---|---|
| Listen address | `0.0.0.0:8000`. Only the flags `--host` and `--port` or a TOML file can change it. | `cli.py:27-28`, `cli.py:109-110` |
| API key sources | The flag `--api-key`, `OPEN_TERMINAL_API_KEY`, `OPEN_TERMINAL_API_KEY_FILE` (a file path), and `api_key` in a TOML file | `cli.py:117-136`, `env.py:18-43` |
| HTTP authentication | `Authorization: Bearer <key>`. No query parameter and no cookie is possible. | `dependencies.py:14-31` |
| WebSocket authentication | The first text message: a JSON object with `type` = `auth` and `token` = the key. The time limit is 10 seconds. | `main.py:747-762` |
| Health check | `GET /health`. No authentication. Status 200, body `{"status":"ok"}`. | `main.py:489-497` |
| Capability data | `GET /api/config`. No authentication. | `main.py:505-522` |
| Identity headers that the server reads | `X-User-Id`, `X-Session-Id`, `X-Workspace-Id`, `X-Workspace-Directory` | Section 5 |
| Identity inputs that the server ignores | The `user_id` query parameter and all `X-Terminal-*` headers | Section 5 |
| Number of routes | 81 registrations: 71 HTTP and 10 WebSocket | Section 3 |
| Paths that must be writable | `$HOME` and the temporary directory | Section 8.4 |

### 2.1 The TOML configuration file

- The server reads `/etc/open-terminal/config.toml` and then `$XDG_CONFIG_HOME/open-terminal/config.toml` (`config.py:19-27`).
- The default value of `$XDG_CONFIG_HOME` is `~/.config`. Thus the second file is in the home directory, on the volume of the user.
- A user, or a model with a shell, can write this file. The file can change the port, the host, and each setting that has no environment variable or flag.
- The flag `--isolated` or the variable `OPEN_TERMINAL_ISOLATED` makes the server ignore the two files (`config.py:85`, `cli.py:59`).

Results for warren:

1. The pod must set `OPEN_TERMINAL_ISOLATED=true`.
2. The pod must give `--host` and `--port` on the command line.

## 3. Route inventory

The count of 81 includes each `@app`, `@router`, and `add_api_route` registration in the package.
Three registrations accept more than one method. Thus there are 85 pairs of HTTP method and path, and 10 WebSocket paths.
The count does not include the FastAPI routes `/docs`, `/redoc`, and `/openapi.json`. These routes are public (`main.py:223-228`).
An independent count gave the same numbers.

Columns:

- "Auth": `Bearer` is the API key header. `First message` is the WebSocket message of section 4.3. `Path token` is the token of section 7.2. `None` means no authentication.
- "Tool": `Yes` means that the route is in the OpenAPI document. Open WebUI gives these routes to a model as tools.

In three routes, `{path}` is a parameter that contains all remaining path segments. It can be empty and can contain `/`.
These routes are `/files/serve/{path}`, `/proxy/{port}/{path}`, and the browser `frame` route.

Some routes exist only when a setting is on:

| Routes | Condition | Reference |
|---|---|---|
| `/system` | `OPEN_TERMINAL_ENABLE_SYSTEM_PROMPT` is on (default on) | `main.py:536` |
| `/info` | `OPEN_TERMINAL_INFO` is not empty (default empty) | `main.py:548` |
| `/api/terminals...` | `OPEN_TERMINAL_ENABLE_TERMINAL` is on (default on) | `main.py:2139` |
| `/notebooks...` | `OPEN_TERMINAL_ENABLE_NOTEBOOKS` is on (default on) and the image has the packages `nbclient` and `ipykernel` | `main.py:2740-2743` |
| `/browser...` | `OPEN_TERMINAL_ENABLE_BROWSER` is on (default on) | `main.py:2746-2748` |
| `/desktop...` | `OPEN_TERMINAL_ENABLE_DESKTOP` is on (default on). The routes are in the OpenAPI document only when the image has the desktop programs. | `main.py:2750-2752` |

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
| POST | `/execute` | Bearer | Yes | Start a command | Query `wait` (0 to 300 seconds), `tail`. The request can wait. | `main.py:1822-1896` |
| GET | `/execute/{process_id}/status` | Bearer | Yes | Output and state | Query `wait`, `offset`, `tail`. The request can wait. | `main.py:1899-1948` |
| POST | `/execute/{process_id}/input` | Bearer | Yes | Write to standard input | | `main.py:1951-1983` |
| DELETE | `/execute/{process_id}` | Bearer | Yes | Stop the process | | `main.py:1986-2009` |
| WebSocket | `/execute` | First message | No | Process list updates | Server to client only | `main.py:1772-1788` |
| WebSocket | `/execute/{process_id}/status` | First message | No | Output updates | Query `offset`, `tail`. If the server cannot find the process, it closes with code 4004. | `main.py:1791-1819` |

If `OPEN_TERMINAL_EXECUTE_TIMEOUT` is set, a request without `wait` waits for that time. That time has no upper limit (`main.py:1879-1880`, `main.py:1932-1933`, `env.py:91-97`).

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

File downloads ignore HTTP range requests (`open_terminal/utils/fs.py:31-41`).

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

- Terminal input and terminal output are binary messages. Control data is JSON text messages (`main.py:2681-2709`).
- The session ID is the first 8 characters of a random UUID (`main.py:2232`). It has 32 bits. It is an identifier and not a secret.
- If the session ID is unknown, the server closes with code 4004 before it accepts the WebSocket (`main.py:2532-2540`).
- Test result: the client then gets HTTP status 403 with an empty body. The client does not see the code 4004.
- After authentication, the server compares `X-User-Id` and the chat ID with the values of the session. If they are different, it closes with code 4004 (`main.py:2545-2550`).
- A shell continues to operate when no viewer is connected (`main.py:2523-2527`).
- The server closes with code 4000 when more than 64 input messages or more than 1 MiB of input wait in its queue (`main.py:2652`, `main.py:2675-2677`).

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
| GET | `/ports` | Bearer | No | TCP ports on which the child processes of the server listen | `main.py:2019-2061` |
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
| GET | `/desktop/screenshot` | Bearer | Yes, if the image has the desktop programs | PNG image | `open_terminal/desktop/router.py:469-483` |
| POST | `/desktop/action` | Bearer | Yes, if the image has the desktop programs | Pointer and keyboard actions | `open_terminal/desktop/router.py:592-637` |
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

After authentication, the client must send a `subscribe` message in a maximum of 10 seconds (`open_terminal/utils/events.py:142-161`).

### 3.13 MCP

The HTTP server has no MCP route. MCP is a different command, `open-terminal mcp` (`cli.py:269-321`).
The container images do not install the MCP package (`Dockerfile:86`). Warren has no MCP work to do.

## 4. Authentication

### 4.1 The API key

- The server looks for the key in this sequence: the flag, the environment variable, the file variable, the TOML file (`cli.py:117-136`).
- To set the environment variable and the file variable at the same time is an error (`env.py:18-43`).
- If the key is empty after these steps, the `run` command makes a random key and prints it to standard output (`cli.py:138-140`, `cli.py:173-175`).
- Thus an empty Secret value causes no error. The pod starts with a key that warren does not know, and `/health` replies with status 200.
- The server does not start without a key (`main.py:59-65`). There is no mode without authentication.
- The comparison of the key uses constant time (`dependencies.py:17-22`).
- A bad key gives status 401 with the body `{"detail":"Invalid API key"}` (`dependencies.py:30-31`).

Result for warren: warren must make sure that the key of a pod works. A request to `/health` is not sufficient. A request with the key is necessary.

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

The first message can also contain `chat_id`. On these four routes the server uses it as the session ID when the `X-Session-Id` header is absent (`main.py:732`, `main.py:1766-1769`, `main.py:2547`).
The terminal WebSocket also reads `resume`, `cursor`, `flow_control`, and `process_info` from the first message (`main.py:2574-2596`).

The agent WebSocket has different rules (`open_terminal/agents/router.py:116-134`):

- The first message has `type` = `auth`, `token`, and `cursor`. The server does not read `chat_id`.
- `X-User-Id` and `X-Session-Id` must be in the upgrade request. The server compares them with the session (`open_terminal/agents/router.py:25-32`).
- Code 4401 means a bad token. Code 4400 means a late message, a bad message, an owner mismatch, or an unknown session.

Close codes for each WebSocket:

| WebSocket | Codes | Reference |
|---|---|---|
| `/events` | 4001 authentication, 4000 bad subscription, 4003 path denied, 1011 failure | `open_terminal/utils/events.py:131-168` |
| `/execute` | 4001, 4000, 1000, 1011 | `main.py:1733-1788` |
| `/execute/{process_id}/status` | 4001, 4000, 4004 not found, 1000, 1011 | `main.py:1791-1819` |
| `/api/terminals/{session_id}` | HTTP status 403 for an unknown session. 4001 authentication. 4004 owner mismatch. 4000 for a bad session ID, a bad `cursor`, a bad `ack`, or too much input. | `main.py:2532-2550`, `main.py:2576-2578`, `main.py:2675-2688` |
| `/agents/sessions/{sid}/connect` | 4401 bad token, 4409 a second client, 4400 other errors, 4004 session removed | `open_terminal/agents/router.py:116-134`, `:193-195` |
| `/desktop/stream`, `/desktop/audio` | 4001 bad token, 4004 no audio, 1012 desktop closes, 1011 all other causes. A late or bad first message gives 1011. | `open_terminal/desktop/router.py:640-665` |
| `/browser/view/.../ws`, `/stream`, `/vnc` | HTTP status 403 for a bad token. 4000 a different viewer took the stream. 4004 session closed. 1011 failure. | `open_terminal/browser/router.py:408-510` |

Results for warren:

1. A key in the header of the upgrade request has no effect. To hide the key of a pod from the client, warren must replace the `token` value in the first message.
2. Warren must send a correct close code to its client when the pod replies with status 403 to an upgrade request.

## 5. Identity inputs

### 5.1 Inputs that the server reads

| Input | Read | Use |
|---|---|---|
| `X-User-Id` header | Yes | Owner key. Refer to section 5.2. |
| `X-Session-Id` header | Yes | Owner key and working directory key. Refer to section 5.3. |
| `X-Workspace-Id` header | Yes | Lets a chat use the terminals and browser tabs of a workspace of the same user (`main.py:2388-2395`). |
| `X-Workspace-Directory` header | Yes | A second root directory for skills (`main.py:650-653`). |
| `chat_id` in the first WebSocket message | Yes | Replacement for `X-Session-Id` on the four routes of section 4.3. |
| A `chat` message on an open terminal WebSocket | Yes | Sets the chat ID of the terminal when the terminal has none (`main.py:2700-2709`). |
| `chat_id` in the body of `PATCH /browser/sessions/{sid}` | Yes | Sets the chat ID of the browser session when the session has none (`open_terminal/browser/router.py:172`). |
| `user_id` query parameter | No | No handler reads it. |
| `X-Terminal-*` headers | No | The package does not contain these names. |

The security policy of the project says that the caller gives `X-User-Id`, and that Open Terminal does not authenticate users. A caller that has the API key can give any user ID (`SECURITY.md:25`).

### 5.2 `X-User-Id`

- In single-user mode (the default), the header is optional. It does not select an operating system user (`dependencies.py:40-41`).
- In the two modes, the server keeps the value as the owner of each process, terminal, notebook, browser session, and agent session. A request with a different value gets status 404 (`main.py:473-481`, `main.py:2372-2376`).
- When the header is absent, browser sessions and agent sessions use the owner name `single` (`open_terminal/browser/router.py:118-119`, `open_terminal/agents/router.py:29`).
- The desktop has no owner check. `X-User-Id` selects the desktop (`open_terminal/desktop/router.py:414`). A different value starts a second desktop and gives no error.

### 5.3 `X-Session-Id`

- It is the key of the working directory for each chat. The default life is 7 days (`main.py:396-423`, `env.py:247-252`).
- It is the second part of the owner key for processes, terminals, browser sessions, and agent sessions.
- It is mandatory for `POST /agents/{provider}/sessions` (`open_terminal/agents/router.py:65-67`).

### 5.4 Multi-user mode

- `OPEN_TERMINAL_MULTI_USER` turns the mode on. The default is off (`env.py:199-202`).
- In this mode, the server makes one Linux account for each `X-User-Id` value with `useradd` and `sudo` (`open_terminal/utils/user_isolation.py:82-140`).
- The account name uses only the first 8 letters and digits of the ID, in lower case. Different IDs can get the same account (`open_terminal/utils/user_isolation.py:58-79`).
- The project says that this mode is not a security boundary (`SECURITY.md:30-39`, `docs/deployment.md:185-187`).
- The mode needs the full image and root or `sudo` (`open_terminal/utils/user_isolation.py:35-55`).

### 5.5 Results for warren

1. Warren must keep multi-user mode off. One pod for each scope key gives the isolation.
2. Warren must set `X-User-Id` from the identity that it authenticated. It must not copy a client value.
3. Warren must send the same `X-User-Id` and `X-Session-Id` on all requests for one session. This includes the request that makes the session and the WebSocket upgrade.
4. If the values are different, the server replies with status 404 or close code 4004. The desktop routes are an exception: they start a second desktop.
5. Warren can pass `X-Session-Id` without changes. It selects data only inside one pod.
6. Warren must remove `X-Workspace-Id` and `X-Workspace-Directory`. Open WebUI v0.13.4 does not send them (`protocol.md`, section 6.2).
7. Warren uses `X-Terminal-Template-Id`, `X-Terminal-Chat-Id`, `X-Terminal-Model-Id`, and `user_id` to select a sandbox. Open Terminal ignores them. Warren can remove them before it calls the pod.

## 6. Health and readiness

- `GET /health` is the only health route. It needs no authentication (`main.py:489-497`).
- There is no readiness route.
- Test result: a route that accepts only GET replies to HEAD with status 405. A probe must use GET.
- The server does not listen before the setup script is complete (`cli.py:195-208`, `cli.py:222-225`). The time limit of a start-up probe must include this time.
- A setup script failure does not stop the server. `GET /system/info` shows the failure (`main.py:530-532`).

## 7. Paths that need special care

### 7.1 Port proxy: `/proxy/{port}/{path}`

Reference: `main.py:2064-2132`.

- The port must be in the list that `GET /ports` returns. If not, the status is 404.
- The target is always `http://localhost:{port}`.
- The server makes the query string again from the parsed parameters. A middleware removes each parameter that has the value `null` (`open_terminal/utils/limits.py:24-35`).
- The server removes `host`, `transfer-encoding`, `connection`, and `authorization` from the request. It passes all other headers, and thus also cookies and `X-User-Id`, to the application of the user.
- The server removes `transfer-encoding`, `connection`, `content-encoding`, and `content-length` from the response. It passes all other response headers, and thus also `Set-Cookie`.
- The server sends the request body and the response body as streams.
- The server does not follow redirects and does not change the `Location` header.
- Time limits: 5 seconds to connect, and 300 seconds without data. There is no limit for the total time (`main.py:2072-2075`).
- The route does not accept WebSockets.
- Test result: a request to `/proxy/{port}` without a final `/` gets a redirect with status 307. Refer to section 7.3.

Results for warren:

1. An application in a pod gets the headers that warren sends to that pod. Warren must not send a credential of the user or of Open WebUI to the pod.
2. An application in a pod can set response headers. Warren must make a decision about `Set-Cookie` and other response headers from this path.

### 7.2 Browser view: `/browser/view/{sid}/{token}/...`

Reference: `open_terminal/browser/router.py:142-147`, `:282-510`.

- `sid` has 32 hexadecimal characters. `token` is a random string. `POST /browser/sessions` returns the two values.
- The path token is the only credential. The server does not examine the `Authorization` header.
- The `frame` route gets any `http` or `https` URL from inside the pod (`open_terminal/browser/rewrite.py:30-48`). It can connect to localhost and to cluster addresses unless a network policy prevents this.
- The `frame` route operates in all images. It does not need a browser program. Refer to section 8.1.
- The server removes cookies, `authorization`, and all `x-*` headers from the request to the remote site (`open_terminal/browser/router.py:335-350`).
- The server returns a remote redirect with a relative `Location` (`open_terminal/browser/router.py:366-367`). A proxy must pass status 3xx to the client. A proxy must not follow the redirect.
- The server sends remote responses as streams, unless the content is HTML, CSS, or JavaScript (`open_terminal/browser/router.py:396-405`).
- The server adds its own content security policy to responses (`open_terminal/browser/router.py:289-297`). A proxy must not replace it.
- The CORS code examines the literal path prefix `/browser/view/` (`open_terminal/browser/router.py:64-69`). The path that the server gets must start at the root.
- The end of the path selects the `/vnc` and `/audio` routes (`open_terminal/browser/router.py:502`, `open_terminal/desktop/router.py:654`).
- The server removes a browser session that had no activity for 24 hours (`open_terminal/browser/router.py:74`).

Results for warren:

1. A proxy that logs full URLs also logs these tokens. Warren must remove the token from log lines.
2. Warren must not change the path segments after `/frame/`. Warren must not merge them. Warren must not encode them again.
3. Open WebUI replaces the token with its own token before the browser sees it (`protocol.md`, section 8.1). Open WebUI sends the token of the server to warren, together with the stored `Authorization` value.
4. The egress rules of a sandbox must block cluster addresses and the metadata address of the cloud provider.

### 7.3 Redirects

- No handler makes a redirect of its own. The port proxy and the browser `frame` route pass a redirect from the remote application to the caller.
- Test result: FastAPI sends status 307 when a path has a missing or extra final `/` and the other form is a registered route.
- Test result: the `Location` value is an absolute URL. FastAPI makes it from the `Host` header and the scheme `http`.
- Test result: for a WebSocket upgrade, the reply is status 403 and not a redirect.

Result for warren: warren must change the `Location` header of such a reply to a relative path.

## 8. Container facts

### 8.1 Images

Image name: `ghcr.io/open-webui/open-terminal` (`README.md:22`). Tags: `latest`, `agents`, `cuda`, `slim`, `alpine`, `openshift` (`docs/deployment.md:12-19`).

| Property | Full (`latest`, `agents`, `cuda`) | `slim` | `alpine` | `openshift` |
|---|---|---|---|---|
| Base image | `python:3.12.16` | `python:3.12.16-slim` | `python:3.12.16-alpine` | `python:3.12.16-slim` |
| Default user | `user`, by name (`Dockerfile:93`) | None. The container starts as root. | None. The container starts as root. | `1001`, by number (`Dockerfile.openshift:52`) |
| User ID | Not set in the file (`Dockerfile:91`) | 1000 | 1000 | 1001, group 0 |
| `sudo` | Yes, with no password (`Dockerfile:91`) | No. Uses `gosu`. | No. Uses `su-exec`. | No |
| `$HOME` | `/home/user` | `/home/user` | `/home/user` | `/home/user` |
| Working directory | `/home/user` | `/app` | `/app` | `/home/user` |
| Port | 8000 | 8000 | 8000 | 8000 |
| Entry point | `tini`, then `entrypoint.sh` | `tini`, then `entrypoint-slim.sh` | `tini`, then `entrypoint-slim.sh` | `tini`, then `entrypoint-openshift.sh` |
| Browser routes in proxy mode (section 7.2) | Yes | Yes | Yes | Yes |
| Browser in Chrome mode, desktop, notebooks | Yes | No | No | No |
| Multi-user mode | Yes | No | No | No |

The browser routes are on in all images, because `OPEN_TERMINAL_ENABLE_BROWSER` is on by default (`env.py:151-154`). Only Chrome mode needs a browser program.

### 8.2 What the entry points do

- Full image: the entry point starts as `user`. Without the optional variables, the only privileged call is a `sudo chown` of the home directory. The entry point ignores a failure of this call (`entrypoint.sh:28-35`).
- Full image: the subsequent `mkdir` in `$HOME` must be successful. If not, the container stops (`entrypoint.sh:37-45`).
- `slim` and `alpine`: the entry point starts as root and then changes to user 1000 with `gosu` or `su-exec` (`entrypoint-slim.sh:61-70`).
- Inference: the `slim` and `alpine` entry point fails when the container starts as a user that is not root.
- `openshift`: the entry point needs no privilege and no network (`entrypoint-openshift.sh:41-61`).

### 8.3 Variables that need privilege

This table is for the full image.

| Variable | What it needs | Reference |
|---|---|---|
| `OPEN_TERMINAL_PACKAGES` | `sudo`, a writable root file system, and network access at each start | `entrypoint.sh:57-62` |
| `OPEN_TERMINAL_PIP_PACKAGES`, `OPEN_TERMINAL_NPM_PACKAGES` | A writable package directory and network access at each start. `sudo` only in multi-user mode. | `entrypoint.sh:64-82` |
| `OPEN_TERMINAL_ALLOWED_DOMAINS` | Root and `NET_ADMIN` for `iptables` and `dnsmasq` | `entrypoint.sh:84-182` |
| `OPEN_TERMINAL_MULTI_USER` | Root or `sudo` | `open_terminal/utils/user_isolation.py:35-55` |
| A mounted Docker socket | `sudo` | `entrypoint.sh:47-55` |

The `slim` and `alpine` entry points ignore the three package variables. The `openshift` entry point ignores the first four variables and stops if multi-user mode is on (`entrypoint-openshift.sh:41-49`).

Warren must reject a policy that sets these variables. Warren must also reject `OPEN_TERMINAL_API_KEY`, `OPEN_TERMINAL_API_KEY_FILE`, and `OPEN_TERMINAL_EXECUTE_TIMEOUT` above its proxy time limit.

### 8.4 Paths that the server writes

Open Terminal uses two different directories that it calls "home":

- `$HOME`. The entry points and the log directory use it.
- The working directory of the process. In single-user mode, the file routes and the setup script use it as the home directory (`open_terminal/utils/fs.py:160-162`).

| Path | Base | Cause | Reference |
|---|---|---|---|
| `.bashrc`, `.profile`, `.local/bin` | `$HOME` | Entry point, at each start | `entrypoint.sh:39-45` |
| `.local/state/open-terminal/logs` | `$HOME` or `$XDG_STATE_HOME` | Each `POST /execute` | `main.py:1869`, `env.py:48-61` |
| `.open-terminal/setup.json` and `setup.log` | Working directory | Setup script | `open_terminal/utils/setup.py:46-58` |
| Browser profile | Working directory | Browser in Chrome mode | `open_terminal/browser/chrome.py:323-327` |
| Response bodies | Temporary directory | `/files/read`, `POST /execute`, `GET /execute/{id}/status` | `open_terminal/utils/fs.py:121`, `open_terminal/utils/log.py:242` |
| ZIP files, previews, search data | Temporary directory | File routes | `main.py:1660`, `open_terminal/utils/search.py:36` |
| Desktop data and X lock files | Temporary directory | Desktop and browser | `open_terminal/desktop/router.py:114` |

In the default configuration the server writes to no other path.

Result for warren: the pod must mount the volume at `$HOME` (`/home/user`). The pod must also set the working directory to the same path.

### 8.5 Results for the restricted Pod Security Standard

1. Writable mounts: `/home/user` (PVC) and `/tmp` (`emptyDir`). The root file system can be read-only.
2. The `openshift` image is the best fit. It has a numeric user, no `sudo`, and an entry point that needs no privilege.
3. The `openshift` image has no Chrome mode and no desktop. Its browser `frame` route stays on unless the pod sets `OPEN_TERMINAL_ENABLE_BROWSER=false`.
4. Inference for the full image: Kubernetes cannot do the `runAsNonRoot` check for a user name. The pod must set `runAsUser`. The implementation must read the real user ID from the image.
5. The launcher of the full image uses a Python copy that has the file capability `cap_setgid` with the effective flag (`Dockerfile:86-89`).
6. Inference for the full image: with `capabilities.drop: [ALL]`, the kernel refuses to start that Python copy. Thus the launcher of the full image does not start in a restricted pod.
7. Inference for the full image: the command `python3 -m open_terminal run` uses the usual Python program and can start (`open_terminal/__main__.py:1-3`). This command does not go through the entry point.
8. The full image keeps `sudo`. `allowPrivilegeEscalation: false` makes it ineffective.
9. `slim` and `alpine`: the pod must replace the entry point. The pod must also set the working directory.
10. The volume must be writable for the user ID of the pod. `fsGroup` does this.
11. The API key comes from a Secret. Inference: a shell in the pod operates as the same user as the server and can read the key of its own pod.
12. Thus each pod must have a different key, and the key must give access to that pod only.
13. The browser starts with `--no-sandbox` in all images (`Dockerfile:95`).

Items 4, 6, 7, and the `slim` entry point need a test on a cluster before the implementation selects the default image.

## 9. Proxy requirements

| Subject | Requirement | Reference |
|---|---|---|
| Path prefix | The server expects to be at `/`. It has no setting for a path prefix or for forwarded headers. Warren must send root paths to the pod. | `cli.py:210-225` |
| Origin and Host | The server does no Origin check and no Host check. | `main.py:231-237` |
| CORS | The default is `*` with credentials. This is not important when only warren can connect to the pod. | `env.py:44-47` |
| Long requests | `/execute` routes wait a maximum of 300 seconds, unless `OPEN_TERMINAL_EXECUTE_TIMEOUT` is set (section 3.2). | `main.py:1835-1840` |
| WebSockets | Most stay open with no time limit, and can be silent for a long time. | Section 3 |
| WebSocket upgrade failure | The pod can reply with status 403 to an upgrade request. | Sections 3.4 and 4.3 |
| Terminal input | Warren must not hold terminal input and then send it in one group. The server closes when more than 64 messages wait. | Section 3.4 |
| Message size | A maximum of 16 MiB for agent messages and browser relay messages. Open WebUI permits 20 MiB on the agent WebSocket. | `open_terminal/agents/sessions.py:45`, `open_terminal/browser/router.py:448` |
| Body size | Uploads and downloads have no size limit. Warren must not buffer them. | Section 3.3 |
| Streams | Open Terminal sends no server-sent events of its own. The port proxy and the browser `frame` route can carry event streams and long downloads. | Sections 7.1 and 7.2 |
| Keep-alive | Test result: the default keep-alive time of uvicorn is 5 seconds. Warren must set `OPEN_TERMINAL_UVICORN_TIMEOUT_KEEP_ALIVE`, or it must not use an idle connection again. | `env.py:214-220` |

## 10. Differences between the Open Terminal routes and the Open WebUI proxy

Open WebUI gives a browser route for some Open Terminal WebSockets, but not for all of them. Refer to `protocol.md`, section 4.2.

| Open Terminal WebSocket | Browser route in Open WebUI | Other caller |
|---|---|---|
| `/events` | Yes | |
| `/execute` | Yes | |
| `/execute/{process_id}/status` | Yes | |
| `/api/terminals/{session_id}` | Yes | |
| `/desktop/stream`, `/desktop/audio` | Yes | |
| `/browser/view/{sid}/{token}/ws`, `/stream`, `/vnc` | Yes | |
| `/agents/sessions/{sid}/connect` | No | The Open WebUI backend connects directly. Refer to `protocol.md`, section 4.7. |
