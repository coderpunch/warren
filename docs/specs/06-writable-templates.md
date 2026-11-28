# Specification 06: templates that a user makes in Open WebUI

Status: proposal. The owner decided on 2026-11-28 that warren gets this function. The design and the open items of section 13 need the approval of the owner.

This work changes decision 6 of `protocol.md`, section 9.5: "Templates are read-only."
It also moves a trust boundary. Refer to section 9.

## 1. Purpose

In the enterprise orchestrator, a user makes and changes a terminal in the Open WebUI user interface (`feature-parity.md`, section 9).
In warren, only an administrator defines policies, in a file. The make, change, and delete calls of Open WebUI get the status 403 (`internal/api/api.go`, `readOnly`).

The public documentation gives these facts (`docs.openwebui.com/features/open-terminal/terminals/orchestration/` and its page `creating-terminals`, read on 2026-11-28). The current documentation uses the word "machine" for a terminal.

- The connection has two permissions. Read lets a user see and use the terminals that a person shared with that user. Write lets a user make and manage terminals on the server, and see each terminal on it.
- A new terminal is private. The person who made it shares it with users and groups. An administrator can make it public.
- The dialog has these fields: name, scope, start directory, setup script, image, CPU, memory, storage, network, and environment variables. An empty field uses the default of the server. The orchestrator examines the values against the maximums of the server.
- The scope is locked while instances exist.
- The delete of a terminal removes the terminal, its instances, and its files.
- The server settings are the maximum CPU, memory, and storage, a list of image patterns, and named network profiles. An empty image list permits each image.
- A terminal can have its own idle time, maximum lifetime, and reset schedule. An administrator sets them in the interface of the orchestrator, not in the dialog of Open WebUI.

This work adds templates that Open WebUI makes with the management API. An administrator sets the limits for them in the policy file.
The policies of the file stay. They stay read-only.

## 2. Words

| Word | Meaning |
|---|---|
| File policy | A policy of the policy file. It is the only type that exists at this time. |
| User template | A template that Open WebUI made with the management API. Warren keeps it in a ConfigMap. |
| Template rules | The key `templates` of the policy file. It says what a user template can have. |

For the proxy and the backend, a user template is a policy. Only its source is different.

## 3. Requirements

| No. | Requirement |
|---|---|
| R1 | The function is off unless the policy file has the key `templates` with `enabled: true`. With the function off, the behaviour is the behaviour of this time. |
| R2 | Warren implements `POST`, `PATCH`, and `DELETE` for templates, with the request data and the response rules of `protocol.md`, section 4.6. |
| R3 | The status call gives the limits of the template rules in the forms that the Open WebUI user interface reads. |
| R4 | A user template can set only the name, the scope, the image, the CPU limit, the memory limit, the storage size, the network profile, and the environment variables. |
| R5 | Warren examines each value against the template rules when Open WebUI writes the template, and again at each start of a sandbox. |
| R6 | A user template cannot make the security settings of a pod weaker. The settings of `isolation.md`, section 2, are fixed. |
| R7 | User templates are in Kubernetes objects. Warren has no database. |
| R8 | The template read calls answer from memory (`protocol.md`, section 9.4, item 1). |
| R9 | A file policy cannot change through the management API. |
| R10 | The delete of a user template stops its sandboxes and deletes their volumes. |

## 4. Template rules in the policy file

```yaml
templates:
  enabled: true
  maxTemplates: 200
  scopes: [user, chat, shared]
  limits:
    cpu: 2
    memory: 4Gi
    storage: 10Gi
  allowedImages:
    - "ghcr.io/open-webui/open-terminal:0.18.1-openshift"
    - "registry.example/terminals/*"
  networkProfiles:
    internet: {egress: internet}
    none: {egress: none}
    databases:
      egress: none
      dns: true
      allow:
        - namespace: databases
          podLabels: {app: postgres}
          ports: [5432]
  defaultNetworkProfile: internet
  base:
    runtimeClassName: gvisor
    idleTimeout: 30m
    storage: {retention: 30d}
```

| Key | Default | Function |
|---|---|---|
| `enabled` | `false` | Turns the function on. |
| `maxTemplates` | `100` | The maximum number of user templates. |
| `scopes` | `[user, chat, shared]` | The scopes that a user template can have. `model` is possible, and it is not in the default: Open WebUI does not examine each model ID (`protocol.md`, section 6.1). |
| `limits.cpu`, `limits.memory`, `limits.storage` | The default limits of a policy (`policies.md`, section 6), and `1Gi` for storage | The maximum that a user template can request. |
| `allowedImages` | Empty | Patterns for the images that a user template can set. Empty means that a user template cannot set an image. This is different from the orchestrator, where an empty list permits each image. |
| `networkProfiles` | `internet` and `none` | Names with network settings. A value has the keys of `network` (`policies.md`, section 14). |
| `defaultNetworkProfile` | `internet` | The profile of a user template that sets none. It must be a name of `networkProfiles`. |
| `base` | Empty | Settings that all user templates get and that a user cannot change. |
| `base.lockedEnv` | Empty | Names of variables of `base.env` that a user template cannot replace. |

Rules:

- `base` can have each key of `defaults` (`policies.md`, section 4.1) and the key `lockedEnv`, but not `image`, `network`, and the limits of `resources`. The other keys of this section give these values.
- The values of `defaults` apply first, then the values of `base`, then the values of the user template.
- If `scopes` has `chat` or `model`, and the storage type of `base` is `persistent`, `base` must set `storage.retention`. This is the rule of `policies.md`, section 7.1.
- A pattern of `allowedImages` is the complete image reference. The character `*` agrees with each sequence of characters, also `/` and `:`. No other character is special.
- The parser applies the strict rules of `policies.md`, section 3, to the new key.

An image is trusted software (`threat-model.md`, section 4.2). A pattern must permit only images that the administrator trusts.
The start command comes from `base.command`, or it is the default command. It must be correct for each image that the patterns permit (`isolation.md`, section 2.1).

## 5. Management API

All calls keep the rules of this time: the connection key is necessary, a request with an identity header gets the status 403, and each reply has a JSON body.
An error reply has the field `detail`. Open WebUI shows it (`protocol.md`, section 4.6).

### 5.1 Status call

`GET /api/v1/terminals/status`, with the function on:

| Field | Value | Form |
|---|---|---|
| `allowed_images` | The patterns of `allowedImages`, with commas | String. The Open WebUI user interface ignores an item that contains `*`. |
| `max_cpu` | `limits.cpu` | JSON number. The field has the Go type `*string` at this time: change it. |
| `max_memory`, `max_storage` | `limits.memory`, `limits.storage` | String, for example `4Gi` |
| `network_profiles` | The names of `networkProfiles`, in the order of the names | List of strings |

With the function off, the reply is the reply of this time.

### 5.2 Make a template

`POST /api/v1/terminals/templates` with the body `{"id": "<UUID>", "data": {...}}`.

| Condition | Status |
|---|---|
| The function is off | 403, with the text of this time |
| The body is not a JSON object, or it has more than 64 KiB | 400 |
| The ID is not a UUID in lower-case letters | 422 |
| A file policy or a user template has the ID | 409 |
| The number of user templates is `maxTemplates` | 429 |
| A value of `data` is not valid (section 6) | 422. The `detail` names the field and the limit. |
| Kubernetes refuses the write | 502 |
| Success | 200 with `{"id": ..., "data": {...}}` |

### 5.3 Change a template

`PATCH /api/v1/terminals/templates/{id}` with the data fields as the body.

- A field that is in the body replaces the value of the template. A field that is not in the body keeps its value. The value `null` means "the default of the server".
- Warren examines the complete result, as for a new template.
- A change of the scope gets the status 409 while the template has a pod or a volume. The documentation of the orchestrator has the same rule (`feature-parity.md`, section 6).
- A file policy gets the status 403.
- An unknown ID gets the status 404.
- Success gives the status 200 with the complete template.

A change applies as a change of the policy file does (`policies.md`, section 11): the image, the limits, the storage, and the variables apply to pods that start after the change. The rules for a network change apply to the network profile.

### 5.4 Delete a template

`DELETE /api/v1/terminals/templates/{id}`.

1. Warren writes the annotation `warren.coderpunch.io/deleting` to the ConfigMap of the template. From this point the template is not in the template list, and each request for it gets the status 404.
2. Warren stops the pods of the template and deletes its volumes.
3. Warren deletes the ConfigMap.

- The call waits a maximum of 60 seconds for step 2. It then replies with the status 200 and `{"id": ..., "deleted": true}`.
- The reaper continues the steps 2 and 3 at each run for each ConfigMap that has the annotation. Thus a stop of warren in the middle loses no step.
- A file policy gets the status 403. An unknown ID gets the status 404.

### 5.5 Read calls

- The template list has the file policies and the user templates, in the order of the IDs.
- For a user template, `data` has the values that the user set, also `env`. The user interface needs them to change the template. A file policy does not show `env`, as at this time.
- Before the cache of the user templates is complete, each call that needs a template gets the status 503 with `Retry-After`. Open WebUI reads a template for each user request, and a "not found" reply would be wrong.

## 6. Fields of a user template

| Field | Rule | Maps to |
|---|---|---|
| `name` | Necessary. 1 to 128 characters. No control characters. | `Policy.Name` |
| `scope` | `user`, `chat`, `model`, or `null` (shared). It must be in `scopes`. | `Policy.Scope` |
| `image` | `null`, or a text that agrees with one pattern of `allowedImages`. A maximum of 512 characters. | `Spec.Image` |
| `cpu_limit` | `null`, or a decimal number as a JSON string or a JSON number. More than 0, and not more than `limits.cpu`. | The CPU limit |
| `memory_limit` | `null`, or a size. Not less than `16Mi`, and not more than `limits.memory`. | The memory limit |
| `storage` | `null`, or a size. Not less than `1Mi`, and not more than `limits.storage`. | `Storage.Size` |
| `network_profile` | `null`, or a name of `networkProfiles`. | `Spec.Network` |
| `env` | `null`, or an object with text values. Refer to the rules below. | `Spec.Env` |

Rules for `env`:

- The rules of a file policy apply: the reserved names of `reservedEnv`, and the limits of `OPEN_TERMINAL_EXECUTE_TIMEOUT` (`policies.md`, section 8).
- A maximum of 64 variables. A name has a maximum of 128 characters and the form of a C identifier. All names and values together have a maximum of 32 KiB.
- `OPEN_TERMINAL_START_DIR` and `OPEN_TERMINAL_SETUP_SCRIPT` are permitted. The user interface has fields for them (`protocol.md`, section 4.6).
- The variables of `base.env` apply first. A user template can replace one of them, unless its name is in the new key `base.lockedEnv`.

Other rules:

- Warren ignores a field of `data` that it does not know, and does not keep it. A new version of Open WebUI can add a field.
- The requests of the pod come from `base`. If a request is more than the limit of the template, the limit is the request. This is the rule of `policy.complete`.
- A user template has no user list. Open WebUI selects the users with its access grants.
- The storage type is the type of `base`, or the default of the scope.

## 7. Design

### 7.1 Storage

One ConfigMap for each user template, in the sandbox namespace.

| Property | Value |
|---|---|
| Name | `warren-template-<id>` |
| Labels | `app.kubernetes.io/managed-by=warren`, `warren.coderpunch.io/kind=template` |
| Data key `template.json` | An object with `version: 1` and the examined `data` |

Why the sandbox namespace: warren already has wide permissions there, and a sandbox has no credential for the Kubernetes API.
In the namespace of warren, the same permission would let warren change its own policy file.

New permission of the service account in the sandbox namespace, only with the function on:

| Resource | Verbs |
|---|---|
| `configmaps` | `get`, `list`, `watch`, `create`, `update`, `delete` |

Add the permission to `requiredPermissions` when the function is on. Warren then stops at its start with a clear message if it is absent.

A ConfigMap is not a place for secrets. The documentation gives the warning of `feature-parity.md`, section 11: each user of a terminal can read its variables.

### 7.2 Package `internal/template`

| Part | Function |
|---|---|
| `Rules` | The template rules of section 4, after the parser examined them. |
| `Data` | The fields of section 6, with JSON tags. |
| `Rules.Policy(id string, d Data) (policy.Policy, error)` | Examines the data and makes the policy. This is the only place for the rules of section 6. |
| `Store` | A cache of the ConfigMaps with an informer, as the manager has for pods. It has `Get`, `List`, `Create`, `Update`, and `Delete`. |

Rules for the store:

- `Get` and `List` read only the cache (R8).
- After a successful write, the store puts the new object into the cache immediately. A read that follows the write must see it, also before the event of the informer comes.
- A write uses the resource version of the object. Two warren processes can operate in a rollout. A conflict gives the status 409.
- `Get` calls `Rules.Policy` with the template rules of this time (R5). If the stored data does not agree with the rules, for example after an administrator made a limit smaller, the template stays in the list. A start of its sandbox gets the status 409 with a text that names the field. Pods that operate continue, as for a change of a file policy.
- A ConfigMap with a JSON error, or with an unknown `version`, is not in the list. Warren writes one log record for it.

### 7.3 One source of policies

At this time, `policy.Store` is the source of policies for the API, the proxy, and the manager.
Add a type that puts the two sources together:

```go
// Source gives the file policies and the user templates.
type Source struct {
    File      *policy.Store
    Templates *template.Store // nil with the function off
}

func (s *Source) Get(id string) (policy.Policy, bool)
func (s *Source) List() []policy.Policy
func (s *Source) ChangedAt() time.Time
```

- `Get` looks in the file policies first. A user template can never replace a file policy.
- `ChangedAt` returns the time of the file only. A write of a user template must not start the delete delay of the volumes again (`policies.md`, section 7.1).
- The reaper stops the pods of a template that is gone with the reason `no_policy`. The network policies follow a changed profile. This code needs no change, because it reads the policies through `Get`.

### 7.4 Limits and accounts

A management call has no user identity (`protocol.md`, section 4.6). Warren does not know which user made a template.

- Open WebUI decides who can make a template. Warren cannot limit the number of templates for one user. `maxTemplates` limits the total.
- The limits for the sandboxes and the volumes of a user continue to apply at each start.

## 8. Changes

| File | Change |
|---|---|
| `internal/template/` | The new package and its tests. |
| `internal/policy/decode.go`, `policy.go` | The key `templates`. Export the checks of `env`, images, and sizes that the new package uses. |
| `internal/api/api.go` | The three write calls. The status call. `env` in the data of a user template. The 503 reply before the cache is complete. |
| `cmd/warren/main.go` | Make the store and the source. Give the source to the API, the proxy, and the manager. |
| `internal/kube/network.go` | The permission for `configmaps`, with the function on. |
| `internal/kube/reaper.go` | Continue the delete of a template that has the annotation `deleting`. |
| `deploy/helm/warren` (specification 02) | The rule for `configmaps` in the Role, when the policies have `templates.enabled`. |
| `test/e2e/` (specification 01) | Case E5 changes: the calls are successful. New cases for each row of sections 5.2 to 5.4. |

## 9. Trust boundary

This work moves a trust boundary (`CONTRIBUTING.md`, section 7). At this time, only the administrator selects the image, the variables, and the limits of a sandbox. With this work, each user that Open WebUI permits selects them, inside the template rules.

| Threat | Protection | What stays open |
|---|---|---|
| A user selects an image that gives more privileges, or that is hostile to other users of a shared terminal. | `allowedImages`. The default permits no image. The security settings of the pod are fixed. | The administrator must trust each image that a pattern permits. A pattern with `*` permits each new tag of a repository. |
| A user sets a variable that changes the programs that the server loads. | The reserved names of a file policy apply. | |
| A user puts text into a variable or a name to change a log record or a pod specification. | Limits for the length and the characters. The `$` escape of `buildPod`. Warren does not write values of variables to its log. | |
| A user requests all resources of the cluster with many templates. | `limits`, `maxTemplates`, and the limits for sandboxes and volumes of each user. | A ResourceQuota for the sandbox namespace is the limit for the total (`threat-model.md`, section 6). |
| The setup script of a shared template does damage to the other users of that terminal. | None. The script operates as the user of the sandbox. | All users of a shared sandbox trust the person who made the template. The documentation must say so. |
| A user selects a network profile with more access than the administrator wants. | A user can select only a name of `networkProfiles`. | The administrator must put only profiles there that each user can have. |
| A client with the connection key deletes all templates and their volumes. | The connection key is the protection, as for the delete of an instance. | Refer to `threat-model.md`, section 4.1. |

Update `threat-model.md` with these rows. Update the sentence "The administrator selects the image and the command" in its section 4.2.

## 10. Tests

| Test | Package | What it shows |
|---|---|---|
| A table for each field of section 6: a valid value, each limit, and each error | `internal/template` | R4, R5 |
| The image patterns: no pattern, an exact text, `*` at each position | `internal/template` | Section 4 |
| A fuzz test of `Rules.Policy`: no panic, and each policy that it returns passes `policy.Validate` | `internal/template` | Hostile input |
| The pod of a user template has the fixed security settings and the values of `base` | `internal/kube` | R6 |
| Each row of the tables of sections 5.2 to 5.4 | `internal/api` | R2 |
| The status call gives a JSON number for `max_cpu` | `internal/api` | R3 |
| A read immediately after a write sees the new template | `internal/template` | Section 7.2 |
| Two stores write the same template: one gets a conflict | `internal/template` | Section 7.2 |
| With the function off, each test of this time passes with no change | All | R1 |
| A user template with the ID of a file policy is refused, and a file policy that gets the ID of a user template has priority | `internal/api`, `cmd/warren` | R9 |
| After the administrator makes `limits.memory` smaller, a start of a template above the limit gets the status 409 | `internal/kube` | R5 |
| The delete of a template: its pods and volumes go away. A new manager completes a delete that stopped in the middle. | `internal/kube` | R10 |
| Cluster check `templates.py`: make, use, change, and delete a template with direct calls | `test/cluster` | The function on a cluster |
| End-to-end: the make, change, and delete of a terminal through Open WebUI | `test/e2e` | R2 with the real client |

## 11. Phases

| Phase | Content | Result |
|---|---|---|
| 1 | The key `templates`, the package `internal/template` with no store, and the status call | An administrator sees the limits in Open WebUI. No write is possible. |
| 2 | The store, the source, and the read calls | Warren shows a template that a person made with `kubectl`. |
| 3 | The write calls and the delete | The complete function. |
| 4 | The cluster check, the end-to-end cases, and the documents | The release. |

Each phase is one pull request, and each one keeps R1.

## 12. Documentation

- New document `docs/templates.md`: the template rules, the fields, and the warnings of section 9.
- `protocol.md`, section 9.5: change decision 6, with the date and the name of the owner.
- `feature-parity.md`: sections 9, 10, and 13. The rows about the user interface, the limits, the image allowlist, and the template API get the plan value "v1".
- `policies.md`: section 1 says that there are two types of policies. Section 12 gets the ConfigMap and the permission.
- `threat-model.md`: section 9 of this document.

## 13. Open items

| No. | Item |
|---|---|
| O1 | Decided on 2026-11-28: the delete of a user template stops its sandboxes and deletes their volumes (R10). The public documentation says the same for the orchestrator. For a file policy, warren keeps the opposite rule (`policies.md`, section 11.1), because a change of a file can be an error. |
| O2 | Fact to get from the Open WebUI source code: the type of `cpu_limit` in the request (text or number), if the `PATCH` body has all fields or only the changed fields, and the status values for which the user interface shows the `detail`. Specification 01 gives these facts. |
| O3 | Fact to get from specification 01, questions Q1 and Q2: how Open WebUI gives access to a template that it did not make. The answer can change the priority of this work. |
| O4 | Decided on 2026-11-28: warren supports the four scopes for a user template. The default of `scopes` does not have `model`. An administrator adds it to permit it (section 4). |
| O5 | Decision of the owner: a template that does not agree with the rules of this time stays in the list and cannot start (section 7.2). The alternative is to stop its pods immediately. |
| O6 | Decided on 2026-11-28: a user template can set only an image that agrees with a pattern of `allowedImages`. An empty list permits no image (section 4). |

## 14. Acceptance

1. With the function off, the behaviour and the permissions are those of this time.
2. With the function on, a user of Open WebUI makes a terminal, uses it, changes its memory limit, and deletes it. The end-to-end test shows this.
3. No test can make a pod that has a security setting different from `isolation.md`, section 2.
4. The tests of section 10 pass.
