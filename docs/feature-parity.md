# Feature parity with the Open WebUI enterprise orchestrator

Status: research record and proposal. The owner of the project must approve the "Plan" column.

## 1. Sources and rules

| Item | Value |
|---|---|
| Source | Public documentation only: `docs.openwebui.com`, from the repository `github.com/open-webui/docs` |
| Commit | `1bcfdcdbf33f6dce876c1d6cdb4ad92cb71f4188` (2026-11-25) |
| Main pages | `/features/open-terminal/terminals/` and the pages below it, `/features/open-terminal/setup/connecting`, `/features/open-terminal/advanced/multi-user`, `/features/open-terminal/advanced/security`, `/reference/env-configuration` |

Clean-room rules that apply to this document:

- This document lists features only. It contains no code and no configuration that came from the documentation.
- No person read the `open-webui/terminals` repository for this document. The public documentation refers to the README of that repository for some settings. This document does not use that README.
- Names of settings, headers, and fields are facts. This document gives them where they are necessary to identify a feature.

In this document, "the orchestrator" means the enterprise product that the public documentation describes.

## 2. Words that the documentation uses

| Word | Meaning |
|---|---|
| Terminal | A named workspace definition that a user makes in Open WebUI. |
| Template | The same object, as the orchestrator stores it. |
| Instance | One container or pod that the orchestrator starts from a template. |
| Policy | The old object (before orchestrator version 0.4.0). A policy was attached to a connection. The documentation says that templates replaced policies. |

The warren specification uses the word "policy" for a named set of sandbox settings.
One warren policy has the same function as one template.

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
| The orchestrator is between Open WebUI and the instances. All traffic goes through it. | v1 | Same flow in warren. |
| The orchestrator has the same tool interface as Open Terminal. Open WebUI uses it as a terminal server. | v1 | Warren is a reverse proxy for the Open Terminal API. |
| One instance has its own files, processes, packages, limits, and network profile. | v1 | One pod for each scope key. |
| The orchestrator makes a private API key for each instance. | v1 | Warren keeps the key in a Secret. |
| Open WebUI detects the orchestrator type when the administrator saves the connection. | v1 | Refer to `protocol.md`, section 3.1. |

## 5. Backends

| Feature in the documentation | Plan | Note |
|---|---|---|
| Docker backend on one host. It needs the Docker socket. | Won't do | Warren is for Kubernetes only. The documentation says that Docker instances on a shared network can connect to the ports of other instances. |
| Kubernetes backend. The orchestrator makes pods, volumes, and services directly. | v1 | This is the warren model. |
| Kubernetes operator backend with a custom resource and a second controller. | Won't do | Warren is the controller. Warren keeps its state in labels and annotations. A custom resource is not necessary. |
| OpenShift restricted mode (pods that are not root). | Later | Warren always applies the restricted Pod Security Standard. A test on OpenShift is not in the first release. |

## 6. Instance scopes

| Feature in the documentation | Plan | Note |
|---|---|---|
| Shared scope: one instance for all users of a terminal. | v1 | |
| User scope: one instance for each user. | v1 | |
| Chat scope: one instance for each saved chat. A temporary chat cannot use it. | v1 | |
| Model scope: one instance for each model. All users of the model share it. | v1 | Only possible with an orchestrator-type connection. Refer to `protocol.md`, section 9.2. |
| The scope cannot change while instances exist. | v1 | A policy change applies only to new sandboxes. |
| Per-automation scope. | Later | The documentation does not describe this scope. The warren specification puts it in a later release. |

## 7. Lifecycle

| Feature in the documentation | Plan | Note |
|---|---|---|
| The orchestrator makes an instance on the first request and uses it again for subsequent requests. | v1 | |
| On Kubernetes, the proxy starts after the pod is ready. | v1 | Readiness gate on the Open Terminal health endpoint. |
| A slow start shows in Open WebUI as a timeout message. The remedy is a larger Open WebUI timeout (default 60 seconds). | v1 | Warren holds the first request until the pod is ready or a deadline occurs. Warren then replies with a clear error that tells the client to try again. |
| Idle timeout. The orchestrator stops the instance and keeps the files. The server default is "off". | v1 | Warren has a configurable time-to-live. The default is "on". |
| Idle timeout for each template. | v1 | Field in a policy. |
| Maximum lifetime. The orchestrator stops an instance after a fixed time. | Later | |
| Scheduled reset. The orchestrator clears the files at a set time or on a cron schedule. | Later | |
| Instance operations: start, stop, restart, delete. | v1 | Only if warren implements the management API. Refer to `protocol.md`, section 9.4, question 5. |
| Instance operation: reset (clear the files and start again). | Later | |
| A template change applies only to instances that start after the change. | v1 | |
| Setup script that runs at each start of an instance. Open Terminal reads it from an environment variable. | v1 | Warren passes the variable. A setup script that needs root or a network download can fail in a restricted sandbox. |
| Image pull policy setting. | v1 | |
| When the orchestrator stops, the instances continue to operate. The orchestrator finds them again when it starts. | v1 | Warren reads its state from Kubernetes at start-up. |

## 8. Storage

| Feature in the documentation | Plan | Note |
|---|---|---|
| Only the home folder stays after a restart. | v1 | The PVC is the home folder. |
| A PVC for each instance, with a storage class and a size (default `1Gi`). | v1 | One PVC for each user. Optionally one PVC for each chat. |
| The PVC stays when the pod stops because of the idle timeout. | v1 | |
| Storage modes with the names `per-user`, `shared`, and `shared-rwo`. The documentation does not define them. | Later | Warren v1 has one PVC for each scope key. |
| Orchestrator state in SQLite or PostgreSQL. | Won't do | Warren keeps its state in Kubernetes objects. |
| Docker bind mounts and extra mounts for all instances. | Won't do | Docker only. |

## 9. Template fields and policy fields

| Feature in the documentation | Plan | Note |
|---|---|---|
| Name. | v1 | |
| Scope. | v1 | |
| Image, with a server default. | v1 | |
| CPU and memory. The documentation says that the orchestrator sets limits only, not requests. | v1 | Warren sets requests and limits. |
| Storage size. | v1 | |
| Network profile: a name that selects one administrator-defined network configuration. | v1 | In warren, the egress rules are part of the policy. |
| Environment variables as raw strings. | v1 | |
| Start directory. Open Terminal reads it from an environment variable. | v1 | Warren passes the variable. |
| Runtime class (gVisor or Kata). | v1 | Not in the documentation. Addition in warren. |
| Node selector and tolerations for instance pods. | v1 | |
| Users make and change templates in the Open WebUI user interface. | Later | In v1, an administrator defines policies in a YAML file or a ConfigMap. Warren shows each policy to Open WebUI as a read-only template. |

## 10. Limits and guardrails

| Feature in the documentation | Plan | Note |
|---|---|---|
| Maximum CPU, memory, and storage that a template can request. | Later | Not necessary while only an administrator defines policies. |
| Image allowlist with glob patterns. | Later | Same reason. |
| Restricted mode that applies restricted security settings. | v1 | Always on in warren. There is no setting to turn it off. |
| In restricted mode, the orchestrator rejects a template that sets the in-container egress filter or start-up package installation. | v1 | These Open Terminal functions need root or `NET_ADMIN`. Warren rejects a policy that sets them. |
| Limit on the number of instances for each user. | v1 | The documentation describes no such limit. Addition in warren. |

## 11. Environment variables

| Feature in the documentation | Plan | Note |
|---|---|---|
| The orchestrator passes the template variables to each instance. | v1 | |
| The orchestrator removes the Open Terminal API key variable from a template. | v1 | |
| The orchestrator adds variables that give the CPU and memory limits to Open Terminal. | v1 | |
| A system prompt variable in the orchestrator environment becomes the default for all instances. | Later | An administrator can set the variable in each policy. |

## 12. Authentication and access

| Feature in the documentation | Plan | Note |
|---|---|---|
| One static API key between Open WebUI and the orchestrator, sent as a bearer token. | v1 | The warren specification calls this the static API key mode. |
| The orchestrator trusts all requests that have the key. Open WebUI gives the identity in headers. | v1 | Warren logs a warning in this mode. Refer to `protocol.md`, section 9.3. |
| The documentation describes no token validation in the current version. | v1 | Addition in warren: validation of the Open WebUI session token. Limits apply. Refer to `protocol.md`, section 9.1. |
| If no key is set, the orchestrator makes one at each start and prints it. | Won't do | Warren does not start without a key from a Secret. |
| The same key opens the administrator interface and the template API. | v1 | Optionally a second key for the management API. |
| Connection access grants in Open WebUI: read and write, for users and groups. | Not applicable | Open WebUI applies them before it calls warren. |
| Sharing of one terminal with users and groups in Open WebUI. | Not applicable | Open WebUI keeps the grants. This is the primary method to give a policy to users or groups. |
| Selection of a policy by user or group in the orchestrator. | v1 | Not in the documentation. Addition in warren. The source of group data is an open question. Refer to `protocol.md`, section 9.4, question 4. |

## 13. Administrator interface and API

| Feature in the documentation | Plan | Note |
|---|---|---|
| Web interface with template and instance views. | Later | The warren specification puts the administrator interface in a later release. |
| Template API. The documentation does not list its paths. | v1 | Read-only in v1. Warren uses only the client behaviour in `protocol.md`, section 4.6. |
| Instance list with state, and system information for each instance. | v1 | The instance list comes from the management API. Open Terminal supplies the system information through the proxy. |

## 14. Network and isolation

| Feature in the documentation | Plan | Note |
|---|---|---|
| Optional NetworkPolicy objects for the orchestrator and the instance pods. The default is "off". | v1 | Always on in warren. Each sandbox gets a default-deny policy. Ingress is possible only from warren. |
| Egress control with the in-container domain filter of Open Terminal. It needs `NET_ADMIN`. | Won't do | It conflicts with the restricted Pod Security Standard. |
| Egress control with Kubernetes NetworkPolicy. | v1 | Rules by CIDR and port in each policy. |
| Egress control by domain name. | Later | Standard NetworkPolicy has no domain rules. Some CNI plug-ins have them. |
| Namespace setting for instances. | v1 | One dedicated namespace for sandboxes. |
| Labels on instance pods. | v1 | Warren also uses labels and annotations for its state. |
| One Service for each instance. | Won't do | Warren connects to the pod IP address. Fewer objects and fewer permissions are necessary. |
| The file browser root setting is visual only and is not a security control. | Not applicable | Open Terminal behaviour. The threat model must include this fact. |
| Port previews, the browser view, and the desktop view go through the proxy. | v1 | Warren proxies these paths. Refer to `open-terminal-api.md`. |

## 15. Observability

| Feature in the documentation | Plan | Note |
|---|---|---|
| Logs with a level setting. | v1 | Structured JSON logs. |
| Metrics. The documentation describes none. | v1 | Addition in warren: Prometheus metrics. |
| Audit log. The documentation describes none. | v1 | Addition in warren: a record of each sandbox creation and each access. |
| Tracing. The documentation describes none. | Later | |

## 16. Scale

| Feature in the documentation | Plan | Note |
|---|---|---|
| More than one worker process or replica. | Later | Warren v1 operates with one active replica. State in Kubernetes makes a restart safe. |
| More than one orchestrator connection in Open WebUI. | Not applicable | Open WebUI function. More than one warren deployment is possible. |
| Multi-cluster operation. The documentation describes none. | Later | The warren specification puts it in a later release. |
| Quota interface. | Later | The warren specification puts it in a later release. |

## 17. Version compatibility

The documentation says that the current orchestrator needs Open WebUI 0.13.4 or later.
Warren v1 has the same minimum: Open WebUI 0.13.4.

## 18. Items that the documentation does not define

Warren must make its own decision for each of these items. No public source gives the behaviour of the orchestrator.

1. The events that reset the idle timer. Warren proposal: each proxied HTTP request and each open WebSocket.
2. The behaviour for a request that arrives while a pod starts on Kubernetes.
3. The health endpoint and the path layout of the orchestrator.
4. The format of the CPU, memory, and storage values. Warren proposal: Kubernetes quantity syntax.
5. The relation between the instance name and the template ID and context.
6. The limits on the number of instances.
7. The behaviour with more than one replica.
