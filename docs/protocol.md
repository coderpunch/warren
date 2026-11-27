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

#### 4.1.1 The system information view

The browser has a view that shows the system information of an instance: the processor, the memory, the disk, the network, and the processes.

The request:

- The view sends `GET {base}/system/info` through path A. The request has no query string and no body (`src/lib/components/chat/TerminalSystemInfoModal.svelte:35-37`).
- No other code of Open WebUI v0.13.4 sends this request. The request `GET {url}/system` of paths C and E is for a different route.
- Before each path A request for a template terminal, Open WebUI reads the template from the orchestrator (`utils/terminals.py:341`). Thus an orchestrator gets two requests for each request of the view.

The timer:

- The view sends the first request when it opens (`TerminalSystemInfoModal.svelte:94-96`).
- After a request with no error, the view starts a timer of 5000 milliseconds for the next request (`TerminalSystemInfoModal.svelte:47`). Thus the time between two requests is 5 seconds and the time of the reply.
- The view sends no request while the document of the browser is hidden (`TerminalSystemInfoModal.svelte:30`). Each change of the visibility calls the request function (`TerminalSystemInfoModal.svelte:104`).
- When the view closes, it stops the timer and the request that is in progress (`TerminalSystemInfoModal.svelte:97-101`).
- While the first request is in progress, the view shows a wait symbol (`TerminalSystemInfoModal.svelte:33`, `:153-155`). The browser sets no time limit for the request. The limit of 300 seconds of path A applies.

A reply with an error status:

- Open WebUI sends the status and the body of the terminal server to the browser (`routers/terminals.py:478`, `:498-502`).
- For each status that is not 2xx, the browser reads the body as JSON. The error text is the `detail` field if it is a string. If not, it is the `error` field if that is a string. If not, it is a fixed text with the status (`src/lib/apis/terminal/index.ts:231-241`).
- The view shows the line "System info unavailable" and the error text (`TerminalSystemInfoModal.svelte:149-152`). It keeps the data of an earlier reply on the screen: only a reply with no error changes the data (`TerminalSystemInfoModal.svelte:38-40`).
- After an error, the view starts no timer (`TerminalSystemInfoModal.svelte:41-49`). All error statuses have the same effect. The view does not read the `Retry-After` header.
- The view then sends the next request only when the user uses the "Refresh" control, when the visibility of the document changes, or when the user opens the view again (`TerminalSystemInfoModal.svelte:104`, `:127-131`).

Where the user opens the view:

- The instance list of a manager offers the view only for an instance with the status `running` (`src/lib/components/chat/UserTerminalsModal.svelte:278-279`, `:697-705`).
- A source comment at that place says that the orchestrator does not start an instance for the system information (`UserTerminalsModal.svelte:276-277`).
- The terminal menu of the chat input asks for the instance status only when the user manages the terminal (`src/lib/components/chat/MessageInput/TerminalMenu.svelte:277`). It then offers the view only for the status `running` (`src/lib/components/chat/MessageInput/TerminalItemMenu.svelte:32-33`, `:88-93`).
- For a user who does not manage the terminal, the menu does no status check and always offers the view (`TerminalItemMenu.svelte:33`, `TerminalMenu.svelte:281`).
- Thus the first request of the view can come for an instance that is stopped, or that starts.

Section 9.5, decision 8, gives the replies of warren.

#### 4.1.2 Requests that the browser sends with no action of the user

Three timers send path A requests:

| Request | Time | Conditions | Reference |
|---|---|---|---|
| `GET /system/info` | 5 seconds after each reply with no error | Only while the system information view is open and the document is visible. An error stops the timer. | Section 4.1.1 |
| `GET /execute`, and `GET /execute/{id}/status?wait=0&offset=...&tail=1000` for an open process tab | 1 second | Only after the `/execute` WebSocket closed before its first message, with a code that is not 4001 or 4003. Not while the process dock is not active, and not while the document is hidden. An error does not stop the timer. | `src/lib/components/chat/TerminalDock.svelte:122-148`, `:196-202`, `src/lib/apis/terminal/socket.ts:98-102` |
| `PATCH /browser/sessions/{sid}` with the body `{}` | 60 seconds | While a Browser tab exists, also while the document is hidden. The browser ignores the reply and each error. | `src/lib/components/chat/Tabs/Browser.svelte:256-259` |

One function sends each `PATCH /browser/sessions/{sid}` request. The body is a JSON object with the fields that change (`src/lib/apis/terminal/browser.ts:30-42`). The same method and path thus carry the timer and the actions of the user:

| Body | Cause | Reference |
|---|---|---|
| `{}` | The timer of 60 seconds | `Tabs/Browser.svelte:256-259` |
| `{active}` | A change of the tab state. `{active: false}` also when the page goes away and when the tab closes. | `Tabs/Browser.svelte:52-54`, `:250-255`, `:261`, `:284-286` |
| `{url, title}` | A change of the page that the tab shows | `Tabs/Browser.svelte:56-59`, `:168`, `:238` |
| `{mode, url}` | A change of the view mode | `Tabs/Browser.svelte:64-67` |
| `{mode}` | Immediately after the browser made the session | `src/lib/apis/terminal/browser.ts:21-27` |
| `{chat_id}` | The session gets its saved chat | `Tabs/Browser.svelte:34` |

These requests come one time for an event, with no action of the user in the terminal:

- `GET /skills`, when the selected terminal, the terminal list, the chat, or the first model changes (`src/lib/components/chat/Chat.svelte:1339-1350`, `src/lib/apis/terminal/index.ts:464-469`).
- `GET /api/config`, when the selected terminal or the chat changes (`src/lib/components/chat/Tabs.svelte:1565-1573`, `:1602-1625`).
- `GET /files/cwd`, then `GET /files/list` and a maximum of 8 `GET /files/read` requests for the list of recent directories, when the directory menu of the chat input loads (`src/lib/components/chat/MessageInput/WorkingDirectoryMenu.svelte:58-99`, `src/lib/apis/terminal/recentFolders.ts:16-57`).
- `GET /files/cwd` and `GET /agents` from the Open WebUI backend (path D), for each load of the model list while a terminal is selected (`main.py:933-941`, `utils/agents/discovery.py:36-48`).
- `GET /files/list` for the directory that the file browser shows, and one more for each open directory below it, after each `fs:ready` and each `fs:change` message on the `/events` WebSocket (`src/lib/components/chat/FileNav.svelte:1275-1281`, `:1314-1319`, `src/lib/apis/terminal/events.ts:56`, `src/lib/apis/terminal/index.ts:310-315`). The file browser does this only while it shows no file. It does not examine the visibility of the document.
- `GET /git/status` from the git bar, after each of the same messages, only while the document is visible (`src/lib/components/chat/Tabs/GitBar.svelte:195-199`, `:296-303`).

Results for warren:

1. Warren has no special rule for the requests of the last list. Each one is a use of the sandbox, and it starts a stopped sandbox. The browser sends some of them again after an event of Open WebUI, with no person (section 4.1.3), and before a WebSocket connects again (section 4.2.3). A program in the sandbox causes the last two when it changes a file (section 4.2.4).
2. The system information is different: a view that stays open sends requests with no end, and the data are not proof that a person uses the sandbox. Decision 8 of section 9.5 is the rule of warren for it.
3. Warren has no rule for the timer of the Browser tab. Each such request is a use of the sandbox, and it starts a stopped sandbox. A forgotten Browser tab thus keeps its sandbox with no end.
4. A new sandbox does not know the browser session of the previous pod, thus the reply to the timer is then status 404 (`open-terminal-api.md`, section 3.7). The browser ignores the reply, and the timer continues.
5. A rule for the method and the path cannot be the remedy: the same method and path carry the actions of the user. A rule must read the body. Open WebUI sends the body as a stream (`routers/terminals.py:424-425`), thus the `Content-Length` header is not a sufficient test.

#### 4.1.3 Requests after the event `terminals:changed`

Open WebUI sends the event `terminals:changed` to the browsers when the terminals change. Each page that gets the event sends requests for its selected terminal, with no action of a person.

When Open WebUI sends the event:

- After each instance operation: restart, stop, start, reset, and delete. Open WebUI waits for the reply of the orchestrator, and then it sends the event (`routers/templates.py:194-204`, `:207-229`). Only a user with write access to the terminal can send an instance operation (`routers/templates.py:195`).
- After the create, the update, and the delete of a template, and after a change of the access grants of a template (`routers/templates.py:93`, `:107`, `:117`, `:144`).
- After a change of a tether device, to the user of that device only (`routers/tether.py:25-31`).

Who gets the event:

- The user that made the change, and each user that has a read grant for the terminal (`routers/templates.py:45-51`).
- A grant for a group gives the event to each member of the group. A grant for all users gives it to each user that is not pending (`models/access_grants.py:684-712`).
- The event goes to each open page of these users (`socket/main.py:523-534`).

What a page does with the event:

- The handler is in the layout of all application pages. It reads the terminal list, the model list, and the tool list again (`src/routes/(app)/+layout.svelte:166-171`). The handler does not examine the visibility of the document, thus a hidden page does the same.
- The read of the terminal list goes to the orchestrator only (section 4.6).
- The store of the terminal list gets a new list at each read (`+layout.svelte:110-116`). The chat page sends `GET /skills` for the selected terminal when that store changes (section 4.1.2). Inference: a new list with the same content is also a change. No person read the source of the Svelte library for this document.
- The read of the model list sends `GET /api/models` to Open WebUI. The request has the parameter `terminal_id` when the page has a selected terminal, and `chat_id` when a chat is open (`src/lib/apis/index.ts:7-31`). The selection is in the storage of the browser, thus each page of the user has it (`+layout.svelte:273-278`, `:415-423`).
- For each such request, the Open WebUI backend sends `GET /files/cwd` and then `GET /agents` to the terminal server on path D, for the user of the page (`main.py:933-941`, `utils/agents/discovery.py:36-48`, `utils/terminals.py:408`). It keeps no result of an earlier request.
- A reply with an error status gives no data and no error (`utils/terminals.py:476`). Thus Open WebUI sends `GET /agents` also when `GET /files/cwd` fails. The time limit of each request is 60 seconds by default (`env.py:662-665`, `utils/terminals.py:457`). Open WebUI ignores each failure (`main.py:940-941`).
- For a template with chat scope and no saved chat, Open WebUI sends no request to the terminal server (`utils/terminals.py:215-216`, `utils/agents/discovery.py:49-50`).

Two more events have a part of this effect:

- When the socket of a page connects again, the page reads the terminal list again (`+layout.svelte:175`). It does not read the model list.
- The event `access:updated` goes to all pages when an administrator saves the default permissions (`routers/users.py:442-449`). Each page then reads the model list and the terminal list again (`+layout.svelte:129-157`, `:172`).

Results for warren:

1. `GET /skills`, `GET /files/cwd`, and `GET /agents` are usual requests in warren. Each one is a use of the sandbox, and it starts a stopped sandbox. Warren cannot tell such a request from a request that a person caused.
2. Thus a stop, a reset, or a delete of an instance does not hold while a page has the terminal selected. Open WebUI sends the event immediately after the operation. A request that comes in the termination time of the pod waits for the end of the pod, and then it starts a new pod. After a delete, the new pod gets a new, empty volume.
3. No close code of a WebSocket can change this: the requests are HTTP requests, and they need no open WebSocket. This was also the behaviour of warren before decision 9 of section 9.5.
4. One operation of a manager, or one change of a template, makes each page of each user with a read grant send the requests. For a template with user scope, each such user with an open page and a selected terminal gets a sandbox that starts. Only a person who manages the terminal can cause this. The limits on the number of sandboxes are the only protection of warren against it.
5. Warren has no time after a stop in which it refuses requests. Such a time is a possible later change. It needs a decision of the owner of the project, because warren then also refuses the request of a person in that time.

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
For each other handshake status, and when Open WebUI cannot connect, the close code is 1011 (`routers/terminals.py:760-765`).

When the terminal server closes the WebSocket, Open WebUI closes the WebSocket of the browser with the close code of the terminal server (`routers/terminals.py:728-729`):

- Open WebUI changes only the codes 1005, 1006, and 1015 to 1011. A close with no code also gives 1011.
- Each other code goes to the browser with no change, for example 1000, 1011, 1012, 1013, and each code from 4000 to 4999.
- Open WebUI does not send the reason text of the terminal server to the browser.

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
When the terminal server closes the WebSocket, the rule of section 4.2.1 applies: the close code goes to the browser with no reason text, and the codes 1005, 1006, and 1015 become 1011 (`routers/terminals.py:197-206`).

#### 4.2.3 Browser retry rules

Each client of a WebSocket has its own rule for the close code. No client has a limit for the number of attempts, unless the table gives one.

| Client | Route | Close codes after which it stops | What it does after each other close code | Reference |
|---|---|---|---|---|
| Interactive terminal | `/api/terminals/{session_id}` | 1000, 4001, 4003, 4004 | It connects again after 1 second. The delay becomes two times as long after each attempt, to a maximum of 10 seconds. Before each attempt it sends `GET /api/terminals/{session_id}` on path A. | `src/lib/components/chat/XTerminal.svelte:40`, `:109-113`, `:202-224` |
| File browser, git bar, and port list | `/events` | 4000 to 4999 | It connects again after 1 second, also after the code 1000. The delay becomes two times as long after each attempt, to a maximum of 30 seconds. It sends no HTTP request before an attempt. | `src/lib/apis/terminal/events.ts:13`, `:29-35` |
| Process list and process output | `/execute`, `/execute/{process_id}/status` | 1000, and 4000 to 4999 | The same delays as for `/events`. | `src/lib/apis/terminal/socket.ts:76`, `:95-107` |
| Desktop | `/desktop/stream` | 4000 to 4999 | It connects again after 2 seconds, while the tab is active. Before each attempt it sends `GET /desktop` on path A. | `src/lib/components/chat/Tabs/Desktop.svelte:63-80`, `:126-147` |
| Desktop audio | `/desktop/audio` | Each code | It does not connect again. | `src/lib/components/chat/Tabs/DesktopAudio.svelte:40-43`, `:94` |
| Browser view | `/browser/view/{sid}/{token}/stream` | 4000 to 4999 | It connects again after 1 second, and then after 2 seconds. The third close in sequence stops it. | `src/lib/components/chat/Tabs/ChromeBrowser.svelte:252-274` |
| Agent session (the Open WebUI backend, path G) | `/agents/sessions/{id}/connect` | None: it does not read the code. | It connects again after 1 second. It stops when it got no message for more than 60 seconds. | `utils/agents/transport.py:73-135` |

More facts:

- Open WebUI has no special rule for the code 1012: the code does not occur in its source. Each client uses it as each other code that is not 1000 and not from 4000 to 4999.
- When the HTTP request of the interactive terminal fails, the terminal does not try again (`XTerminal.svelte:229-237`). For status 404, the tab shows the text "Session no longer available" and the control "Start new terminal". For each other status, the tab shows the error text and the control "Retry" (`src/lib/components/chat/Tabs.svelte:2145-2176`).
- A new pod does not know the terminal sessions of the previous pod, thus its reply to that HTTP request is status 404 (`open-terminal-api.md`, section 3.4).
- When the HTTP request of the desktop fails, the desktop does not try again. The tab shows the error text (`Tabs/Desktop.svelte:139-143`).
- The process list has one more rule. If its WebSocket closes before the first message, with a code that is not 4001 or 4003, the client does not connect again. It then sends HTTP requests each second (section 4.1.2).

Facts about the clients of the `/events` and `/execute` routes:

- The file browser, the git bar, and the port list each open a WebSocket on `/events` (`src/lib/components/chat/FileNav.svelte:1305-1325`, `src/lib/components/chat/Tabs/GitBar.svelte:285-310`, `src/lib/components/chat/FileNav/PortList.svelte:33-58`). Each one opens it when the page shows that part. No action of a person in the terminal is necessary.
- Before each of these parts opens its WebSocket, it sends `GET /api/config` on path A (`FileNav.svelte:1312`, `Tabs/GitBar.svelte:286`, `FileNav/PortList.svelte:41`). The browser keeps no copy of the reply: each call sends the request (`src/lib/apis/terminal/index.ts:121-133`). The client sends no such request before it connects again (`src/lib/apis/terminal/events.ts:29-35`).
- The file browser also sends `GET /files/cwd` on path A when the page makes it, unless it shows a file first (`FileNav.svelte:2262-2266`).
- The process dock opens the WebSocket on `/execute` when the page makes the dock (`src/lib/components/chat/TerminalDock.svelte:181-204`). The dock is a part of each file browser of the Files tab (`FileNav.svelte:3227-3254`). The page makes a new dock only when the chat, the terminal, or the model changes (`FileNav.svelte:3243`).
- The dock opens the WebSocket on `/execute/{process_id}/status` for the process that it shows, while the dock is active and open (`TerminalDock.svelte:54-76`).
- On `/events`, the client sends the first message and one `subscribe` message (`events.ts:15-17`). On the two `/execute` routes, it sends only the first message (`src/lib/apis/terminal/socket.ts:38-62`, `:82`). The client sends no other message on these three routes.
- The terminal server sends the process list on `/execute` immediately after the first message (`open-terminal-api.md`, section 3.2). Thus the rule for a close before the first message applies only when the WebSocket gets no data from a terminal server.
- After a code from 4000 to 4999, the process list does not connect again, and it sends no HTTP requests. The dock has a mark that says that the WebSocket operates. While the mark is set, the function for the HTTP requests returns immediately (`TerminalDock.svelte:48`, `:123`). Only the rule for a close before the first message removes the mark (`TerminalDock.svelte:196-202`).
- The dock then shows the text "Live updates unavailable" (`socket.ts:103`, `TerminalDock.svelte:193-195`, `:291-296`). The page hides a dock that has no process (`TerminalDock.svelte:80`, `FileNav.svelte:3229`). Thus the page does not show a command that the model runs after that time.
- The stream of the process output has no rule for a close before the first message (`TerminalDock.svelte:66-74`).
- After a code from 4000 to 4999, the file browser reads its directory again in two conditions only: the chat tells it that a tool wrote a file or ran a command while its watcher has no connection, and the document becomes visible (`src/lib/components/chat/Chat.svelte:1606-1611`, `FileNav.svelte:1318`, `:2257-2258`, `:2305-2316`). The git bar reads its status again only when the document becomes visible (`Tabs/GitBar.svelte:480-482`).
- The port list sends no HTTP request for the ports when the terminal server has the events function. Its only data source is then the WebSocket (`FileNav/PortList.svelte:41-57`).

Results for warren:

1. Each attempt is a new WebSocket upgrade or a new HTTP request at warren.
2. An HTTP request is a usual request: it starts a stopped sandbox. The interactive terminal and the desktop send one before each attempt.
3. If each WebSocket upgrade started a stopped sandbox, a page with the file browser would start the sandbox again 1 second after each close with the code 1011, 1012, or 1013, with no person. Only a code from 4000 to 4999 stops the clients of the `/events` and `/execute` routes.
4. After a code from 4000 to 4999, these clients stay stopped until the page makes them again, for example when the user changes the chat or opens the tab again. The process list and the port list then show no new data, also when the sandbox operates again.
5. Thus no one close code is correct for these clients while each upgrade starts a sandbox. Warren has a different rule for the three routes `/events`, `/execute`, and `/execute/{process_id}/status`: a WebSocket upgrade on them does not start a sandbox. It waits for a start that a different request or operation began. Warren can then close such a WebSocket with a code after which the client connects again. Decision 10 of section 9.5 is this rule.
6. A WebSocket upgrade on each other route is a usual request: it starts a stopped sandbox.
7. The wait of rule 5 must be long. When the page makes the file browser, the HTTP requests of the list above start the sandbox at the same time. The WebSocket of the process list must stay open until the sandbox is ready: a close before the first message starts the HTTP requests of section 4.1.2 each second, with no end, and each one is a usual request.

#### 4.2.4 Messages that a client sends with no action of a person

After the first message, the clients send these messages on a WebSocket:

| Client | Message | When | Reference |
|---|---|---|---|
| Interactive terminal | Text `{"type":"ping"}` | Each 25 seconds while the WebSocket is open. No other client in the browser has such a timer. | `src/lib/components/chat/XTerminal.svelte:128-130` |
| Interactive terminal | Text `{"type":"activity","active":B}` | After each connect, and when `B` changes. `B` is true only while the page shows the terminal tab and the document is not hidden. | `XTerminal.svelte:61-68`, `:74`, `:124`, `:241`, `:244`, `src/lib/components/chat/Tabs.svelte:196`, `:2184` |
| Interactive terminal | Text `{"type":"resize","cols":N,"rows":N}` | After each connect, and when the size of the terminal changes while the tab is active. | `XTerminal.svelte:127`, `:284-287` |
| Interactive terminal | Text `{"type":"ack","cursor":N}` | After the terminal wrote a group of output to the screen, when the terminal server confirmed flow control. The client always asks for flow control. | `XTerminal.svelte:132`, `:155-158`, `:180-188` |
| Interactive terminal | Text `{"type":"chat","chat_id":ID}` | One time, when a terminal gets its saved chat. | `XTerminal.svelte:44-57`, `src/lib/apis/terminal/socket.ts:3-36` |
| Interactive terminal | Binary | The data that the terminal library gives as input: the keys of the person, and a paste. | `XTerminal.svelte:277-283` |
| File browser, git bar, and port list | Text `{"type":"subscribe", ...}` | One time after each connect. | `src/lib/apis/terminal/events.ts:15-17` |
| Process list and process output | None | | `src/lib/apis/terminal/socket.ts:82-94` |
| Desktop, transport `frames` | Text `{"type":"frameAck","window":3}` | After each picture of the screen that the client drew. | `src/lib/components/chat/Tabs/Desktop.svelte:81-93` |
| Desktop, transport `vnc` | Binary | The VNC library of the browser uses the WebSocket. | `Tabs/Desktop.svelte:100-105` |
| Desktop audio | None | | `src/lib/components/chat/Tabs/DesktopAudio.svelte:86-94` |
| Browser view, `stream` | Text messages of the types `frames`, `quality`, `visibility`, and `viewport` | After each connect. | `src/lib/components/chat/Tabs/ChromeBrowser.svelte:104-106`, `:200-207` |
| Browser view, `stream` | Text `{"type":"frameAck","window":3}` | After each binary message of the terminal server. | `Tabs/ChromeBrowser.svelte:226-230` |
| Agent session (the Open WebUI backend) | Text `{"type": "ack", "sequence": N}` | After each output message of the agent. | `utils/agents/transport.py:109-115` |

Facts about these messages:

- Open WebUI sends each text or binary message of the browser that is not empty to the terminal server with no change. It drops an empty message (`routers/terminals.py:700-710`).
- Open WebUI relays only text and binary messages from the terminal server (`routers/terminals.py:715-727`). It opens the WebSocket to the terminal server with no heartbeat (`routers/terminals.py:674-678`). Thus no ping frame and no pong frame of the WebSocket protocol goes through Open WebUI on these routes. The agent WebSocket of path G is different (section 4.7).
- In each object that a client makes, `type` is the first member. The browser makes the text with no white space. The Open WebUI backend makes it with one space after each `:` and each `,` (`utils/agents/transport.py:65-66`).
- The interactive terminal reports `active` as false on a tab that the page does not show, and in a hidden document, for example a browser tab in the background (`XTerminal.svelte:63`).
- The `ack` message comes from output of the sandbox, and not from a person. The terminal sends it only after it wrote the output to its screen (`XTerminal.svelte:180-188`). No person read the source of the terminal library to find out if that occurs in a hidden document.
- The terminal copies the value `end` of an `output` message of the terminal server into `cursor` with no check, and it sends that value in the `ack` message (`XTerminal.svelte:160`, `:181`, `:187`). Thus a terminal server controls the value, and with it the size, of the `ack` message.
- The desktop does not close its WebSocket when the document becomes hidden: its condition is the visible tab of the page only (`src/lib/components/chat/Tabs.svelte:196`, `:2134`, `Tabs/Desktop.svelte:161-164`).
- Each 10 seconds, Open WebUI makes sure that the caller continues to have access (section 4.2.1, step 12). That check sends a request to the orchestrator only (section 4.6), and no message on the WebSocket.

A terminal server can make a client send requests and messages. No person is necessary:

| What the terminal server does | What the client does | Reference |
|---|---|---|
| It closes a WebSocket with a code after which the client connects again (section 4.2.3). | The client connects again. The interactive terminal first sends `GET /api/terminals/{session_id}`, and then the `activity` and `resize` messages on the new WebSocket. The desktop first sends `GET /desktop`. | `XTerminal.svelte:109-127`, `:206`, `:221-223`, `Tabs/Desktop.svelte:70-74`, `:131-137` |
| It closes the `/events` WebSocket. | The client connects again after each code that is not from 4000 to 4999, and sends `subscribe`. The delay goes back to 1 second after each message that the client got, thus the terminal server controls the times. | `events.ts:15-17`, `:22`, `:29-35` |
| It sends `fs:ready` or `fs:change` on `/events`. A change of a file in the directory that the page watches causes `fs:change` (`open-terminal-api.md`, section 3.12). | The file browser sends `GET /files/list` requests, and the git bar sends `GET /git/status` (section 4.1.2). | Section 4.1.2 |
| It sends output on the terminal WebSocket. | The interactive terminal sends `ack`. | `XTerminal.svelte:180-188` |
| It sends output with a query for the terminal, for example for the position of the cursor. | Inference: the terminal library answers through its input event, and the client sends the answer as a binary message. Open Terminal has a rule for input that "the terminal itself" sends (`open-terminal-api.md`, section 3.4). No person read the source of the terminal library for this document. | `XTerminal.svelte:277-279` |
| It sends a picture of the screen (transport `frames`), or data of the VNC protocol. | The client sends `frameAck`. Inference for the VNC protocol: the client answers with binary data. No person read the VNC library for this document. | `Tabs/Desktop.svelte:81-105` |
| The agent process sends a request to its client. | The Open WebUI backend answers with an `input` message (section 4.7). | Section 4.7 |

Results for warren:

1. An open WebSocket is not proof that a person uses the sandbox, and a message on it is not always such proof. Decision 11 of section 9.5 is the rule of warren for the idle time.
2. The `ping` message, the `frameAck` message, and the `ack` message are automatic. Warren finds them from the first bytes of the message, because `type` is the first member in each message of a client.
3. The rule for an automatic message must not use the size of the message: a terminal server controls the size of the `ack` message.
4. The `activity` message is the only signal that tells a terminal that a person looks at from a forgotten terminal. Warren reads it, and it counts an `ack` message as a use only while the terminal is visible.
5. Warren cannot prevent that a sandbox with a connected client keeps itself in operation: the table above gives the methods. The idle time is thus a rule for resources, and not a protection against a hostile sandbox.

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

Facts about the WebSocket of an agent session:

- Its only client is the Open WebUI backend. The WebSocket routes of Open WebUI for the browser are the fixed list of section 4.2, and no route of that list goes to `agents/`.
- The backend opens the WebSocket at the start of one chat turn of an agent, and it closes the WebSocket at the end of that turn (`utils/agents/runner.py:161-180`, `utils/agents/transport.py:59-63`).
- The backend sends an `input` message for the prompt (`utils/agents/transport.py:141-152`). After the first message of each new connection, it sends again each `input` message that the terminal server did not confirm (`utils/agents/transport.py:96-98`).
- After each output message of the agent, the backend sends an `ack` message (`utils/agents/transport.py:109-115`).
- When the agent process sends a request to its client, the backend answers with an `input` message. Examples are a request for the permission to use a tool, and a request with a method that the backend does not know (`utils/agents/acp.py:106-124`). In the default mode of a chat, the backend gives the permission with no person (`utils/agents/output.py:154-168`, `utils/tool_permissions.py:116-119`, `utils/agents/runner.py:157`).
- The backend sends a ping frame of the WebSocket protocol each 20 seconds (`utils/agents/transport.py:91`).
- In the turn, the backend sends no HTTP request to the terminal server. Its HTTP requests are the start and the stop of the session (`utils/agents/transport.py:60`, `:184`).
- The backend has no time limit for a turn (`utils/agents/acp.py:238`).
- The backend does not read the close code. After each close it connects again after 1 second, until it got no message for more than 60 seconds (`utils/agents/transport.py:100-126`). The turn then ends with the error text "Agent environment disconnected. Reconnect before continuing this chat."
- At the end of the turn, also after a failure, the backend sends `DELETE {url}/agents/sessions/{id}` (`utils/agents/transport.py:182-184`).

Results for warren:

1. No message of the backend shows that an agent works: in a long step of the agent, the backend sends only `ack` messages, or nothing. An idle rule that counts only use messages would stop the sandbox in the middle of a turn.
2. A new pod does not know the agent session of the previous pod (`open-terminal-api.md`, section 3.9). Thus a stop in a turn loses the work of the agent, and the backend then starts a new, empty sandbox with its next connection or with its `DELETE` request.
3. No forgotten page of a browser can keep this WebSocket open. Thus warren uses the open WebSocket of an agent session as a use of the sandbox, as an HTTP request in progress (decision 11 of section 9.5).

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
6. `GET /system/info` has the browser limit of 300 seconds, and each error status stops the timer of the view (section 4.1.1). Thus this request must wait for a sandbox that starts: an error reply in that time stays on the screen. The request itself must never start a sandbox (section 9.5, decision 8).

### 9.5 Decisions

The owner of the project made the decisions 1 to 6 on 2026-11-25. The decisions 7 to 11 are from 2026-11-27. The rule for all decisions: warren implements only the current protocol, and no older or optional protocol.

| No. | Subject | Decision |
|---|---|---|
| 1 | Integration mode | Warren accepts only an orchestrator connection from Open WebUI 0.13.4 or later. It does not accept a plain connection. |
| 2 | Authentication of Open WebUI | The connection key, as section 5 shows for `bearer`. Each request and each WebSocket must have the key. |
| 3 | Identity of the user | The headers that Open WebUI sets on its server: `X-User-Id` and the `X-Terminal-*` headers (section 6.1). |
| 4 | Verification of a user token | Not in warren. The current protocol sends no user token on an orchestrator connection (section 9.1, item 9). |
| 5 | Groups | Warren has no group data. An administrator gives a template to users and groups with the access grants of Open WebUI. |
| 6 | Management API | Warren implements the template read calls, the status call, and the instance calls. Templates are read-only. An administrator defines them in the warren configuration. |
| 7 | Limit variables | Warren sets four variables on the container of each sandbox, from the limits of the policy. `OPEN_TERMINAL_CPU_LIMIT` and `OPEN_TERMINAL_MEMORY_LIMIT` have the form in which Kubernetes shows a quantity, for example `500m` and `2Gi`. `OPEN_TERMINAL_CPU_COUNT` is the CPU limit as a whole number, rounded up, with a minimum of 1. `OPEN_TERMINAL_MEMORY_BYTES` is the memory limit as a whole number of bytes. A policy cannot set these variables. The note below the table gives the source. |
| 8 | System information | Warren sends `GET /system/info` only to a sandbox that operates. For a stopped sandbox, the reply is status 409, and the sandbox does not start. The request is not activity: it does not change the idle time of the sandbox. The note below the table gives the details. |
| 9 | WebSockets of a sandbox that stops | When warren stops a sandbox, it closes each open WebSocket of that sandbox. For a stop, restart, reset, or delete operation of the management API, the client gets the close code 1012. The note below the table gives the codes for the other reasons, and the details. |
| 10 | WebSockets that only watch a sandbox | A WebSocket upgrade on `/events`, `/execute`, or `/execute/{process_id}/status` does not start a sandbox. It waits for a start that a different request or operation began. These routes have their own close code for the idle time. The note below the table gives the details. |
| 11 | Use of a sandbox through a WebSocket | An open WebSocket is not a use of the sandbox. A message of the client is a use, unless it is a keepalive `ping` or a different automatic message. A message of the sandbox is not a use. Thus warren stops a sandbox for its idle time also while WebSockets are open. The WebSocket of an agent session is the exception. The note below the table gives the details. |

Because the identity is in headers, warren must have these protections:

1. Warren does not start without a connection key. The key must have a minimum of 32 characters.
2. Warren compares the key in constant time.
3. Warren can serve TLS 1.3, so that the key does not go through the cluster network as clear text.
4. A NetworkPolicy permits connections to warren only from Open WebUI.
5. Warren never sends the connection key to a sandbox. Each sandbox has its own random key.
6. Warren logs a warning at start-up. The warning says that warren trusts the identity headers of each client that has the key.

Note: the management API paths contain the word `terminals`. The paths are part of the wire protocol and are not a brand name.

Note for decision 7: the four names are not in the source code of Open WebUI v0.13.4. Their only source is the public documentation, in the repository `github.com/open-webui/docs` at the commit `ee47d77ed76357560568dd5a1b0898fd514e96d3` (2026-11-26):

- The page `docs/features/open-terminal/terminals/orchestration/environment-variables.md`, lines 45-54, gives the four names. It says that the orchestrator sets a variable for each CPU or memory field of a terminal that has a value.
- The page says that the two `LIMIT` variables come from the fields, and that the orchestrator calculates the count and the number of bytes from the fields. It gives no form of a value, no example of a value, and no rule to round a number.
- An earlier text of the same lines, at the commit `14a0f2f7bdbe5d59a4cf975d984bf6b43a178dba`, says that the variables come from the CPU limit and the memory limit of a policy, and that a user does not set them. It also gives no form of a value.

Thus the forms of decision 7 are a decision of warren. Warren gives each policy the two limits, thus it always sets the four variables.

Note for decision 8: the owner of the project asked for the three rules of the table row, because the view of section 4.1.1 sends its request each 5 seconds. The details are decisions of warren:

- The rule is for a request that is not a WebSocket upgrade, and whose decoded path is exactly `/system/info`. The query string has no effect.
- Open Terminal has only the GET method on this path. For each other method, warren replies with status 405 and the header `Allow: GET`. No method on this path starts a sandbox.
- The checks of the key, the path, the identity, the policy, the context, and the user list come first, with their usual replies. The status 409 is thus also the reply for a chat scope with no saved chat. The `detail` text is different.
- A stopped sandbox has no pod and no start in progress, or its pod is in deletion or in a final phase, or a reset or a delete operation is in progress. The reply is status 409 with the body `{"detail":"The terminal is stopped. It starts when you use it."}`. Open WebUI shows this text to the user (section 4.1.1).
- The request waits when a different request or operation started the sandbox and the pod is not ready. It also waits while a restart operation is in progress, from the stop of the old pod to the start of the new pod: the operation starts the sandbox again (decision 9). It waits a maximum of 50 seconds, as each request. At the end of that time, and while warren is not ready, the reply is status 503 with the header `Retry-After: 5`. An immediate error is not correct here: it stops the timer of the view, and the error stays on the screen after the sandbox is ready.
- If the sandbox stops in that time, the reply is status 409 immediately.
- In the full time of the wait and of the relay, the request is not a use of the sandbox. Warren can stop the sandbox, also for the idle time, while the request is in progress. The reply is then status 502 or 504.
- The limits on the number of sandboxes do not apply: the request starts nothing.
- The first request for a pod makes the checks of `isolation.md`, section 7, before warren sends data to the pod. This request makes them also. They can stop a pod, with the same reasons as for each request.

Note for decision 9: the owner of the project asked for the close code 1012 at a stop, a restart, a reset, and a delete of the management API, so that the client connects again. Before this decision, warren closed no WebSocket because of a stop. The client got the close code that the sandbox sent when it stopped, or the code 1011 when the connection broke. A sandbox that stops usually sends the code 1012 with no text (`open-terminal-api.md`, section 4.3). The details are decisions of warren:

- The table of `policies.md`, section 10.2, gives the close code and the reason text for each stop reason, and for each class of route (decision 10). The code is 4004 for a removed policy, because a new connection gets that code from the route check. It is 1012 for a removed network permission, for a volume that is gone, and for a pod that a different actor deletes: the sandbox can start again. It is 1011 for each other reason.
- Warren also closes the WebSockets of a pod that a different actor deletes, for example a person with kubectl. Warren sees that in its pod cache.
- Warren tells the WebSockets before it sends the delete request to Kubernetes. If the sandbox side closes first, and warren stopped the sandbox, the client gets the code of warren and not the code of the sandbox. If Kubernetes refuses the delete request, the WebSockets are closed, and the pod continues.
- The sandbox side of the WebSocket gets the code 1001, as at a stop of the warren process.
- Open WebUI does not send the reason text to the browser (section 4.2.1). The text is for other clients.
- The audit record of the WebSocket has the code and the stop reason. The record for a stop of the warren process has the code 1012.
- An HTTP request in progress gets no signal. A WebSocket that has no connection to its pod at the time of the stop gets the usual code for a connection that fails.
- An open WebSocket is not a use of the sandbox (decision 11). Thus warren stops a sandbox for its idle time also while WebSockets are open, and they get the code for the idle time. That code is the only code that is different for the two classes of routes (decision 10).
- The close code does not make a stop hold. After the code 1012, the clients connect again (section 4.2.3), and Open WebUI sends requests by itself after each instance operation (section 4.1.3). Each HTTP request, and each new WebSocket on a viewer route, starts the sandbox again. A new WebSocket on a watcher route does not (decision 10).
- While a restart operation is in progress, only the operation starts the sandbox. A request in that time waits for the start of the operation. Without this rule, a client that connects again can send the start request before the operation. The sandbox then goes on the account of the user of that client, or that user gets the error of the sandbox limit. A request for the system information also waits in that time (decision 8).

Note for decision 10: the owner of the project did not ask for this rule. It is a decision of warren, and it comes from the review of decision 9. Section 4.2.3 gives the cause: the clients of the three routes connect again with no limit after each close code that is not from 4000 to 4999, and they stay stopped after a code from 4000 to 4999. With one rule for all routes, the code 1012 lets an open page start a stopped sandbox again with no person, and the code 4004 leaves the process list and the port list of the page with no new data after the next start. The details:

- The class of a route comes from the decoded path of a WebSocket upgrade. The watcher routes are exactly `/events`, `/execute`, and `/execute/{process_id}/status`, with one path segment for the process ID. Each other WebSocket route is a viewer route. The query string has no effect. An HTTP request has no class: it is a usual request, also on these paths.
- A WebSocket on a watcher route does not begin the start of a sandbox. Warren accepts the WebSocket, and waits for a start that a different request or operation began. When the pod is ready, the watcher gets its connection.
- The wait has the usual maximum of 50 seconds, and not a shorter time (section 4.2.3, result 7). At the end of that time, warren closes the WebSocket with the code 1013, as for each sandbox that is not ready. Open WebUI sends that code to the browser with no change (section 4.2.1), and the clients connect again after it.
- Warren does not refuse the upgrade in place of the wait. Open WebUI changes each handshake status that is not 401, 403, or 404 to the close code 1011, and it writes an error with a stack trace to its log (section 4.2.1, `routers/terminals.py:760-765`). The process list then gets a close before its first message immediately.
- In the wait, the watcher is not a use of the sandbox, and it sends no delete request for a pod that failed. The limits on the number of sandboxes do not apply: the watcher starts nothing.
- On a sandbox that operates, a watcher is a usual WebSocket. It makes the checks of `isolation.md`, section 7, when it is the first request for a pod.
- The close codes of the two classes are different only for the idle time: 4004 on a viewer route, and 1012 on a watcher route (`policies.md`, section 10.2). After the code 1012 for an operation of the management API, the watchers connect again and start nothing. Thus that code does not start the sandbox again through a watcher.
- A watcher cannot send the start request before a restart operation does. The rule of decision 9 for a restart operation stays necessary for the HTTP requests and for the viewer routes.
- `policies.md`, section 10.3, gives the limits of this rule. The most important one: the process list of Open WebUI sends HTTP requests each second when its WebSocket closes before the first message (section 4.1.2). That occurs when the sandbox is not ready at the end of the wait.

Note for decision 11: the owner of the project asked that warren counts the messages on a WebSocket as activity, and not the open connection, and that it ignores the keepalive pings of the client. If not, a page that a person forgot keeps its sandbox with no end. Before this decision, each open WebSocket was a use of the sandbox. The details are decisions of warren, and `policies.md`, section 10.4, gives them for an administrator:

- The owner named no direction. Only a message from the client to the sandbox can be a use. A message of the sandbox is never a use: the output of a program is not proof that a person uses the sandbox. With the two directions, a forgotten page with a program that prints, or with the audio of a desktop, keeps its sandbox.
- This rule is not a security property. While a client has a connection, a sandbox can make that client send requests and messages (section 4.2.4). Warren does not try to prevent that.
- The owner named only the keepalive pings. Warren also does not count the `frameAck` message, and it counts the `ack` message only for a visible terminal: the client sends them after data of the sandbox, with no person (section 4.2.4).
- A text message of a client is automatic if and only if its first bytes are, in this sequence: optional JSON white space, `{`, optional white space, `"type"`, optional white space, `:`, optional white space, and then exactly `"ping"`, `"ack"`, or `"frameAck"`. Warren examines a maximum of the first 64 bytes. The size of the message has no effect, because a sandbox controls the size of the `ack` message of the terminal (section 4.2.4). Each other text message, and each binary message, is a use message.
- Warren reads the `activity` message of the interactive terminal: a text message of a maximum of 64 bytes, a JSON object with the type `activity` and a boolean `active`. An `ack` message is a use only while the last such message of the WebSocket had the value true. Thus output in a visible terminal keeps the sandbox, and output in a terminal that nobody can see does not.
- The start of a WebSocket that gets a connection to the sandbox is a use, as each request. The end of a WebSocket is not a use. The frames of the WebSocket protocol (ping, pong, close) are not a use.
- A WebSocket upgrade whose decoded path is `/agents/sessions/{id}/connect`, with one path segment for the ID, is different: the open WebSocket is a use, as an HTTP request in progress, and its end is a use. Section 4.7 gives the cause. It gets the stop signal of decision 9 for each other stop reason.
- Warren records the time of a use message a maximum of one time in 10 seconds for each WebSocket, and only in memory. Thus a stream of messages does not cost one lock for each message, and no message causes a request to Kubernetes.
- At the idle stop, a WebSocket on a viewer route gets the close code 4004, and a WebSocket on a watcher route gets the code 1012 (decisions 9 and 10). After the code 4004, the clients of Open WebUI stop: the terminal tab shows "Session no longer available" (section 4.2.3). Open WebUI does not send the reason text, thus the page does not show that the idle time was the cause.
- Warren does not examine the data of the VNC protocol. On the desktop route and on the browser view route with that transport, each message of the client is binary, and each one is a use message. `policies.md`, section 10.4, gives this limit and the other limits.
