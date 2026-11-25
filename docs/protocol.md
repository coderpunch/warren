# Protocol: how Open WebUI calls a terminal server

Status: research record. This document contains no design decisions that are final.

## 1. Sources and rules

| Item | Value |
|---|---|
| Repository | `github.com/open-webui/open-webui` |
| Version | `v0.13.4` |
| Commit | `0666e59d26a4d056e0bacee41742486badd71cf8` (2026-11-17) |
| License | Open WebUI License (BSD-3-Clause text with a brand clause). It is not MIT. |

All file paths in this document are relative to the root of that repository.
A reference such as `routers/terminals.py:353` means line 353 of `backend/open_webui/routers/terminals.py`.
Paths that start with `src/` are frontend files.

Clean-room rules that apply to this document:

- This document records facts about a wire protocol. It contains no copied source code.
- No person read the `open-webui/terminals` repository for this document.
- Where Open WebUI calls a management API on an orchestrator, this document records only the client side of the call.

## 2. Terms

| Term | Meaning |
|---|---|
| Terminal server | An HTTP server that has the Open Terminal API. |
| Connection | One entry in the list of terminal servers in Open WebUI. An administrator makes it. |
| Plain connection | A connection that points to one Open Terminal server. |
| Orchestrator connection | A connection with `server_type` set to `orchestrator`. |
| Template | A record that the orchestrator keeps. It gives the name, scope, image, and limits of a terminal. |
| Instance | One sandbox that an orchestrator starts for a template and a context. |
| Context | The user ID, chat ID, and model ID that select an instance. |
| Session token | The JWT that Open WebUI gives to a browser after sign-in. |
| Connection key | The static secret in the `key` field of a connection. |
| Caller | The user that Open WebUI authenticated for a request. |
| Instance owner | The user whose instance a request uses. Usually this is the caller. |
| Tether | A reverse connection from the device of a user to Open WebUI, with a `tether://` URL. Warren does not use it. |

## 3. Connection configuration

An administrator sets connections in the administrator settings or in the environment variable `TERMINAL_SERVER_CONNECTIONS` (a JSON list, `config.py:375`).
The function is active only when `ENABLE_TOOL_SERVERS` is true (`env.py:1186`).
Open WebUI has no license check on these code paths. An orchestrator connection needs no license key in Open WebUI.

Fields of one connection (`routers/configs.py:313-330`):

| Field | Default | Use |
|---|---|---|
| `id` | empty | The selector that the browser uses in the URL. |
| `name` | empty | The name that the user sees. |
| `enabled` | `true` | Open WebUI ignores a disabled connection. |
| `url` | none | Base URL of the terminal server. Open WebUI removes a final `/` (`utils/terminals.py:36-37`). |
| `path` | `/openapi.json` | Path of the OpenAPI document for tool discovery. |
| `key` | empty | Connection key. |
| `auth_type` | `bearer` | Refer to section 5. |
| `forward_cookies` | `false` | If true, Open WebUI sends the browser cookies to the terminal server. |
| `config` | none | Object with `access_grants`, `chat_uploads`, `working_directory_context`, `user_shell_tools` (`src/lib/components/AddTerminalServerModal.svelte:53-56`). |
| `server_type` | none | `orchestrator`, `terminal`, or empty. Only `orchestrator` changes the behaviour. |

The model permits more fields (`routers/configs.py:329`).

Open WebUI keeps the path part of `url` in each URL that it calls, also for WebSockets (`utils/terminals.py:36-37`, `routers/terminals.py:339`, `:647-664`).
Open WebUI does not send `url` or `key` to the browser.

Open WebUI adds the headers in `TERMINAL_PROXY_HEADERS` (a JSON object, `config.py:380`) to each proxied HTTP response (`routers/terminals.py:445`).

### 3.1 How Open WebUI finds the server type

The "verify" operation in the administrator settings does these steps (`routers/configs.py:365-414`):

1. It sends `GET {url}/api/v1/terminals/templates`. If the status is 2xx, the type is `orchestrator`.
2. If not, it sends `GET {url}/api/config`. If the status is 2xx, the type is `terminal`.

It sends `Authorization: Bearer <key>` only when `auth_type` is `bearer` or `session` and the connection has a key (`routers/configs.py:382-384`).
The administrator interface keeps the result in `server_type` (`AddTerminalServerModal.svelte:69`, `:101`).

Thus one URL cannot be an orchestrator connection and a plain connection at the same time. The reply to the first request decides the type.

### 3.2 Effects of `server_type: orchestrator`

- A user cannot select the connection directly. Open WebUI replies with status 409 (`utils/terminals.py:335-336`).
- Open WebUI gets the template list from the orchestrator. Each template becomes one terminal in the user interface (`utils/terminals.py:151-198`).
- The terminal ID has the form `template:{connection_id}:{template_id}`. Open WebUI percent-encodes the template ID (`utils/terminals.py:102-103`).
- Open WebUI sets `auth_type` to `bearer` for each template terminal (`utils/terminals.py:195`, `:382`, `AddTerminalServerModal.svelte:98`). No other authentication mode is possible.
- Open WebUI does not put the connection in the start-up OpenAPI cache (`utils/tools.py:1503`).

## 4. Call paths

Open WebUI calls a terminal server on seven paths (A to G).
The browser never calls the terminal server directly. The browser base URL is always `{WEBUI_API_BASE_URL}/terminals/{selector}` (`src/lib/apis/terminal/index.ts:186-188`).

### 4.1 Path A: HTTP proxy for the browser

Route: `/api/v1/terminals/{server_id}/{path}` and `/api/v1/terminals/{server_id}/chats/{chat_id}/{path}` (`main.py:862`, `routers/terminals.py:300-302`).
Methods: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS (`routers/terminals.py:297`).

The browser sends these headers to Open WebUI (`src/lib/apis/terminal/index.ts:245-255`):

| Header | Value |
|---|---|
| `Authorization` | `Bearer <session token>` |
| `X-Terminal-User-Id` | The owner of a different instance. Only a manager of the terminal can use it. |
| `X-Session-Id` | Chat ID |
| `X-Terminal-Model-Id` | Model ID, only for a template with model scope |

Open WebUI then does these steps:

1. It authenticates the caller (`routers/terminals.py:307`).
2. It makes a context from the three headers and the `chat_id` path parameter (`routers/terminals.py:314-318`).
3. It finds the connection and makes sure that the caller has access (`utils/terminals.py:269-385`).
4. For a saved chat, it makes sure that the caller can write to the chat (`utils/terminals.py:298-304`).
5. It decodes the path a maximum of 8 times. It rejects a path that stays encoded (`routers/terminals.py:135-142`).
6. It rejects a path that contains `\`, tab, carriage return, or line feed (`routers/terminals.py:146`).
7. It resolves `.` and `..` segments. It rejects a path that goes above the root (`routers/terminals.py:149-154`).
8. It rejects a path that starts with `api/v1/policies`, `api/v1/status`, or `api/v1/terminals` with status 403 (`routers/terminals.py:48`, `:344-348`).
9. It makes a new set of request headers (`routers/terminals.py:353-399`). It copies only `Content-Type` from the browser request.
10. It sends the request with a total timeout of 300 seconds and a connect timeout of 10 seconds (`routers/terminals.py:419`).

Request properties:

- Open WebUI copies the query string of the browser without changes (`routers/terminals.py:350-351`).
- Open WebUI sends the request body as a stream (`routers/terminals.py:424-425`).
- Open WebUI sends cookies only when `forward_cookies` is true (`routers/terminals.py:358`).
- Open WebUI does not follow redirects (`routers/terminals.py:434`).

Response properties:

- Open WebUI removes `transfer-encoding`, `connection`, `content-encoding`, `content-length`, `server`, and `date` (`routers/terminals.py:50-52`).
- Open WebUI sends the response as a stream when the content type contains `application/octet-stream`, `image/`, `application/pdf`, or `text/event-stream` (`routers/terminals.py:47`, `:464`).
- Open WebUI reads all other responses into memory first (`routers/terminals.py:478`).
- When Open WebUI cannot connect, it replies with status 502 and a JSON object with an `error` field (`routers/terminals.py:510-517`).

### 4.2 Path B: WebSocket proxy for the browser

Open WebUI has these WebSocket routes below `/api/v1/terminals/{server_id}`:

| Route | Path on the terminal server | Reference |
|---|---|---|
| `/events` | `events` | `routers/terminals.py:590-592` |
| `/desktop/stream` | `desktop/stream` | `routers/terminals.py:595-597` |
| `/desktop/audio` | `desktop/audio` | `routers/terminals.py:600-602` |
| `/execute` | `execute` | `routers/terminals.py:605-607` |
| `/execute/{process_id}/status` | `execute/{process_id}/status` | `routers/terminals.py:610-614` |
| `/api/terminals/{session_id}` | `api/terminals/{session_id}` | `routers/terminals.py:617-619` |
| `/browser/view/{sid}/{token}/{kind}` | `browser/view/{sid}/{capability}/{kind}` | `routers/terminals.py:218-270` |

`kind` is `ws`, `stream`, or `vnc` (`routers/terminals.py:224`).
The first six routes use the sequence of section 4.2.1. The browser view route uses the sequence of section 4.2.2.

#### 4.2.1 The six routes with a first message

1. Open WebUI accepts the WebSocket (`routers/terminals.py:634`).
2. The browser sends one text message. The message is a JSON object with `type: "auth"`, `token`, and `chat_id` (`src/lib/apis/terminal/socket.ts:42-57`).
3. The object can also have `instance_user_id`, `model_id`, `resume`, `cursor`, `flow_control`, and `process_info`.
4. Open WebUI waits a maximum of 10 seconds for this message (`routers/terminals.py:534`).
5. Open WebUI verifies the token and finds the connection (`routers/terminals.py:558-587`).
6. Open WebUI opens a WebSocket to the terminal server. The URL has the scheme `ws` or `wss` and the query parameter `user_id` (`routers/terminals.py:647-666`).
7. For `execute/...` routes, Open WebUI also copies the query parameters `offset` and `tail` (`routers/terminals.py:652-655`).
8. The upgrade request has `X-User-Id`, `X-Session-Id`, and the template headers (`routers/terminals.py:656`). It has no `Authorization` header.
9. Open WebUI sends one text message to the terminal server. The message is the JSON object from the browser with a different `token` value (`routers/terminals.py:682-687`).
10. When `auth_type` is `bearer`, the `token` value is the connection key. For all other values of `auth_type`, it is an empty string.
11. Open WebUI then copies text and binary messages in the two directions (`routers/terminals.py:700-732`).
12. Each 10 seconds, Open WebUI makes sure that the caller continues to have access. If not, it closes the WebSocket (`routers/terminals.py:734-744`).

Important: on these six routes the credential of the caller does not go to the terminal server. This is true for all authentication modes.

Important: Open WebUI changes only the `token` field of the first message. All other fields, and all subsequent messages, come from the browser without a check.

Close codes that Open WebUI sends to the browser on these six routes:

| Code | Cause | Reference |
|---|---|---|
| 4001 | Bad or late first message, or bad token | `routers/terminals.py:537-547`, `:567` |
| 4003 | Access denied, or the function is off | `routers/terminals.py:561`, `:577` |
| 4004 | Connection not found | `routers/terminals.py:584` |
| 1011 | Failure of the terminal server. Open WebUI also changes the codes 1005, 1006, and 1015 from the terminal server to 1011. | `routers/terminals.py:729`, `:763` |
| 1013 | A tether connection is offline | `routers/terminals.py:582` |

When the handshake with the terminal server fails with status 401, 403, or 404, Open WebUI sends the close code 4001, 4003, or 4004 (`routers/terminals.py:762-763`).

#### 4.2.2 The browser view route

1. Open WebUI verifies the browser view token in the path (`routers/terminals.py:221`). Refer to section 8.1.
2. Open WebUI opens a WebSocket to the terminal server. The URL has no query parameter (`routers/terminals.py:235-236`).
3. The upgrade request has `X-User-Id`, `X-Session-Id`, and the template headers (`routers/terminals.py:237`).
4. The upgrade request also has the `Authorization` value and the cookies that the browser view token contains (`routers/terminals.py:238-246`).
5. Open WebUI sends no first message. It copies the messages of the browser in the two directions (`routers/terminals.py:186-206`).
6. The maximum message size is 16 MiB (`routers/terminals.py:253`).

The `Authorization` value is the value of the path A request that made the browser session (`routers/terminals.py:486-494`).
In `bearer` mode it is the connection key. In `session` mode it is the credential of the caller at that time.

Close codes on this route: 4001 for each failure before Open WebUI accepts the WebSocket, 4004 for a handshake status of 401, 403, or 404, and 1011 for all other failures (`routers/terminals.py:223-233`, `:261-270`).

#### 4.2.3 Browser retry rules

- All browser WebSockets stop after the close codes 4001, 4003, and 4004. They try again after 1011 and 1013.
- The `execute` streams also stop after code 1000 and after each code from 4000 to 4999. The delay increases from 1 second to 30 seconds (`src/lib/apis/terminal/socket.ts:95-107`).
- The interactive terminal stops after 1000, 4001, 4003, and 4004 only. The maximum delay is 10 seconds (`src/lib/components/chat/XTerminal.svelte:206`, `:222-223`).

### 4.3 Path C: tool calls from the Open WebUI backend

When a model uses the terminal tools, the Open WebUI backend calls the terminal server (`utils/tools.py:1588-1733`).

How Open WebUI gets the tool list:

- Template terminal: Open WebUI sends `GET {url}/openapi.json` each time it gets the tools of the terminal. The request has the connection key and the template headers (`utils/tools.py:1618-1621`).
- Plain connection: Open WebUI uses a cache that it fills at start-up (`utils/tools.py:1640-1641`). Refer to path E.

Calls for each chat turn:

| Call | Time limit | Result of a failure | Reference |
|---|---|---|---|
| `GET {url}/openapi.json` (template terminal) | `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER_DATA`, default 10 seconds | The chat turn fails with status 503. | `utils/tools.py:1753-1757`, `env.py:655-659`, `utils/middleware.py:2794` |
| `GET {url}/files/cwd` | 5 seconds | Open WebUI ignores the failure. | `utils/tools.py:1430-1446` |
| `GET {url}/api/config`, then `GET {url}/system` | 3 seconds for the two calls | Open WebUI ignores the failure. | `utils/tools.py:1467-1489` |
| One request for each tool call | `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER` | The tool call fails. | `utils/tools.py:2036-2043` |

Open WebUI sends `GET {url}/system` only when the `/api/config` response has `features.system` (`utils/tools.py:1477`).
The OpenAPI document gives the method and path of each tool call.
A tool call follows redirects only when `AIOHTTP_CLIENT_ALLOW_REDIRECTS` is true. The default is false (`env.py:627`, `utils/tools.py:2054`).

Headers (`utils/tools.py:1650-1668`): `Content-Type: application/json`, `X-User-Id`, `Authorization` (refer to section 5), `X-Session-Id` (chat ID), and the template headers.

### 4.4 Path D: helper calls from the Open WebUI backend

Open WebUI also calls the terminal server for file upload, file read, skills, the working directory, and the state of the user terminal (`utils/terminals.py:415-482`).
Known paths: `GET /files/cwd`, `POST /files/cwd`, `/files/upload`, `/files/view`, `/files/read`, `/skills`, `/skills/read`, `/api/terminals/user/output`.

This path uses a different function to make the headers (`utils/terminals.py:388-412`, `utils/tools.py:143-199`). It sends:

- `Authorization`. Refer to section 5.
- The custom `headers` of the connection, if the connection has them. Refer to section 6.4.
- When `ENABLE_FORWARD_USER_INFO_HEADERS` is true: the user information headers. Refer to section 6.3.
- The template headers, `Accept: application/json`, `X-User-Id`, and `X-Session-Id`.

The time limit is `AIOHTTP_CLIENT_TIMEOUT_TERMINAL` (`env.py:662-665`).
If the variable is not set, the value of `AIOHTTP_CLIENT_TIMEOUT_TOOL_SERVER_DATA` applies. If the two variables are not set, the limit is 60 seconds.
Some calls have a shorter limit of 5 seconds: the state of the user terminal and the read of `AGENTS.md` (`utils/terminals.py:546`, `:640`).

The source comment says that a terminal request can make a container before the response comes (`env.py:661`).
On a timeout, the user sees a message. The message says that it is possible that the start of the terminal is not complete (`utils/terminals.py:477-481`).

### 4.5 Path E: calls with no user

For a plain connection, Open WebUI calls the terminal server at start-up and after a configuration change (`utils/tools.py:1493-1548`). These calls have no user and no identity header.

| Call | Credential | Reference |
|---|---|---|
| `GET {url}{path}` for the OpenAPI document | The connection key when `auth_type` is `bearer` or `session`. No header in the other modes. | `utils/tools.py:1510-1520` |
| `GET {url}/api/config`, then `GET {url}/system` | The connection key only when `auth_type` is `bearer`. No header in the other modes. | `utils/tools.py:1540-1543` |

### 4.6 Path F: management API of an orchestrator

Open WebUI calls these paths only on an orchestrator connection (`utils/terminals.py:46-69`, `routers/templates.py`).
Each call has `Authorization: Bearer <connection key>` and no user headers. The value of `auth_type` has no effect.
If the connection has no key, Open WebUI replies with status 503 and does not call the orchestrator (`utils/terminals.py:47-48`).

| Method and path | Request data | Response fields that Open WebUI reads | Reference |
|---|---|---|---|
| `GET api/v1/terminals/templates` | none | List of objects with `id` and `data` | `utils/terminals.py:168` |
| `GET api/v1/terminals/templates/{id}` | none | `id`, `data` | `utils/terminals.py:147`, `:341` |
| `POST api/v1/terminals/templates` | `id` (UUID), `data` | `id` | `routers/templates.py:82-84` |
| `PATCH api/v1/terminals/templates/{id}` | The `data` fields | No field | `routers/templates.py:106` |
| `DELETE api/v1/terminals/templates/{id}` | none | No field | `routers/templates.py:115` |
| `GET api/v1/terminals/templates/{id}/instance` | Query: `user_id`, `chat_id`, `model_id`. Open WebUI does not send a parameter that has no value. | `status` | `routers/templates.py:161-165` |
| `GET api/v1/terminals/templates/{id}/instances` | none | List of objects with `id`, `context_id`, `status` | `routers/templates.py:176-190` |
| `POST api/v1/terminals/templates/{id}/restart` | Body: `user_id`, `chat_id`, `model_id` | No field | `routers/templates.py:194-209` |
| `POST api/v1/terminals/templates/{id}/stop` | Same body | No field | `routers/templates.py:212-214` |
| `POST api/v1/terminals/templates/{id}/start` | Same body | No field | `routers/templates.py:217-219` |
| `POST api/v1/terminals/templates/{id}/reset` | Same body | No field | `routers/templates.py:222-224` |
| `DELETE api/v1/terminals/templates/{id}/instance` | Same body | No field | `routers/templates.py:227-229` |
| `GET api/v1/terminals/status` | none | `allowed_images`, `max_cpu`, `max_memory`, `max_storage`, `network_profiles` | `routers/templates.py:74`, `src/lib/components/chat/UserTerminalsModal.svelte:105-112`, `:616` |

Response rules:

- Open WebUI reads the body of each successful response as JSON and sends it to the browser (`utils/terminals.py:67`). Thus each response must have a JSON body. A response with status 204 causes a failure.
- A status of 300 or more is an error. Open WebUI does not follow redirects (`utils/terminals.py:59-66`).
- On an error, Open WebUI reads the `detail` field of the JSON response (`utils/terminals.py:62-66`).
- The time limit is 180 seconds (`utils/terminals.py:51`).

Call frequency:

- Open WebUI sends `GET api/v1/terminals/templates/{id}` each time it finds a template terminal (`utils/terminals.py:341`). It does not keep the result.
- This occurs for each path A request, for each WebSocket that opens, and each 10 seconds for each open WebSocket.
- It also occurs before each tool call (`utils/tools.py:1703-1706`) and for each helper call.
- Open WebUI sends `GET api/v1/terminals/templates` for each request for the terminal list (`routers/terminals.py:279`).
- If one of these calls fails, the user request fails.

Data rules that the client shows:

- `context_id` has the form `kind:value`. `kind` is `user`, `chat`, or `model` (`routers/templates.py:178-190`).
- The value `default` is the `context_id` of the shared instance of a template with no scope (`UserTerminalsModal.svelte:267-268`).
- The instance `status` value `running` means that the instance operates. The user interface shows all other values as "Stopped" (`UserTerminalsModal.svelte:684`).
- `allowed_images` is one string with commas between the items. The user interface ignores an item that contains `*` (`UserTerminalsModal.svelte:105-108`).
- `max_cpu` is a number. The user interface accepts only a decimal number for the CPU limit, for example `0.5` (`UserTerminalsModal.svelte:154`).
- `max_memory` and `max_storage` are strings: a number with an optional unit, for example `512Mi` or `2Gi` (`UserTerminalsModal.svelte:145-161`).
- `network_profiles` is a list of strings (`UserTerminalsModal.svelte:616`).
- An instance operation body always has the three keys. A key with no value is null (`routers/templates.py:35-38`, `:196`).
- For a shared instance, `user_id` in an operation body is the ID of the manager that sends the operation (`routers/templates.py:196`).

Fields of template `data` that the Open WebUI user interface sends (`UserTerminalsModal.svelte:207-226`):

| Field | Meaning |
|---|---|
| `name` | The name that the user sees. Mandatory. |
| `scope` | `user`, `chat`, `model`, or null. Null means one shared instance (`UserTerminalsModal.svelte:49-54`). Open WebUI rejects all other values with status 422 (`utils/terminals.py:210-212`). |
| `image` | Container image, or null. |
| `cpu_limit` | CPU limit, or null. |
| `memory_limit` | Memory limit, or null. |
| `storage` | Storage size, or null. |
| `network_profile` | Name of a network profile, or null. |
| `env` | Object of environment variables, or null. The user interface gives special fields to `OPEN_TERMINAL_START_DIR` and `OPEN_TERMINAL_SETUP_SCRIPT` (`UserTerminalsModal.svelte:56-57`). |

Open WebUI keeps the access grants for a template in its own database (`routers/templates.py:87-89`, `:131-145`). The orchestrator does not get them.
A user cannot call the management API through path A, because Open WebUI blocks the `api/v1/...` prefixes (section 4.1, step 8).

### 4.7 Path G: agent sessions from the Open WebUI backend

The Open WebUI backend starts coding agents on the terminal server and connects to them directly (`utils/agents/transport.py`). The browser has no route for this WebSocket.

- `GET {url}/agents` finds the installed agents. The query can have `provider` and `cwd` (`utils/agents/discovery.py:36-47`).
- `POST {url}/agents/{provider}/sessions` starts a session, and `DELETE {url}/agents/sessions/{id}` stops it (`utils/agents/transport.py:60`, `:184`). The time limit is 30 seconds (`utils/agents/transport.py:42`).
- A WebSocket to `{url}/agents/sessions/{id}/connect` carries the agent data (`utils/agents/transport.py:86-92`). The maximum message size is 20 MiB. The backend sends a heartbeat each 20 seconds.

The headers are the headers of path D (`utils/agents/transport.py:34-38`). On this path the WebSocket upgrade request has the `Authorization` header.
The first WebSocket message has `type: "auth"`, `cursor`, and `token`. The `token` value is the bearer value of the `Authorization` header (`utils/agents/transport.py:95-96`).
Thus in `session` mode, the credential of the caller goes to the terminal server on this WebSocket.

## 5. Authentication modes

`auth_type` controls the credential that Open WebUI sends (`routers/terminals.py:359-382`, `utils/tools.py:1654-1662`, `utils/tools.py:160-183`).

| Path | `bearer` | `session` | `system_oauth` | `none` |
|---|---|---|---|---|
| A, C, D: HTTP header | Connection key | Credential of the caller | OAuth access token of the caller | No header |
| B: first message on the six routes | Connection key | Empty token | Empty token | Empty token |
| B: browser view WebSocket, and browser view HTTP | The header value from the time of the browser session | Same rule | Same rule | No header |
| E: OpenAPI document | Connection key | Connection key | No header | No header |
| E: `/api/config` and `/system` | Connection key | No header | No header | No header |
| F: management API | Connection key | Not applicable | Not applicable | Not applicable |
| G: HTTP header and first WebSocket message | Connection key | Credential of the caller | OAuth access token of the caller | No header, empty token |

Notes:

- An orchestrator connection always uses `bearer` (section 3.2). Thus the last three columns do not apply to template terminals.
- With `session`, the credential is the value that the caller gave to Open WebUI (`routers/terminals.py:368`).
- The credential can be a session token, an automation token, a subagent token, or an API key that starts with `sk-` (`utils/auth.py:421`, `utils/automations.py:382`, `utils/subagents.py:83`).
- Open WebUI has no JWKS endpoint for these tokens. Refer to section 7.

## 6. Identity and context data

### 6.1 Headers that Open WebUI sends to the terminal server

| Header | Value | Paths | Reference |
|---|---|---|---|
| `X-User-Id` | Open WebUI user ID of the instance owner | A, B, C, D, G | `routers/terminals.py:353`, `:656`, `utils/tools.py:1652`, `utils/terminals.py:408` |
| `X-Session-Id` | Chat ID from the browser. On path B the value can be an empty string. | A, B, C, D, G | `routers/terminals.py:355-357`, `:656` |
| `X-Terminal-Template-Id` | Template ID | A, B, C, D, G (template terminals only) | `utils/terminals.py:259` |
| `X-Terminal-Chat-Id` | Chat ID, only for a saved chat | Same | `utils/terminals.py:262-263`, `:221` |
| `X-Terminal-Model-Id` | Model ID, only when the template scope is `model` | Same | `utils/terminals.py:264-265`, `:222` |
| `user_id` (query parameter) | User ID of the instance owner | B, the six routes | `routers/terminals.py:651` |

Rules for `X-User-Id`:

- Open WebUI makes the value from the authenticated caller. It never copies an identity header from the browser (`utils/terminals.py:253-254`).
- The instance owner can be a different person from the caller in three cases.
- Case 1: a manager of a template opens the instance of a different user (`utils/terminals.py:281-293`).
- Case 2: a manager of a template opens the chat of a different user (`utils/terminals.py:300-304`).
- Case 3: a chat has a shared agent session that a different user owns (`utils/terminals.py:311-324`).
- On a plain connection, only case 3 is possible. Cases 1 and 2 need a template terminal (`utils/terminals.py:106-111`).

Rules for the chat ID:

- A temporary chat has an ID that starts with `temporary:` or `local:`. A channel chat has an ID that starts with `channel:` (`utils/chat_id.py:3-15`).
- Open WebUI verifies chat access only for a saved chat ID (`utils/terminals.py:297-304`).
- `X-Session-Id` is the value that the browser sent. For an ID that is not a saved chat ID, Open WebUI does no check. The browser user selects the value.
- `X-Terminal-Chat-Id` contains only a saved chat ID that Open WebUI verified.
- For a subagent chat, `X-Terminal-Chat-Id` is the ID of the parent chat (`utils/terminals.py:305-308`). `X-Session-Id` continues to be the ID of the subagent chat.
- A template with chat scope does not work without a saved chat. Open WebUI replies with status 409 (`utils/terminals.py:201-216`).

Rules for the model ID:

- A plain connection never gets a model ID. Only a template terminal with model scope gets `X-Terminal-Model-Id`.
- For a usual model ID, Open WebUI makes sure that the caller has access to the model (`utils/terminals.py:235-250`).
- For a model ID that starts with `agent:`, Open WebUI examines only the structure of the ID (`utils/terminals.py:229-234`).
- Thus a user of a model-scope template can send many different `agent:` IDs. Each ID is a new instance key.

### 6.2 Data that Open WebUI does not send

- Open WebUI does not send the user name, the email address, the role, or the groups of the user on paths A, B, and C.
- Open WebUI v0.13.4 does not send `X-Workspace-Id` or `X-Workspace-Directory` on any path. Open Terminal reads these headers. Refer to `open-terminal-api.md`, section 5.

### 6.3 User information headers on paths D and G

When `ENABLE_FORWARD_USER_INFO_HEADERS` is true, paths D and G add user information (`utils/headers.py:56-87`, `env.py:999-1018`).
The header names in this section are the default names. Environment variables can change them.

- If `FORWARD_USER_INFO_HEADER_JWT_SECRET` is set, Open WebUI sends one header, `X-OpenWebUI-User-Jwt`.
- The value of that header is an HS256 JWT with the claims `sub`, `email`, `name`, `role`, `iss` (`open-webui`), `iat`, and `exp`. The default life is 300 seconds (`utils/headers.py:42-53`).
- If not, Open WebUI sends `X-OpenWebUI-User-Name`, `X-OpenWebUI-User-Id`, `X-OpenWebUI-User-Email`, and `X-OpenWebUI-User-Role`.
- Open WebUI also sends `X-OpenWebUI-Chat-Id` and `X-OpenWebUI-Message-Id` (`utils/tools.py:194-197`).

Paths A, B, and C do not use this function. Thus it cannot be the only proof of identity.

### 6.4 How to get the groups of a user

No header contains the groups by default. There are three sources:

| Source | Credential | Notes | Reference |
|---|---|---|---|
| `GET /api/v1/users/groups` | The credential of the user | Returns only direct groups by default. `include_inherited=true` returns all groups. Open WebUI uses all groups for its access checks. | `routers/users.py:183-187`, `:1230-1236` |
| `GET /api/v1/users/{user_id}/groups` | An administrator credential | Same parameter. | `routers/users.py:1144-1151` |
| A custom header in the `headers` field of the connection, with the text `{{USER_GROUPS}}` or `{{USER_GROUP_IDS}}` | None | Paths D and G only. | `utils/tools.py:186-189`, `utils/headers.py:26` |

`GET /api/v1/groups/` returns group records with a member count. It does not return member IDs (`routers/groups.py:46-61`, `models/groups.py:104-105`).

### 6.5 Browser data that goes to the terminal server without a check

A terminal server must not use these inputs as proof of identity.

| Input | Path | Reference |
|---|---|---|
| The full query string. A browser can add `user_id` to an HTTP request. | A | `routers/terminals.py:350-351` |
| All fields of the first WebSocket message, but not `token` | B | `routers/terminals.py:685-687` |
| All subsequent WebSocket messages | B | `routers/terminals.py:700-710` |
| `X-Session-Id` when the value is not a saved chat ID | A, B | Section 6.1 |
| A model ID that starts with `agent:` | A, B | Section 6.1 |
| The request path, after the checks of section 4.1 | A | `routers/terminals.py:125-158` |

## 7. Session token format

| Property | Value | Reference |
|---|---|---|
| Algorithm | HS256 only | `utils/auth.py:48`, `:241` |
| Secret | `WEBUI_SECRET_KEY` (the old name is `WEBUI_JWT_SECRET_KEY`) | `utils/auth.py:47`, `env.py:774-777` |
| Key distribution | Shared secret. No public key and no JWKS endpoint. | `utils/auth.py:225-245` |
| Default life | `JWT_EXPIRES_IN`, default `4w`. The value `-1` means no expiry. | `config.py:2591-2596` |
| Short session | 900 seconds maximum, with a refresh token | `routers/sessions.py:114-119` |

Claims of a session token:

| Claim | Meaning | Reference |
|---|---|---|
| `id` | Open WebUI user ID | `utils/auth.py:727-737` |
| `typ` | `session`. A token with no `typ` is also a session token. | `utils/auth.py:279` |
| `session_stamp` | A value that Open WebUI changes to revoke all tokens of an account | `utils/auth.py:277` |
| `jti` | Random token ID | `utils/auth.py:232-233` |
| `iat`, `exp` | Issue time and expiry time. `exp` can be absent. | `utils/auth.py:228-233` |
| `sid` | Session row ID (short sessions only) | `routers/sessions.py:117-118` |
| `auth_method`, `auth_time`, `mfa_verified` | Sign-in data | `utils/auth.py:727-735` |

Open WebUI signs seven token types with the same secret:

| `typ` | Use | Open WebUI accepts it as a user credential |
|---|---|---|
| `session` | Browser session | Yes |
| `automation` | Scheduled automation, life 1 hour by default | Yes (`utils/auth.py:280`) |
| `subagent` | Subagent run, life 1 hour | Yes (`utils/auth.py:280`) |
| `refresh` | Refresh token | No |
| `session_code` | Sign-in code, life 60 seconds | No |
| `browser_view` | Browser view link, no expiry | No |
| `agent_session` | Agent state | No |

A verifier must reject the last four types.
A verifier must accept `automation` and `subagent` tokens if automations and subagents must operate in `session` mode.

Checks that Open WebUI does and that a local verifier cannot do:

- `session_stamp` must agree with the account (`utils/auth.py:276-278`).
- The session row for `sid` must exist, must belong to the user, and must not be expired (`utils/auth.py:282-292`).
- The multi-factor rules of the server must agree with `mfa_verified` (`utils/auth.py:294-303`).
- Redis revocation of one token or of all tokens of a user (`utils/auth.py:306-328`).
- The user must exist and have the role `user` or `admin` (`utils/auth.py:588-590`).

Thus a local HS256 check accepts a revoked token until the token expires.

Open WebUI endpoints that can verify a credential:

| Endpoint | Credentials that it accepts | Notes | Reference |
|---|---|---|---|
| `GET /api/v1/auths/` | Session tokens only. It rejects API keys, automation tokens, and subagent tokens with status 403. | It does not reject a user with the role `pending`. The response contains the role. | `routers/auths.py:185-190`, `utils/auth.py:769-772` |
| `GET /api/v1/users/groups` | Session, automation, and subagent tokens, and API keys | It rejects a user that does not have the role `user` or `admin`. An administrator setting can block API keys on this path. | `routers/users.py:183-187`, `utils/auth.py:573`, `:540-545` |

## 8. Special behaviours of Open WebUI

A terminal server must know these behaviours, because they change the requests that it gets.

### 8.1 Browser view tokens

- When a response from a path that starts with `browser/sessions` contains `id` and `token`, Open WebUI replaces `token` with its own signed token (`routers/terminals.py:479-495`).
- The browser then uses `/browser/view/{sid}/{token}/...`. Open WebUI replaces its token with the token of the terminal server before it calls the terminal server (`routers/terminals.py:161-183`).
- Open WebUI keeps the `Authorization` value and the cookies of the request in its token, encrypted (`utils/browser.py:9-30`).
- The token of Open WebUI has no expiry. Open WebUI makes sure only that the user continues to exist (`utils/browser.py:32-39`).
- Thus in `session` mode the terminal server can get a credential that is expired or revoked.
- On these routes Open WebUI also copies `accept`, `origin`, `access-control-request-method`, `access-control-request-headers`, `last-event-id`, and `range` (`routers/terminals.py:384-394`).

### 8.2 File preview for template terminals

For a template terminal, `POST files/preview` does not go to the terminal server.
Open WebUI makes a read-only token. The browser then gets `files/serve/{path}` (`routers/terminals.py:167-173`, `:401-413`).

### 8.3 Port proxy

For a path that starts with `proxy/{port}/`:

- Open WebUI changes a `Location` header that points to `http://localhost:{port}` (`routers/terminals.py:447-461`).
- Open WebUI changes HTML responses. It changes root-relative links and adds a script (`routers/terminals.py:55-122`, `:496-497`).

## 9. Results for warren

These are conclusions from the facts above. They are inputs to the design. They are not final decisions.

### 9.1 Token verification

1. Open WebUI has no JWKS. The only possible local check is HS256 with `WEBUI_SECRET_KEY`.
2. A component that has `WEBUI_SECRET_KEY` can make a valid token for each Open WebUI user. This increases the damage if an attacker gets control of warren.
3. A credential of the caller gets to a terminal server only when `auth_type` is `session`.
4. In `session` mode, paths A, C, D, and G have the credential. The browser view WebSocket has a stored copy that can be old.
5. In `session` mode, the six first-message WebSockets of path B have no credential. The terminal server gets only `X-User-Id`, `X-Session-Id`, and `user_id`.
6. In `session` mode, path E has the connection key for the OpenAPI document and no credential for `/api/config` and `/system`.
7. In `session` mode the credential can be an API key (`sk-...`). A local check cannot verify an API key. Only a call to Open WebUI can.
8. In `session` mode the credential identifies the caller, and `X-User-Id` identifies the instance owner. They are different for a shared agent session.
9. An orchestrator connection always uses the connection key. Token verification is not possible on an orchestrator connection.

### 9.2 Scopes

1. An orchestrator connection gives the template ID, the user ID, the verified saved chat ID, and the model ID. All four warren scopes are possible: user, chat, model, and shared.
2. A plain connection gives a user ID that Open WebUI made, and an `X-Session-Id` that the browser selected. It gives no model ID.
3. On a plain connection, user scope is possible. Model scope is not possible.
4. On a plain connection, chat scope is possible only as a second key below the user ID. `X-Session-Id` alone must never select a sandbox.
5. With model scope, warren must limit the number of instance keys, because `agent:` model IDs have no check (section 6.1).

### 9.3 Header trust

1. Open WebUI sets `X-User-Id` and the `X-Terminal-*` headers on its server. A browser user cannot change them through Open WebUI.
2. Warren must take the identity only from these headers, or from a credential that it verified.
3. Warren must ignore the inputs of section 6.5 for identity. It must remove a `user_id` query parameter from HTTP requests.
4. Warren must remove `X-Workspace-Id` and `X-Workspace-Directory` unless it knows the sender. Open WebUI does not send them.
5. A client that can connect to warren directly, and that has the connection key, can set all identity headers.
6. Thus in `bearer` mode the security of the identity depends on the secrecy of the key and on the network path to warren.

### 9.4 Time limits and cold start

1. On an orchestrator connection, the two template read calls are in the path of each user request (section 4.6). Warren must answer them from memory, with no delay.
2. `GET /openapi.json` has a limit of 10 seconds, and a failure stops the chat turn. Warren must answer it without a sandbox start.
3. `GET /files/cwd`, `GET /api/config`, and `GET /system` have limits of 5 and 3 seconds. A late reply causes no error, but the model then has no working directory and no system prompt.
4. Helper calls have a limit of 60 seconds, and browser HTTP requests have a limit of 300 seconds. These calls can wait for a sandbox start.
5. `GET /api/config` and `GET /openapi.json` arrive with no user on a plain connection (path E). Warren must answer them without a sandbox.

### 9.5 Decisions that are necessary before implementation

| No. | Question | Options |
|---|---|---|
| 1 | Which integration mode is the primary mode? | (a) Orchestrator connection: all scopes, key and headers. (b) Plain connection with `session`: credential check on HTTP, no model scope, no credential on six WebSockets. (c) Plain connection with `bearer`: key and `X-User-Id`, no model scope, no management API. (d) More than one mode. One URL can have only one server type (section 3.1). |
| 2 | How does warren authorize the six first-message WebSockets in `session` mode? | (a) A secret path prefix in the connection URL. It authenticates Open WebUI on all paths. (b) Network-level authentication of Open WebUI: NetworkPolicy or mutual TLS. (c) Accept a WebSocket for a terminal or process ID only if a verified HTTP request made that ID. (d) Do not offer `session` mode. Option (c) does not help for `/events`, `/execute`, and the desktop routes. |
| 3 | How does warren verify a credential in `session` mode? | (a) Local HS256: fast, needs `WEBUI_SECRET_KEY`, no revocation, no API keys. (b) A call to an Open WebUI endpoint (section 7): no secret, has revocation, adds latency. (c) Option (b) with a short cache. |
| 4 | Does `session` mode accept automation tokens, subagent tokens, and API keys? | If not, automations, subagents, and API clients cannot use a terminal in this mode. |
| 5 | In `session` mode, which identity selects the sandbox when the caller and `X-User-Id` are different? | (a) The caller. Shared agent sessions then use the sandbox of the caller. (b) `X-User-Id`. The credential then proves only that the caller is a valid user. |
| 6 | Where do the groups for policy selection come from? | (a) No groups in warren. Open WebUI gives a template to users and groups with its own access grants. (b) `GET /api/v1/users/groups` with the credential of the user (`session` mode only). (c) An API key of an Open WebUI administrator, for `GET /api/v1/users/{user_id}/groups`. |
| 7 | How much of the management API does warren implement? | (a) The two template read calls and the status call only. An administrator defines templates in the warren configuration. (b) Option (a) and the instance calls. (c) The full API. Note: the API paths contain the word `terminals`. The paths are part of the wire protocol and are not a brand name. |
