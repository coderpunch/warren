# Protocol: how Open WebUI calls a terminal server

Status: research record. This document contains no design decisions that are final.

## 1. Sources and rules

| Item | Value |
|---|---|
| Repository | `github.com/open-webui/open-webui` |
| Version | `v0.13.4` |
| Commit | `0666e59d26a4d056e0bacee41742486badd71cf8` (2026-11-17) |
| License | Open WebUI License (BSD-3-Clause text with a branding clause). It is not MIT. |

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
| Connection | One entry in the Open WebUI terminal server list. An administrator makes it. |
| Plain connection | A connection that points to one Open Terminal server. |
| Orchestrator connection | A connection with `server_type` set to `orchestrator`. |
| Template | A record that the orchestrator keeps. It gives the name, scope, image, and limits of a terminal. |
| Instance | One sandbox that an orchestrator starts for a template and a context. |
| Context | The user ID, chat ID, and model ID that select an instance. |
| Session token | The JWT that Open WebUI gives to a browser after sign-in. |
| Connection key | The static secret in the `key` field of a connection. |

## 3. Connection configuration

An administrator sets connections in the admin settings or in the environment variable `TERMINAL_SERVER_CONNECTIONS` (a JSON list, `config.py:375`).
The feature is active only when `ENABLE_TOOL_SERVERS` is true (`env.py:1186`).

Fields of one connection (`routers/configs.py:313-330`):

| Field | Default | Use |
|---|---|---|
| `id` | empty | The selector that the browser uses in the URL. |
| `name` | empty | Display name. |
| `enabled` | `true` | Open WebUI ignores a disabled connection. |
| `url` | none | Base URL of the terminal server. Open WebUI removes a final `/` (`utils/terminals.py:36-37`). |
| `path` | `/openapi.json` | Path of the OpenAPI document for tool discovery. |
| `key` | empty | Connection key. |
| `auth_type` | `bearer` | Refer to section 5. |
| `forward_cookies` | `false` | If true, Open WebUI sends the browser cookies to the terminal server. |
| `config` | none | Object with `access_grants`, `chat_uploads`, `working_directory_context`, `user_shell_tools` (`src/lib/components/AddTerminalServerModal.svelte:53-56`). |
| `server_type` | none | `orchestrator` or empty. |

The model permits more fields (`routers/configs.py:329`).

Open WebUI adds the headers in `TERMINAL_PROXY_HEADERS` (a JSON object, `config.py:380`) to each proxied HTTP response (`routers/terminals.py:445`).

### 3.1 How Open WebUI finds the server type

The admin "verify" operation does these steps (`routers/configs.py:365-414`):

1. It sends `GET {url}/api/v1/terminals/templates`. If the status is 2xx, the type is `orchestrator`.
2. If not, it sends `GET {url}/api/config`. If the status is 2xx, the type is `terminal`.

It sends `Authorization: Bearer <key>` only when `auth_type` is `bearer` or `session` and a key is set (`routers/configs.py:382-384`).

### 3.2 Effects of `server_type: orchestrator`

- A user cannot select the connection directly. Open WebUI replies with status 409 (`utils/terminals.py:335-336`).
- Open WebUI gets the template list from the orchestrator. Each template becomes one terminal in the user interface (`utils/terminals.py:151-198`).
- The terminal ID has the form `template:{connection_id}:{template_id}`. The template ID is percent-encoded (`utils/terminals.py:102-103`).
- Open WebUI sets `auth_type` to `bearer` for each template terminal (`utils/terminals.py:195`, `utils/terminals.py:382`, `AddTerminalServerModal.svelte:98`). No other auth mode is possible.
- Open WebUI does not put the connection in the start-up OpenAPI cache (`utils/tools.py:1503`).

## 4. Call paths

Open WebUI calls a terminal server on seven paths (A to G). The browser never calls the terminal server directly. The browser base URL is always `{WEBUI_API_BASE_URL}/terminals/{selector}` (`src/lib/apis/terminal/index.ts:186-188`).

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

1. It authenticates the user (`routers/terminals.py:307`).
2. It makes a context from the three headers and the `chat_id` path parameter (`routers/terminals.py:314-318`).
3. It finds the connection and makes sure that the user has access (`utils/terminals.py:269-385`). For a saved chat, it makes sure that the user can write to the chat (`utils/terminals.py:298-304`).
4. It makes the path safe. It decodes the path until the path is stable. It rejects `..`, `\`, tab, and newline characters (`routers/terminals.py:125-158`).
5. It rejects a path that starts with `api/v1/policies`, `api/v1/status`, or `api/v1/terminals` with status 403 (`routers/terminals.py:48`, `routers/terminals.py:344-348`).
6. It makes a new set of request headers (`routers/terminals.py:353-399`). It does not copy the browser headers.
7. It sends the request with a total timeout of 300 seconds and a connect timeout of 10 seconds (`routers/terminals.py:419`). It does not follow redirects (`routers/terminals.py:434`).

Request properties:

- Open WebUI copies the query string without changes (`routers/terminals.py:350-351`).
- Open WebUI sends the request body as a stream (`routers/terminals.py:424-425`).
- Open WebUI sends cookies only when `forward_cookies` is true (`routers/terminals.py:358`).

Response properties:

- Open WebUI removes `transfer-encoding`, `connection`, `content-encoding`, `content-length`, `server`, and `date` (`routers/terminals.py:50-52`).
- Open WebUI sends the response as a stream when the content type contains `application/octet-stream`, `image/`, `application/pdf`, or `text/event-stream` (`routers/terminals.py:47`, `routers/terminals.py:464`).
- Open WebUI reads all other responses into memory first (`routers/terminals.py:478`).
- When Open WebUI cannot connect, it replies with status 502 and a JSON object with an `error` field (`routers/terminals.py:510-517`).

### 4.2 Path B: WebSocket proxy for the browser

Open WebUI has these WebSocket routes below `/api/v1/terminals/{server_id}`:

| Route | Upstream path | Reference |
|---|---|---|
| `/events` | `events` | `routers/terminals.py:590-592` |
| `/desktop/stream` | `desktop/stream` | `routers/terminals.py:595-597` |
| `/desktop/audio` | `desktop/audio` | `routers/terminals.py:600-602` |
| `/execute` | `execute` | `routers/terminals.py:605-607` |
| `/execute/{process_id}/status` | `execute/{process_id}/status` | `routers/terminals.py:610-614` |
| `/api/terminals/{session_id}` | `api/terminals/{session_id}` | `routers/terminals.py:617-619` |
| `/browser/view/{sid}/{token}/{kind}` | `browser/view/{sid}/{capability}/{kind}` | `routers/terminals.py:218-270` |

`kind` is `ws`, `stream`, or `vnc` (`routers/terminals.py:224`).

Sequence for all routes except the browser view route:

1. Open WebUI accepts the WebSocket (`routers/terminals.py:634`).
2. The browser sends one text message: a JSON object with `type: "auth"`, `token`, `chat_id`, and optionally `instance_user_id`, `model_id`, `resume`, `cursor`, `flow_control`, `process_info` (`src/lib/apis/terminal/socket.ts:42-57`).
3. Open WebUI waits a maximum of 10 seconds for this message (`routers/terminals.py:534`).
4. Open WebUI verifies the token and finds the connection (`routers/terminals.py:558-587`).
5. Open WebUI opens a WebSocket to the terminal server. The URL is the connection URL with the scheme `ws` or `wss`, the upstream path, and the query parameter `user_id` (`routers/terminals.py:647-666`).
6. For `execute/...` routes, Open WebUI also copies the query parameters `offset` and `tail` (`routers/terminals.py:652-655`).
7. Open WebUI sends the upgrade request with `X-User-Id`, `X-Session-Id`, and the template headers (`routers/terminals.py:656`). It sends no `Authorization` header.
8. Open WebUI sends one text message upstream: the JSON object from the browser, with a different `token` value (`routers/terminals.py:682-687`).
   - When `auth_type` is `bearer`, `token` is the connection key.
   - For all other auth types, `token` is an empty string.
9. Open WebUI then copies text and binary messages in the two directions (`routers/terminals.py:700-732`).
10. Each 10 seconds, Open WebUI makes sure that the user continues to have access. If not, it closes the WebSocket (`routers/terminals.py:734-744`).

Important: on path B the session token of the user does not go to the terminal server. This is true for all auth types.

Close codes that Open WebUI sends to the browser:

| Code | Cause | Reference |
|---|---|---|
| 4001 | Bad or late auth message, or bad token | `routers/terminals.py:537-547`, `routers/terminals.py:567` |
| 4003 | Access denied, or the feature is off | `routers/terminals.py:561`, `routers/terminals.py:577` |
| 4004 | Connection not found | `routers/terminals.py:584` |
| 1011 | Upstream failure. Open WebUI also changes the upstream codes 1005, 1006, and 1015 to 1011. | `routers/terminals.py:729`, `routers/terminals.py:763` |
| 1013 | A tether connection is offline | `routers/terminals.py:582` |

Open WebUI changes an upstream handshake status of 401, 403, or 404 to the close codes 4001, 4003, or 4004 (`routers/terminals.py:762-763`).

The browser tries again after a close, with a delay that increases from 1 second to 30 seconds. It does not try again after code 1000 or a code from 4000 to 4999 (`src/lib/apis/terminal/socket.ts:95-107`).

### 4.3 Path C: tool calls from the Open WebUI backend

When a model uses the terminal tools, the Open WebUI backend calls the terminal server (`utils/tools.py:1588-1733`).

Discovery of the tool list:

- Template terminal: `GET {url}/openapi.json` on each resolution, with the connection key and the template headers (`utils/tools.py:1618-1621`).
- Plain connection: Open WebUI uses a cache that it fills at start-up (`utils/tools.py:1640-1641`). Refer to path E.

Calls for each chat turn:

- `GET {url}/files/cwd` (`utils/tools.py:1430`) and `GET {url}/api/config`, then `GET {url}/system` (`utils/tools.py:1470-1484`).
- One request for each tool call. The OpenAPI document gives the method and path.

Headers (`utils/tools.py:1650-1668`): `Content-Type: application/json`, `X-User-Id`, `Authorization` (by auth type), `X-Session-Id` (chat ID), and the template headers.

### 4.4 Path D: helper calls from the Open WebUI backend

Open WebUI also calls the terminal server for file upload, file read, skills, and the user terminal state (`utils/terminals.py:415-482`).
Known paths: `/files/cwd`, `/files/upload`, `/files/view`, `/files/read`, `/skills`, `/skills/read`, `/api/terminals/user/output`.

This path uses a different header builder (`utils/terminals.py:388-412`, `utils/tools.py:143-199`). It sends:

- `Authorization` by auth type.
- The custom `headers` of the connection, if the connection has them.
- When `ENABLE_FORWARD_USER_INFO_HEADERS` is true: the user information headers. Refer to section 6.3.
- The template headers, `Accept: application/json`, `X-User-Id`, and `X-Session-Id`.

The timeout is `AIOHTTP_CLIENT_TIMEOUT_TERMINAL`. The default is 60 seconds (`env.py:662-665`).
The source comment says that a terminal request can provision a container before it returns (`env.py:661`).
On a timeout, the user sees a message that says the terminal is possibly starting (`utils/terminals.py:477-481`).

### 4.5 Path E: calls with no user

For a plain connection, Open WebUI calls the terminal server at start-up and after a configuration change (`utils/tools.py:1493-1548`):

- `GET {url}{path}` for the OpenAPI document.
- `GET {url}/api/config` and `GET {url}/system`.

These calls have no user. When `auth_type` is `session`, Open WebUI uses the connection key as the bearer token (`utils/tools.py:1510-1512`).

### 4.6 Path F: management API of an orchestrator

Open WebUI calls these paths only on an orchestrator connection (`utils/terminals.py:46-69`, `routers/templates.py`).
Each call has `Authorization: Bearer <connection key>`, a JSON body where applicable, and a timeout of 180 seconds.
If the connection has no key, Open WebUI replies with status 503 and does not call the orchestrator (`utils/terminals.py:47-48`).
These calls have no user headers.

| Method and path | Request data | Response fields that Open WebUI reads | Reference |
|---|---|---|---|
| `GET api/v1/terminals/templates` | none | List of objects with `id` and `data` | `utils/terminals.py:168` |
| `GET api/v1/terminals/templates/{id}` | none | `id`, `data` | `utils/terminals.py:147`, `utils/terminals.py:341` |
| `POST api/v1/terminals/templates` | `id` (UUID), `data` | `id` | `routers/templates.py:82-84` |
| `PATCH api/v1/terminals/templates/{id}` | The `data` fields | Not read | `routers/templates.py:106` |
| `DELETE api/v1/terminals/templates/{id}` | none | Not read | `routers/templates.py:115` |
| `GET api/v1/terminals/templates/{id}/instance` | Query: `user_id`, `chat_id`, `model_id` | `status` | `routers/templates.py:161-165` |
| `GET api/v1/terminals/templates/{id}/instances` | none | List of objects with `id`, `context_id`, `status` | `routers/templates.py:176-190` |
| `POST api/v1/terminals/templates/{id}/restart` | Body: `user_id`, `chat_id`, `model_id` | Not read | `routers/templates.py:194-209` |
| `POST api/v1/terminals/templates/{id}/stop` | Same body | Not read | `routers/templates.py:212-214` |
| `POST api/v1/terminals/templates/{id}/start` | Same body | Not read | `routers/templates.py:217-219` |
| `POST api/v1/terminals/templates/{id}/reset` | Same body | Not read | `routers/templates.py:222-224` |
| `DELETE api/v1/terminals/templates/{id}/instance` | Same body | Not read | `routers/templates.py:227-229` |
| `GET api/v1/terminals/status` | none | `allowed_images`, `max_cpu`, `max_memory`, `max_storage`, `network_profiles` | `routers/templates.py:74`, `src/lib/components/chat/UserTerminalsModal.svelte:105-112`, `:616` |

Data rules that the client shows:

- `context_id` has the form `kind:value`. `kind` is `user`, `chat`, or `model` (`routers/templates.py:178-190`).
- The instance `status` value `running` means that the instance operates. The user interface shows all other values as "Stopped" (`UserTerminalsModal.svelte:684`).
- `allowed_images` is one string with commas between the items. The user interface ignores an item that contains `*` (`UserTerminalsModal.svelte:105-108`).
- On an error, Open WebUI reads the `detail` field of the JSON response (`utils/terminals.py:62-66`).

Fields of template `data` that the Open WebUI user interface sends (`UserTerminalsModal.svelte:207-226`):

| Field | Meaning |
|---|---|
| `name` | Display name. Mandatory. |
| `scope` | `user`, `chat`, `model`, or null. Null means one shared instance (`UserTerminalsModal.svelte:49-54`). |
| `image` | Container image, or null. |
| `cpu_limit` | CPU limit, or null. |
| `memory_limit` | Memory limit, or null. |
| `storage` | Storage size, or null. |
| `network_profile` | Name of a network profile, or null. |
| `env` | Object of environment variables, or null. The user interface gives special fields to `OPEN_TERMINAL_START_DIR` and `OPEN_TERMINAL_SETUP_SCRIPT` (`UserTerminalsModal.svelte:56-57`). |

Open WebUI keeps the access grants for a template in its own database (`routers/templates.py:87-89`, `routers/templates.py:131-145`). The orchestrator does not get them.
A user cannot call the management API through path A, because Open WebUI blocks the `api/v1/...` prefixes (section 4.1, step 5).

### 4.7 Path G: agent sessions from the Open WebUI backend

The Open WebUI backend starts coding agents on the terminal server and connects to them directly (`utils/agents/transport.py`). The browser has no route for this WebSocket.

- `GET {url}/agents` finds the installed agents (`utils/agents/discovery.py:47`).
- `POST {url}/agents/{provider}/sessions` starts a session, and `DELETE {url}/agents/sessions/{id}` stops it (`utils/agents/transport.py:60`, `:184`). The timeout is 30 seconds (`utils/agents/transport.py:42`).
- A WebSocket to `{url}/agents/sessions/{id}/connect` carries the agent data (`utils/agents/transport.py:86-92`). The maximum message size is 20 MiB. The backend sends a heartbeat each 20 seconds.

The headers are the headers of path D (`utils/agents/transport.py:34-38`). On this path the WebSocket upgrade request has the `Authorization` header.
The first WebSocket message has `type: "auth"`, `cursor`, and `token`. The `token` value is the bearer value of the `Authorization` header (`utils/agents/transport.py:95-96`).
Thus in `session` mode, the token of the user goes to the terminal server on this WebSocket. This is different from path B.

## 5. Auth modes

`auth_type` controls the `Authorization` header that Open WebUI sends (`routers/terminals.py:359-382`, `utils/tools.py:1654-1662`).

| `auth_type` | HTTP paths A, C, D, G | WebSocket path B | Paths E and F |
|---|---|---|---|
| `bearer` | `Bearer <connection key>` | First message contains the connection key | `Bearer <connection key>` |
| `session` | `Bearer <token of the user>` | First message contains an empty token | `Bearer <connection key>` (path E only) |
| `system_oauth` | `Bearer <OAuth access token of the user>` | First message contains an empty token | No header |
| `none` | No header | First message contains an empty token | No header |

Notes:

- The WebSocket of path G is different from path B. Its first message contains the same bearer value as the HTTP paths (section 4.7).
- An orchestrator connection always uses `bearer` (section 3.2).
- With `session`, the token is the credential that the caller gave to Open WebUI (`routers/terminals.py:368`). It can be a session token, an automation token, a subagent token, or an API key that starts with `sk-` (`utils/auth.py:421`, `utils/automations.py:382`, `utils/subagents.py:83`).
- Open WebUI has no JWKS endpoint for these tokens. Refer to section 7.

## 6. Identity and context headers

### 6.1 Headers that Open WebUI sends upstream

| Header | Value | Paths | Reference |
|---|---|---|---|
| `X-User-Id` | Open WebUI user ID. Refer to the rules below. | A, B, C, D | `routers/terminals.py:353`, `:656`, `utils/tools.py:1652`, `utils/terminals.py:408` |
| `X-Session-Id` | Chat ID. On path B the value can be an empty string. | A, B, C, D | `routers/terminals.py:355-357`, `:656` |
| `X-Terminal-Template-Id` | Template ID | A, B, C, D (template terminals only) | `utils/terminals.py:259` |
| `X-Terminal-Chat-Id` | Chat ID, only for a saved chat | Same | `utils/terminals.py:262-263`, `:221` |
| `X-Terminal-Model-Id` | Model ID, only when the template scope is `model` | Same | `utils/terminals.py:264-265`, `:222` |
| `user_id` (query parameter) | Instance user ID, or user ID | B | `routers/terminals.py:651` |

Rules for `X-User-Id`:

- Open WebUI makes the value from the authenticated user. It never copies an identity header from the browser (`utils/terminals.py:253-254`).
- For a template terminal, the value is the user ID of the instance owner (`utils/terminals.py:260`, `:366`).
- The instance owner can be a different person from the caller in three cases: a manager opens the instance of a different user (`utils/terminals.py:281-293`), a manager opens the chat of a different user (`utils/terminals.py:300-304`), or a chat has a shared agent session (`utils/terminals.py:311-324`).
- For a plain connection, the value is the caller, unless one of these cases applies (`utils/terminals.py:255-256`).

Rules for the chat ID:

- A temporary chat has an ID that starts with `temporary:` or `local:`. A channel chat has an ID that starts with `channel:` (`utils/chat_id.py:3-15`).
- `X-Terminal-Chat-Id` never contains these IDs. `X-Session-Id` can contain them.
- A template with chat scope does not work without a saved chat. Open WebUI replies with status 409 (`utils/terminals.py:201-216`).
- A subagent chat uses the instance of its parent chat (`utils/terminals.py:305-308`).

A plain connection never gets a model ID. Only a template terminal gets `X-Terminal-Model-Id`.

### 6.2 Headers that Open WebUI does not send

Open WebUI does not send the user name, the email address, the role, or the groups of the user on paths A, B, and C.

### 6.3 User information headers on path D

When `ENABLE_FORWARD_USER_INFO_HEADERS` is true, path D adds user information (`utils/headers.py:56-87`, `env.py:999-1018`).

- If `FORWARD_USER_INFO_HEADER_JWT_SECRET` is set, Open WebUI sends one header, `X-OpenWebUI-User-Jwt`.
  The value is an HS256 JWT with the claims `sub`, `email`, `name`, `role`, `iss` (`open-webui`), `iat`, and `exp`. The default life is 300 seconds (`utils/headers.py:42-53`).
- If not, Open WebUI sends `X-OpenWebUI-User-Name`, `X-OpenWebUI-User-Id`, `X-OpenWebUI-User-Email`, and `X-OpenWebUI-User-Role`.
- Open WebUI also sends `X-OpenWebUI-Chat-Id` (`utils/tools.py:194-195`).

Paths A, B, and C do not use this function. Thus it cannot be the only proof of identity.

### 6.4 How to get the groups of a user

A terminal server can get the groups only with a call to the Open WebUI API:

- `GET /api/v1/users/groups` with the token of a user returns the groups of that user (`routers/users.py:183-187`).
- `GET /api/v1/groups/` with an administrator credential returns all groups (`routers/groups.py:46-61`).

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
| `id` | Open WebUI user ID | `routers/auths.py:1105-1107` |
| `typ` | `session`. A token with no `typ` is also a session token. | `utils/auth.py:279` |
| `session_stamp` | A value that Open WebUI changes to revoke all tokens of an account | `utils/auth.py:277` |
| `jti` | Random token ID | `utils/auth.py:232-233` |
| `iat`, `exp` | Issue time and expiry time. `exp` can be absent. | `utils/auth.py:228-233` |
| `sid` | Session row ID (short sessions only) | `routers/sessions.py:117-118` |
| `auth_method`, `auth_time`, `mfa_verified` | Sign-in data | `utils/auth.py:727-735` |

Open WebUI signs other token types with the same secret. A verifier must reject them.

| `typ` | Use | Accepted as a user credential by Open WebUI |
|---|---|---|
| `session` | Browser session | Yes |
| `automation` | Scheduled automation, life 1 hour by default | Yes (`utils/auth.py:280`) |
| `subagent` | Subagent run, life 1 hour | Yes (`utils/auth.py:280`) |
| `refresh` | Refresh token | No |
| `session_code` | Sign-in code, life 60 seconds | No |
| `browser_view` | Browser view link | No |
| `agent_session` | Agent state | No |

Checks that Open WebUI does and that a local verifier cannot do:

- `session_stamp` must agree with the account (`utils/auth.py:276-278`).
- The session row for `sid` must exist (`utils/auth.py:282-292`).
- Redis revocation of one token or of all tokens of a user (`utils/auth.py:306-328`).
- The user must exist and have the role `user` or `admin` (`utils/auth.py:588-590`).

Thus a local HS256 check accepts a revoked token until the token expires.
A call to `GET /api/v1/auths/` with the token does all checks (`routers/auths.py:185-191`).

## 8. Special handling in Open WebUI

A terminal server must know these behaviours, because they change the requests that it gets.

### 8.1 Browser view tokens

- When a response from a path that starts with `browser/sessions` contains `id` and `token`, Open WebUI replaces `token` with its own signed token (`routers/terminals.py:479-495`).
- The browser then uses `/browser/view/{sid}/{token}/...`. Open WebUI replaces the token with the initial value before it calls upstream (`routers/terminals.py:161-183`).
- Open WebUI keeps the upstream `Authorization` value and cookies in the token, encrypted (`utils/browser.py:9-30`).
- On these routes Open WebUI also copies `accept`, `origin`, `access-control-request-method`, `access-control-request-headers`, `last-event-id`, and `range` (`routers/terminals.py:384-394`).

### 8.2 File preview for template terminals

For a template terminal, `POST files/preview` does not go upstream. Open WebUI makes a read-only token and the browser then gets `files/serve/{path}` (`routers/terminals.py:167-173`, `:401-413`).

### 8.3 Port proxy

For a path that starts with `proxy/{port}/`:

- Open WebUI changes a `Location` header that points to `http://localhost:{port}` (`routers/terminals.py:447-461`).
- Open WebUI changes HTML responses. It changes root-relative links and adds a script (`routers/terminals.py:55-122`, `:496-497`).

## 9. Results for warren

These are conclusions from the facts above. They are inputs to the design. They are not final decisions.

### 9.1 Token verification

1. Open WebUI has no JWKS. The only possible local check is HS256 with `WEBUI_SECRET_KEY`.
2. A component that has `WEBUI_SECRET_KEY` can make a valid token for each Open WebUI user. This increases the damage if warren is compromised.
3. A token gets to a terminal server only when `auth_type` is `session`, and only on paths A, C, D, and G.
4. On a browser WebSocket (path B), `session` mode gives no credential. The upstream sees only `X-User-Id`, `X-Session-Id`, and `user_id`. The agent WebSocket (path G) has the token.
5. On path E, `session` mode uses the connection key and has no user.
6. In `session` mode the bearer value can be an API key (`sk-...`). A local check cannot verify an API key. Only a call to Open WebUI can.

### 9.2 Scopes

1. An orchestrator connection gives the template ID, the user ID, the saved chat ID, and the model ID. All four warren scopes are possible: user, chat, model, and shared.
2. A plain connection gives the user ID and `X-Session-Id` only. User scope and chat scope are possible. Model scope is not possible.
3. An orchestrator connection always uses the connection key. The identity is in headers that the Open WebUI backend sets. Token verification is not possible in this mode.

### 9.3 Header trust

1. Open WebUI makes the identity headers on its server. A browser user cannot change them through Open WebUI.
2. A client that can connect to warren directly, and that has the connection key, can set all identity headers.
3. Thus in `bearer` mode the security of the identity depends on the secrecy of the key and on the network path to warren.

### 9.4 Decisions that are necessary before implementation

| No. | Question | Options |
|---|---|---|
| 1 | Which integration mode is the primary mode? | (a) Orchestrator-compatible: all scopes, key and headers. (b) Plain connection with `session`: token check on HTTP, no model scope, no credential on WebSockets. (c) The two modes. |
| 2 | How does warren authorize a WebSocket in `session` mode? | (a) Accept `X-User-Id` only when the same user had a verified HTTP request a short time before. (b) Do not offer WebSockets in this mode. (c) Use `bearer` mode for all traffic. |
| 3 | Does warren verify tokens locally or with a call to Open WebUI? | (a) Local HS256: fast, needs the secret, no revocation. (b) Call `GET /api/v1/auths/`: no secret, has revocation, accepts API keys, adds latency. (c) Call with a short cache. |
| 4 | Where do the groups for policy selection come from? | (a) `GET /api/v1/users/groups` with the token of the user (`session` mode only). (b) An Open WebUI administrator API key for warren. (c) Policy selection by user ID and template only. |
| 5 | Does warren implement the management API of section 4.6? | Necessary for option 1(a). The API paths contain the word `terminals`, but the paths are part of the wire protocol and are not branding. |
