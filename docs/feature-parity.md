# Feature parity with the Open WebUI enterprise orchestrator

Status: research record and proposal. The owner of the project must approve the "Plan" column.

## 1. Sources and rules

| Item | Value |
|---|---|
| Source | Public documentation only: `docs.openwebui.com`, from the repository `github.com/open-webui/docs` |
| Commit | `1bcfdcdbf33f6dce876c1d6cdb4ad92cb71f4188` (2026-11-25) |
| Main pages | `/features/open-terminal/terminals/` and the pages below it |
| Other pages | `/features/open-terminal/setup/connecting`, `/features/open-terminal/setup/updating`, `/features/open-terminal/advanced/multi-user`, `/features/open-terminal/advanced/security`, `/reference/env-configuration`, `/reference/server-side-tool-calling` |

Clean-room rules that apply to this document:

- This document lists features only. It contains no code and no configuration that came from the documentation.
- No person read the `open-webui/terminals` repository for this document. The public documentation refers to the README of that repository for some settings. This document does not use that README.
- Names of settings, headers, and fields are facts. This document gives them where they are necessary to identify a feature.

In this document, "the orchestrator" means the enterprise product that the public documentation describes.
"The documentation" means the public documentation.

## 2. Words that the documentation uses

| Word | Meaning |
|---|---|
| Terminal | A named workspace definition that a user makes in Open WebUI. |
| Template | The same object, as the orchestrator stores it. |
| Instance | One container or pod that the orchestrator starts from a template. |
| Policy | The old object (before orchestrator version 0.4.0). A policy belonged to a connection. The documentation says that templates replaced policies. |

The warren specification uses the word "policy" for a named set of sandbox settings.
One warren policy has the same function as one template.
The warren word for an instance is "sandbox".

## 3. Plan values

| Value | Meaning |
|---|---|
| v1 | In the first release. |
| Later | Not in the first release. Possible in a subsequent release. |
| Won't do | Not planned. The note gives the reason. |
| Not applicable | Open WebUI or Open Terminal supplies the feature. Warren has no work to do. |

## 4. Architecture

| Feature in the documentation | Plan | Note |
|---|---|---|
| The orchestrator is between Open WebUI and the instances. All traffic goes through it. | v1 | Same flow in warren. This includes port previews, the browser view, and the desktop view. Refer to `open-terminal-api.md`, section 7. |
| The orchestrator has the same tool interface as Open Terminal. Open WebUI uses it as a terminal server. | v1 | Warren is a reverse proxy for the Open Terminal API. |
| One instance has its own files, processes, packages, limits, and network profile. | v1 | One pod for each scope key. |
| A template ID and a context ID identify an instance. The context ID is `default`, `user:<id>`, `chat:<id>`, or `model:<id>`. | v1 | Warren uses the same context ID forms. Open WebUI reads them. Refer to `protocol.md`, section 4.6. |
| The orchestrator makes a private API key for each instance. | v1 | Warren keeps the key in a Secret. |
| Open WebUI finds the server type `orchestrator` when the administrator saves the connection. | v1 | Refer to `protocol.md`, section 3.1. Warren must answer the template list call. |

## 5. Backends

| Feature in the documentation | Plan | Note |
|---|---|---|
| Docker backend on one host. It needs the Docker socket. | Won't do | Warren is for Kubernetes only. The documentation says that Docker instances on a shared network can connect to the ports of other instances. |
| Kubernetes backend. The orchestrator makes pods, volumes, and services directly. | v1 | This is the warren model. |
| Kubernetes operator backend with a custom resource and a second controller. | Won't do | Warren is the controller. Warren keeps its state in labels and annotations. A custom resource is not necessary. |
| Instance phases in the status of the custom resource. | Won't do | Warren shows the state in pod annotations, metrics, and the instance list. |
| OpenShift restricted mode (pods that are not root). | Later | Warren always applies the restricted Pod Security Standard. A test on OpenShift is not in the first release. |
| A kubeconfig setting for an orchestrator that is not in the cluster. | Later | Warren v1 operates in the cluster. |
| Deployment as a subchart of the Open WebUI Helm chart, with a key that the chart makes. | Won't do | Warren has its own Helm chart. |

## 6. Instance scopes

| Feature in the documentation | Plan | Note |
|---|---|---|
| Shared scope: one instance for all users of a terminal. | v1 | |
| User scope: one instance for each user. | v1 | |
| Chat scope: one instance for each saved chat. A temporary chat cannot use it. | v1 | |
| Model scope: one instance for each model. All users of the model share it. | v1 | Only possible with an orchestrator connection. Refer to `protocol.md`, section 9.2. |
| The scope cannot change while instances exist. | v1 | Warren does not prevent the change. After a change, warren stops the pods of the previous scope and keeps their volumes. Refer to `policies.md`, section 11. |
| Per-automation scope. The documentation describes none. | Later | The warren specification puts it in a subsequent release. |

## 7. Lifecycle

| Feature in the documentation | Plan | Note |
|---|---|---|
| The orchestrator makes an instance on the first request and uses it again for subsequent requests. | v1 | |
| On Kubernetes, the proxy starts after the pod is ready. | v1 | A readiness gate on the Open Terminal health route. |
| A slow start shows in Open WebUI as a timeout message. The remedy is a larger Open WebUI timeout (default 60 seconds). | v1 | Warren holds the first request until the pod is ready or a deadline occurs. Open WebUI has shorter limits for some calls. Refer to `protocol.md`, section 9.4. |
| Idle timeout. The orchestrator stops the instance and keeps the files. The environment default is "off". The Helm chart default is 30 minutes. | v1 | Warren has an idle timeout that an administrator can set. The default is "on". |
| The activity time of an instance is the time of the last proxied request. | v1 | Warren also counts an open WebSocket as activity. The documentation does not define this case. |
| Idle timeout for each template. | v1 | Field in a policy. A change applies also to the pods that operate. |
| Maximum lifetime, for the orchestrator and for each template. The orchestrator stops the instance, also during use. | Later | |
| Scheduled reset. The orchestrator clears the files at a set time, each week, each month, or on a cron schedule. An administrator sets it in the orchestrator interface. | Later | |
| Instance operations: start, stop, restart, delete. | v1 | Warren implements the instance calls. Refer to `protocol.md`, section 9.5. |
| Restart stops an instance and starts it again with the current template settings. | v1 | |
| Instance operation: reset (clear the files and start again). | v1 | Warren deletes the pod and the volume, and makes a new, empty volume. The instance stays in the instance list. Warren does not start the instance again: the next request of a user does that. Open WebUI 0.13.4 has no control for this operation. |
| A deleted instance keeps its volume on the operator backend. A deleted terminal removes its instances and files. | v1 | Different in warren: the delete operation for an instance removes the pod and the volume. The Open WebUI user interface tells the person that the delete removes the instance and its files. The stop operation keeps the volume. A template is part of the warren configuration, thus Open WebUI cannot delete it. |
| A template change applies only to instances that start after the change. | v1 | Warren reads the policy file again with no restart. The idle time, the retention time, and the user list apply immediately. Refer to `policies.md`, section 11. |
| Setup script that runs at each start of an instance. Open Terminal reads it from an environment variable. | v1 | Warren passes the variable. A setup script that needs root or a network download can fail in a restricted sandbox. |
| A setting for the image pull policy. | v1 | |
| Docker backend: when the orchestrator stops, open sessions stop and the containers continue. The orchestrator finds the containers again when it starts. | v1 | Warren reads its state from Kubernetes at start-up. Open WebSockets stop when warren stops. |

## 8. Storage

| Feature in the documentation | Plan | Note |
|---|---|---|
| Only the home directory stays after a restart. | v1 | The PVC is the home directory. |
| An optional PVC for each instance, with a storage class and a size (default `1Gi`). The default is "on". | v1 | One PVC for each scope key. The default is "on" for the scopes `user` and `shared`, and "off" for the scopes `chat` and `model`. Refer to `policies.md`, section 7. |
| The PVC stays when the pod stops because of the idle timeout. | v1 | |
| A retention time for a volume that no instance uses. The documentation describes none. | v1 | Addition in warren. It is necessary for a policy with the scope `chat` or `model` and persistent storage. |
| Limits on the number of volumes, for each user and in total. The documentation describes none. | v1 | Addition in warren. |
| The Secret with the instance key also stays when the pod stops. | Won't do | Warren makes a new key at each pod start. A key then has a shorter life. |
| Storage modes with the names `per-user`, `shared`, and `shared-rwo`. The documentation does not define them. | Later | |
| Orchestrator state in SQLite or PostgreSQL. | Won't do | Warren keeps its state in Kubernetes objects. |
| Docker bind mounts and extra mounts for all instances. | Won't do | Docker only. |

## 9. Template fields and policy fields

| Feature in the documentation | Plan | Note |
|---|---|---|
| Name. | v1 | |
| Scope. | v1 | |
| Image, with an orchestrator default. A second default for Kubernetes backends. | v1 | One default image in warren. |
| CPU and memory. The documentation says that the orchestrator sets limits only, not requests. | v1 | Warren sets requests and limits. |
| Storage size. | v1 | |
| Network profile: a name that selects one network configuration that an administrator defined. | v1 | In warren, the egress rules are part of the policy. |
| Environment variables as raw strings. | v1 | Warren does not show the variables of a policy to Open WebUI. |
| Start directory. Open Terminal reads it from an environment variable. | v1 | Warren passes the variable. |
| Runtime class (gVisor or Kata). The documentation describes none. | v1 | Addition in warren. |
| Changes to the security context and the pod template of an instance. | Won't do | Warren has a fixed restricted security context. Only the runtime class, node selector, and tolerations can change. |
| Node selector and tolerations. In the documentation these are orchestrator settings, not template fields. | v1 | Keys of a policy, and of the `defaults` of the policy file. |
| Users make and change templates in the Open WebUI user interface. | Later | In v1, an administrator defines policies in a YAML file or a ConfigMap. Warren shows each policy to Open WebUI as a read-only template. |

The Open WebUI user interface accepts a decimal number for CPU, for example `0.5`. It accepts a size such as `512Mi` or `2Gi` for memory and storage (`protocol.md`, section 4.6).
Warren must show values in these forms.

## 10. Limits

| Feature in the documentation | Plan | Note |
|---|---|---|
| Maximum CPU, memory, and storage that a template can request. | Later | Not necessary while only an administrator defines policies. |
| Image allowlist with glob patterns. | Later | Not necessary while only an administrator defines policies. |
| Restricted mode that applies restricted security settings. | v1 | Always on in warren. There is no setting to turn it off. |
| In restricted mode, the orchestrator rejects a template that sets the in-container egress filter or package installation at start-up. | v1 | These Open Terminal functions need root or `NET_ADMIN`. Warren rejects a policy that sets them. |
| Limit on the number of instances for each user. The documentation describes none. | v1 | Addition in warren. |

## 11. Environment variables

| Feature in the documentation | Plan | Note |
|---|---|---|
| The orchestrator passes the template variables to each instance. | v1 | |
| The orchestrator removes the Open Terminal API key variable from a template. | v1 | |
| The orchestrator adds variables that give the CPU and memory limits to the processes in the instance. | v1 | Open Terminal 0.18.1 does not read these variables. A system prompt or a tool can read them. |
| A system prompt variable in the orchestrator environment becomes the default for all instances. | Later | An administrator can set the variable in each policy. |
| The documentation warns that all users of a terminal can see its environment variables. | Not applicable | The warren documentation must give the same warning. A policy is not a place for secrets. |

## 12. Authentication and access

| Feature in the documentation | Plan | Note |
|---|---|---|
| Open WebUI sends one static API key to the orchestrator as a bearer token. | v1 | The warren specification calls this the static API key mode. |
| The orchestrator trusts all requests that have the key. Open WebUI gives the identity in headers. | v1 | Warren logs a warning in this mode. Refer to `protocol.md`, section 9.3. |
| Terminals from an orchestrator connection always use the static key. The documentation describes no token verification in the current version. | v1 | Warren uses the same model. The current protocol sends no user token to an orchestrator. Thus warren does not verify user tokens. Refer to `protocol.md`, section 9.5. |
| If no key is set, the orchestrator makes one at each start and prints it. | Won't do | Warren does not start without a key from a Secret. |
| The same key opens the administrator interface and the template API. | v1 | The paths that Open WebUI calls must accept the connection key. A second key is possible only for direct administrator calls. |
| Connection access grants in Open WebUI: read and write, for users and groups. | Not applicable | Open WebUI applies them before it calls warren. |
| A user can share one terminal with users and groups in Open WebUI. | Not applicable | Open WebUI keeps the grants. This is the primary method to give a policy to users or groups. |
| A person with write access to the connection can open the files and the system data of each instance on it. | Not applicable | Open WebUI applies it. Warren gets only the ID of the instance owner, and not the ID of the manager. Refer to `protocol.md`, section 6.1. |
| Selection of a policy by user or group in the orchestrator. The documentation describes none. | v1 | Open WebUI selects by user and group with its access grants. Addition in warren: an optional list of permitted user IDs in a policy. |

## 13. Administrator interface and API

| Feature in the documentation | Plan | Note |
|---|---|---|
| Web interface at `/` with template and instance views, and a setting to turn it off. | Later | The warren specification puts the administrator interface in a subsequent release. |
| Template API. The documentation does not list its paths. | v1 | Read calls, status call, and instance calls. Templates are read-only. Warren uses only the client behaviour in `protocol.md`, section 4.6. |
| Instance list with state, and system information for each instance. | v1 | Open Terminal supplies the system information through the proxy. |

## 14. Network and isolation

| Feature in the documentation | Plan | Note |
|---|---|---|
| Optional NetworkPolicy objects for the orchestrator and the instance pods. The default is "off". | v1 | Always on in warren. Each sandbox gets a default-deny policy. Ingress is possible only from warren. |
| Egress control with the in-container domain filter of Open Terminal. It needs `NET_ADMIN`. | Won't do | It conflicts with the restricted Pod Security Standard. |
| Egress control with Kubernetes NetworkPolicy. | v1 | Rules by CIDR and port in each policy. The default rules block cluster addresses. Refer to `open-terminal-api.md`, section 7.2. |
| Egress control by domain name in a network policy. The documentation describes none. | Later | Standard NetworkPolicy has no domain rules. Some CNI plug-ins have them. |
| Namespace setting for instances. | v1 | One dedicated namespace for sandboxes. |
| Labels on instance pods. | v1 | Warren also uses labels and annotations for its state. |
| One Service for each instance, with a setting for the Service type. | Won't do | Warren connects to the pod IP address. Fewer objects and fewer permissions are necessary. |
| The setting for the root of the file browser is visual only and is not a security control. | Not applicable | Open Terminal behaviour. The threat model must include this fact. |

## 15. Observability

| Feature in the documentation | Plan | Note |
|---|---|---|
| Logs with a level setting. | v1 | Structured JSON logs. |
| Metrics. The documentation describes none. | v1 | Addition in warren: Prometheus metrics. |
| Audit log. The documentation describes none. | v1 | Addition in warren: a record of each new sandbox and each access. On an orchestrator connection, the record contains the instance owner. Warren cannot know if a manager is the real caller. |
| Tracing. The documentation describes none. | Later | |

## 16. Scale

| Feature in the documentation | Plan | Note |
|---|---|---|
| More than one worker process or replica. | Later | Warren v1 operates with one active replica. State in Kubernetes makes a restart safe. |
| More than one orchestrator connection in Open WebUI. | Not applicable | Open WebUI function. More than one warren deployment is possible. |
| Multi-cluster operation. The documentation describes none. | Later | The warren specification puts it in a subsequent release. |
| Quota interface. The documentation describes none. | Later | The warren specification puts it in a subsequent release. |

## 17. Version compatibility

The documentation says that the current orchestrator needs Open WebUI 0.13.4 or later.
Warren v1 has the same minimum: Open WebUI 0.13.4.

## 18. Items that the documentation does not define

Warren must make its own decision for each of these items. No public source gives the behaviour of the orchestrator.

1. If an open WebSocket with no messages counts as activity for the idle timeout. Warren proposal: it counts.
2. The maximum wait and the error reply for a request that arrives while a pod starts.
3. The health route of the orchestrator.
4. The format of the instance ID. The context ID has a defined form (section 4).
5. The limits on the number of instances.
6. The behaviour with more than one replica.
7. The storage modes (section 8).
