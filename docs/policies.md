# Policies and storage

This document describes the policy file of warren and the storage of a sandbox.

## 1. What a policy is

A policy is a named set of sandbox settings.
An administrator writes the policies in one YAML file.
Warren shows each policy to Open WebUI as one read-only template.
Open WebUI sends the ID of the policy in the header `X-Terminal-Template-Id`.

Give the file to warren with the flag `--policy-file`.
On Kubernetes, put the file in a ConfigMap and mount the ConfigMap in the warren pod.

A policy is not a place for secrets.
Each person who can read the ConfigMap can read the file.
Each process in a sandbox can read the environment variables of the sandbox.

## 2. Example

```yaml
defaults:
  resources:
    limits: {cpu: 1, memory: 1Gi}
  env:
    TZ: "Europe/Berlin"

policies:
  - id: personal
    name: Personal sandbox
    scope: user
    storage:
      size: 5Gi
      retention: 90d

  - id: by-chat
    name: One sandbox for each chat
    scope: chat
    idleTimeout: 10m

  - id: data-team
    name: Data team sandbox
    scope: user
    users: ["8f14e45f-ceea-467f-a0e6-1f3a4b5c6d7e"]
    image: registry.example/data-terminal:1.4
    runtimeClassName: gvisor
    resources:
      requests: {cpu: 500m, memory: 1Gi}
      limits: {cpu: 4, memory: 8Gi}
    storage:
      size: 50Gi
      storageClassName: fast
    env:
      OPEN_TERMINAL_EXECUTE_TIMEOUT: "120"
```

## 3. Rules for the file

Warren reads the file with strict rules:

- The file has one YAML document.
- An unknown key is an error.
- A key that occurs two times is an error.
- Anchors, aliases, and merge keys are not permitted.
- A text value must be a YAML string. Put a value such as `007`, `true`, or `null` in quotation marks.
- An error in a key or in a policy gives the line number.

If the file is not valid, warren does not start.

## 4. Keys of a policy

| Key | Necessary | Default | Function |
|---|---|---|---|
| `id` | Yes | | The template ID. It is a DNS label: lower-case letters, digits, and `-`, with a maximum of 63 characters. |
| `name` | Yes | | The name that the user sees. |
| `scope` | Yes | | `user`, `chat`, `model`, or `shared`. Refer to section 5. |
| `users` | No | All users | A list of user IDs of Open WebUI. Refer to section 9. |
| `image` | No | The flag `--sandbox-image` | The container image of the sandbox. |
| `imagePullPolicy` | No | The default of Kubernetes | `Always`, `IfNotPresent`, or `Never`. |
| `imagePullSecrets` | No | None | Names of Secrets in the sandbox namespace. |
| `runtimeClassName` | No | None | The name of a RuntimeClass, for example for gVisor or Kata Containers. |
| `resources` | No | Refer to section 6 | Requests and limits for `cpu`, `memory`, and `ephemeral-storage`. |
| `storage` | No | Refer to section 7 | The home directory of the sandbox. |
| `env` | No | None | Environment variables. Refer to section 8. |
| `idleTimeout` | No | The flag `--idle-timeout` | Warren stops a sandbox after this time with no use. `0` means never. |
| `nodeSelector` | No | None | Node labels that the pod of the sandbox requires. |
| `tolerations` | No | None | A list of tolerations with the keys `key`, `operator`, `value`, `effect`, and `tolerationSeconds`. |
| `network` | No | Refer to section 14 | The connections that the sandbox can open. |
| `command` | No | Refer to section 15 | The start command of the sandbox, for an image with a different layout. |
| `runAsUser` | No | `1001` | The user ID of the processes in the sandbox. Refer to section 15. |
| `userNamespace` | No | `false` | `true` gives the sandbox its own user namespace. Refer to section 15. |

A time is a number with a unit: `s`, `m`, `h`, or `d`. Examples are `30m`, `12h`, and `90d`. The value `0` needs no unit.
The unit `d` is one day of 24 hours. Do not use the unit `d` together with a different unit. Write `36h`, not `1d12h`.

### 4.1 Defaults

The optional key `defaults` at the top of the file can have each key of the table, but not `id`, `name`, `scope`, `users`, and `storage.type`.
A policy uses a value of `defaults` when it does not set that value.

- `env` and the resources merge by name. A policy adds to the variables of `defaults` or replaces them.
- `nodeSelector`, `tolerations`, `imagePullSecrets`, and `command` do not merge. The value of the policy replaces the value of `defaults`.
- `network`: refer to section 14.3.

## 5. Scopes

| Scope | One sandbox for each | Default storage |
|---|---|---|
| `user` | User | Persistent |
| `chat` | Saved chat | Ephemeral |
| `model` | Model ID | Ephemeral |
| `shared` | Policy | Persistent |

All users of a `shared` policy use one sandbox and can read all its files.
All users who can send a model ID use the sandbox of that model.

Do not change the scope of a policy that has sandboxes.
The name of a sandbox includes its scope. After a change, no request gets to the old pods and volumes.
Warren stops the old pods and keeps the old volumes. Refer to section 11.

## 6. Resources

```yaml
resources:
  requests: {cpu: 100m, memory: 256Mi, ephemeral-storage: 100Mi}
  limits: {cpu: 2, memory: 2Gi, ephemeral-storage: 2Gi}
```

The values in this example are the defaults of warren.
A sandbox always has a limit for each of the three resources. A sandbox with no limit could use all memory or all disk space of its node.

- A CPU value is a number of processors, for example `500m`, `0.5`, or `2`.
- A size is a whole number of bytes with a unit, for example `512Mi` or `2Gi`. The minimum for memory is `16Mi`. The maximum for each size is `1Ei`.
- A request must not be more than its limit. If a policy sets a limit that is less than the default request, warren uses the limit as the request.
- The limit for `ephemeral-storage` is for the temporary directory, the changes to the container file system, and the logs.

If a sandbox uses more memory than its limit, Kubernetes stops the process.
If a sandbox uses more ephemeral storage than its limit, Kubernetes stops the pod. Warren then starts a new pod at the next request.

## 7. Storage

```yaml
storage:
  type: persistent
  size: 5Gi
  storageClassName: fast
  retention: 90d
```

| Key | Default | Function |
|---|---|---|
| `type` | Refer to section 5 | `persistent` or `ephemeral`. |
| `size` | `1Gi` | The size of the home directory. The minimum is `1Mi`. |
| `storageClassName` | The default class of the cluster | The StorageClass of the volume. |
| `retention` | `0` | The time that warren keeps a volume that no sandbox uses. `0` means that warren keeps the volume. The minimum is `1m`. |

The home directory of a sandbox is `/home/user`.

### 7.1 Persistent storage

Warren makes one PersistentVolumeClaim for each sandbox.
The volume has the name of the sandbox. Warren never gives it to a different sandbox.
The volume stays when the pod stops. The next pod of the sandbox uses it again.

A policy with the scope `chat` or `model` and persistent storage must set `retention`.
A user can make a new chat at each time, and Open WebUI does not examine each model ID. Without an end, the number of volumes increases with no limit.

Warren deletes a volume in these conditions only:

- A person deletes the instance in Open WebUI, or a client sends the reset operation. Refer to section 10.
- No sandbox used the volume for the retention time of its policy.
- A start made the volume, and the same start did not make the pod. That volume is empty.
- The volume never got storage, and its storage class or size is different from the policy. That volume is empty. Warren deletes it at the next start, and makes a new volume with the settings of the policy.

The retention time starts when warren finds that the volume has no pod. Warren writes that time to the annotation `warren.coderpunch.io/unused-since` of the volume. A start of the sandbox removes the annotation.

Warren uses the retention time of the policy file at that moment.
If you make the retention time shorter, the new time applies immediately to the volumes that are already not in use.
After its start, and after each change of the policy file, warren deletes no volume for the time of the flag `--volume-delete-delay` (default 10 minutes). In that time, a log record gives the number of volumes that are past their retention time. If the number is not correct, change the file back.

### 7.2 Requirements for the StorageClass

- The class must make volumes on demand.
- The class must apply the `fsGroup` of a pod. The sandbox operates as a user that is not root, and it writes to the volume as a member of the group 1001.
- The class must enforce the size of a volume. If it does not, one sandbox can fill the storage of all sandboxes.
- The reclaim policy must be `Delete`. With `Retain`, the files stay after warren deletes a volume.
- Use the volume binding mode `WaitForFirstConsumer`. With `Immediate`, a volume can be in a zone where the pod cannot operate.

The default class of a kind cluster (`local-path`) does not enforce the size. Use it only for tests.

A volume has the access mode `ReadWriteOnce`. Warren does not change the size of a volume that exists. A new size in the policy applies to new volumes.

### 7.3 Ephemeral storage

The home directory is in the pod. Kubernetes deletes it when the pod stops, also after the idle time.
The size of the home directory adds to the `ephemeral-storage` limit of the pod.
If the sandbox writes more than the size, Kubernetes stops the pod and deletes the files.

### 7.4 Limits for volumes

| Flag | Default | Function |
|---|---|---|
| `--max-volumes-per-user` | 20 | The maximum number of volumes of the scopes `chat` and `model` that the starts of one user can make. `0` means no limit. |
| `--max-volumes` | 0 | The maximum number of all volumes. `0` means no limit. |

When a user is at the limit, a start that needs a new volume gets status 429.
A sandbox that has its volume can start. A sandbox of the scope `user` or `shared` does not count for the limit of the user.
To give a volume back, delete the instance in Open WebUI, or set a retention time.

Set `--max-volumes`, or a ResourceQuota for the sandbox namespace, to limit the total storage.

### 7.5 A pod on a node that does not answer

If a pod with a volume stays in deletion, warren does not remove it. The node can still write to the volume, and a second pod on the same volume could break the files.
The sandbox is not available in that time. Warren writes a log record with the name of the node.
The sandbox becomes available in one of these conditions:

- The node operates again.
- An administrator deletes the Node object.
- An administrator sets the taint `node.kubernetes.io/out-of-service` on the node.

## 8. Environment variables

```yaml
env:
  TZ: "Europe/Berlin"
  EDITOR: "nano"
```

Each value must be a YAML string.
A sandbox gets the value as the file has it. Kubernetes does not replace `$(NAME)` in a value.

A policy cannot set these variables:

| Variable | Cause |
|---|---|
| `OPEN_TERMINAL_API_KEY`, `OPEN_TERMINAL_API_KEY_FILE` | Warren makes a new key for each pod. |
| `OPEN_TERMINAL_ISOLATED`, `OPEN_TERMINAL_MULTI_USER`, `OPEN_TERMINAL_USER_PREFIX` | Warren sets the isolation mode. One sandbox for each scope key gives the isolation. |
| `OPEN_TERMINAL_PACKAGES`, `OPEN_TERMINAL_PIP_PACKAGES`, `OPEN_TERMINAL_NPM_PACKAGES`, `OPEN_TERMINAL_ALLOWED_DOMAINS` | They need root privilege or a network download at each start. Put packages into the image. |
| `OPEN_TERMINAL_UVICORN_TIMEOUT_KEEP_ALIVE` | Warren depends on the default value. |
| `HOME`, `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`, `XDG_DATA_HOME` | Warren mounts the home directory at a fixed path. |
| `PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, `LD_AUDIT`, `PYTHONPATH`, `PYTHONHOME`, `PYTHONSTARTUP`, `PYTHONUSERBASE`, `BASH_ENV`, `ENV` | They select the programs and libraries that the server loads. Change the image. |

`OPEN_TERMINAL_EXECUTE_TIMEOUT` must be a number of seconds from 1 to 300. Open WebUI stops a request after 300 seconds.

Open Terminal does not give the variables that start with `OPEN_TERMINAL_` to the shells of the user.

## 9. User lists

```yaml
users: ["8f14e45f-ceea-467f-a0e6-1f3a4b5c6d7e", "0cc175b9-c0f1-46a8-b1e2-4a8d9e3f7b21"]
```

Open WebUI selects the users and groups of a terminal with its access grants. That is the primary control.
The key `users` is a second control in warren. It is the list of the users who can have a sandbox of the policy.

- A value is the ID of a user in Open WebUI, not the name or the e-mail address.
- Warren compares the ID with the header `X-User-Id`, which Open WebUI sets on its server (`protocol.md`, section 6.1). Warren refuses a different user with status 403 before it starts a sandbox.
- For the scopes `model` and `shared`, all users of the list share one sandbox.
- Open WebUI shows the terminal to each user that has an access grant. A user who is not in the list sees the terminal and gets an error.
- A person who manages the connection in Open WebUI can open the sandbox of a user of the list. Open WebUI then sends the ID of that user.
- Warren has no data about groups. Use the access grants of Open WebUI for groups.

When you remove a user from the list, warren refuses the next request of that user. The volume stays.
A terminal that the user has open continues to operate, and the pod does not become idle. Send the stop operation for the instance to close the terminal. Refer to section 10.
When the pod has no open connection, it stops after its idle time.
Delete the instance in Open WebUI to delete the files.

## 10. Instance operations

A person who manages a terminal in Open WebUI can see its instances and send operations.

| Operation | Pod | Volume |
|---|---|---|
| Stop | Deleted | Kept |
| Restart | Deleted, then started again | Kept |
| Start | Started | Kept |
| Delete | Deleted | Deleted |
| Reset | Deleted | Deleted. Warren then makes a new, empty volume. |

After a delete or a reset, the next request of a user starts the sandbox again with an empty home directory.
The two operations do not start the sandbox.

- After a reset, the sandbox stays in the instance list as a stopped instance. Its empty volume counts for the limits. Warren has no database, thus the volume is the record of an instance that has no pod.
- The retention time of the policy applies to the empty volume, as to each volume that no sandbox uses (section 7.1). When warren deletes the volume, the sandbox is not in the instance list.
- After a delete, the sandbox is not in the instance list, and its volume does not count for the limits.
- A sandbox that has no volume is not in the instance list after a stop, a reset, or a delete. A sandbox with ephemeral storage usually has no volume.
- If the storage type of a policy changed from persistent to ephemeral, a sandbox can have a volume from the time before the change (section 11). That sandbox stays in the instance list as a stopped instance until a reset or a delete removes the volume. A reset makes no new volume for it.
- A reset first deletes the old volume, then makes the new volume. If the warren process stops between the two steps, the result is that of a delete: the sandbox is not in the instance list. Warren has no second record to repair this.
- The result is also that of a delete, and the operation gets an error, in two conditions. Kubernetes refuses the new volume for 30 seconds. Or the client goes away, and Kubernetes does not remove the old volume in 30 seconds.

Open WebUI 0.13.4 has no control for the reset operation. A client of the management API can send it.

The instance list shows a sandbox that has a volume and no pod as stopped.
A sandbox stays on the account of the user that started it first, also when a different person starts it again.
If a manager starts a new sandbox of the scope `chat`, `model`, or `shared`, the sandbox is on the account of no user. It does not count for the limits of a user.
A start or restart operation waits while warren deletes the volume of the sandbox.

## 11. Changes of the policy file

Warren reads the file again each 30 seconds. A restart is not necessary, and open terminals stay open.
Kubernetes can take one to two minutes to change the file of a mounted ConfigMap.
If the new file is not valid, warren writes an error to its log and continues with the previous policies.

| Change | Result |
|---|---|
| Image, resources, storage, environment, runtime class, node selector, tolerations, command, user ID, user namespace | Applies to pods that start after the change. A pod that operates keeps its settings. Send a restart operation to apply the change. |
| Network: a new permission | Applies a maximum of 30 seconds after warren reads the file, also to the pods that operate. |
| Network: a removed permission | Warren stops the sandboxes of the policy. The next request starts them again with the new settings. A network plugin can let an open connection continue after a change, thus only a stop closes it. |
| Idle time | Applies immediately, also to the pods that operate. |
| Retention time | Applies immediately, also to the volumes that exist. Refer to section 7.1. |
| User list | Applies to the next request. A terminal that is open stays open. Refer to section 9. |
| Storage type from persistent to ephemeral | New pods do not use the volume. The volume stays until its retention time, or until a person deletes the instance. |
| You remove a policy | Each chat that uses the terminal gets an error. Warren stops the pods of the policy a maximum of 30 seconds after it reads the file, also a pod with an open terminal. Warren keeps the volumes. |
| The scope of a policy changes | Warren does the same as for a removed policy, for the pods and volumes of the previous scope. The retention time of the policy does not apply to the volumes of the previous scope. |

### 11.1 Volumes of a removed policy

Rule: the removal of a policy from the file never deletes data. Only an explicit cleanup operation deletes the volumes of that policy.

Warren keeps the volumes that have no policy, and writes their number to its log after each change of the file.
The retention time does not apply to these volumes. Open WebUI does not show them, because the template does not exist.
If you put the policy back, its sandboxes find their volumes again.

The cleanup operation is a command of the administrator. It deletes all volumes of one policy:

```bash
kubectl -n <sandbox namespace> delete pvc -l warren.coderpunch.io/template=<policy id>
```

To see the volumes first, use `get` in place of `delete` in the command.
Warren stops the pods of a removed policy, thus no pod uses these volumes.

## 12. Kubernetes objects

Warren makes these objects in the sandbox namespace. Each object has the label `app.kubernetes.io/managed-by=warren`.

| Object | Name | Life |
|---|---|---|
| Pod | `wr-` and 40 hexadecimal characters | From the first request to the stop. |
| Secret | The pod name, `-`, and 8 hexadecimal characters | The same as its pod. It has the API key of that pod. |
| PersistentVolumeClaim | The pod name | Refer to section 7.1. |

The labels `warren.coderpunch.io/template`, `/scope`, and `/owner` give the policy, the scope, and a digest of the user ID.
The annotations `warren.coderpunch.io/context-id` and `/created-by` give the context and the user that started the sandbox first.

The service account of warren needs these permissions in the sandbox namespace:

| Resource | Verbs |
|---|---|
| `pods` | `get`, `list`, `watch`, `create`, `delete`, `patch` |
| `secrets` | `get`, `list`, `create`, `delete`, `patch` |
| `persistentvolumeclaims` | `get`, `list`, `watch`, `create`, `delete`, `patch` |
| `networkpolicies` (`networking.k8s.io`) | `get`, `list`, `create`, `update`, `patch`, `delete` |

At its start, warren asks Kubernetes for each of these permissions. If one is absent, warren stops with a message that names it.
The network policies and the test pod of warren are in [isolation.md](isolation.md), section 9.

## 13. Metrics and audit records

| Metric | Meaning |
|---|---|
| `warren_volumes` | The number of volumes at this time. |
| `warren_volume_deletes_total{reason}` | Volumes that warren deleted. The reason is `api`, `retention`, `start_failed`, or `unbound`. |
| `warren_sandbox_creates_total{result="volume_limit"}` | Starts that warren refused because of a volume limit. |
| `warren_sandbox_reaps_total{reason}` | Pods that warren stopped. The reasons are `idle`, `failed`, `not_ready`, `api`, `bad_key`, `stuck`, `volume_gone`, `no_policy`, and the reasons of [isolation.md](isolation.md), section 7. |

Warren writes an audit record (`"log":"audit"`) when it makes a volume and when it deletes a volume. The record has the sandbox name, the policy, and the context.
The record of a new volume also has the user and the size. The record of a deleted volume also has the reason.

## 14. Network

```yaml
network:
  egress: none
  dns: true
  allow:
    - cidr: 203.0.113.0/24
      ports: [443]
    - namespace: databases
      podLabels: {app: postgres}
      ports: [5432]
```

| Key | Default | Function |
|---|---|---|
| `egress` | `internet` | `internet` permits public addresses. `none` permits no connection. |
| `dns` | `true` for `internet`, `false` for `none` | Permits name lookups with the DNS service of the cluster. |
| `allow` | None | More destinations. Refer to section 14.1. |

No policy can permit a connection to a sandbox. Only warren can connect to a sandbox.
The `internet` mode does not permit private networks and the addresses of the cluster. Refer to [isolation.md](isolation.md), section 3.3.

### 14.1 Destinations

An item of `allow` has one of two forms:

| Form | Keys | Use |
|---|---|---|
| Network | `cidr` | A destination that is not in the cluster, for example a database server in a private network. |
| Pods | `namespace` and `podLabels` | Pods of the cluster. |

The optional key `ports` limits an item to these TCP and UDP ports. Without it, the item permits all ports. A maximum of 16 ports is possible.
For pods of the cluster, give the port of the pod, not the port of the service.

- A `cidr` is a network with a prefix length, for example `10.20.0.0/16` or `2001:db8::/32`. Warren refuses the network of all addresses.
- `podLabels` is necessary with `namespace`. A list with no labels would select all pods of the namespace.
- A file can have a maximum of 32 items in one `allow` list.

An item of `allow` can permit a private address. Use it with care: a sandbox runs code that you do not control.

### 14.2 What the modes mean

- `egress: internet`: a user or a model can get data from the internet and send data to the internet. All connections come from the addresses of your cluster.
- `egress: none`: the sandbox can open no connection. Package installation and downloads do not operate.
- `egress: none` with `dns: true`: the sandbox can ask the DNS service only. Data can go out in DNS questions.

Open WebUI shows the network settings of a template as `internet`, `none`, or `custom`. `none` means no connection and no DNS.

### 14.3 The network settings of the defaults

- A policy that sets `egress` replaces all network settings of `defaults`. It gets no `dns` value and no `allow` list from them.
- A policy that sets only `dns` or `allow` keeps the `egress` mode of `defaults`.

Thus `egress: none` in a policy always means none. And if `defaults` has `egress: none`, a policy that adds one destination does not get internet access.

## 15. Command, user ID, and user namespace

```yaml
image: registry.example/terminal-alpine:1
command: ["/sbin/tini", "--", "/usr/local/bin/python3", "-I", "-u", "-m", "open_terminal"]
runAsUser: 1000
userNamespace: true
```

| Key | Function |
|---|---|
| `command` | The start command. The first item must be an absolute path. Warren adds the arguments `run --host 0.0.0.0 --port 8000`. The program gets each item as the file has it: Kubernetes does not replace `$(NAME)` in an item. |
| `runAsUser` | The user ID of the processes. It must not be 0. The group ID is always 1001. |
| `userNamespace` | `true` gives the pod its own range of user IDs on the node. The cluster must support user namespaces. With `true`, use a `runAsUser` value from 1 to 65535. Refer to [isolation.md](isolation.md), section 2.2. |

Refer to [isolation.md](isolation.md), section 2, for the default command and for the security settings that a policy cannot change.

Do not change `runAsUser` for a policy that has volumes. The files of a volume keep the user ID of the sandbox that made them, and a different user ID cannot change them.
To change the user ID, delete the instances first.

If a container cannot start with the image and the command of its policy, warren writes the message of Kubernetes to its log.
After 2 minutes, warren stops the pod.
