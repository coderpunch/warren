# Isolation of a sandbox

This document describes how warren isolates a sandbox from the cluster, from warren, and from other sandboxes.
The threat model is in [threat-model.md](threat-model.md). The policy file is in [policies.md](policies.md).

## 1. Summary

A sandbox runs code of a user or of a model. Warren treats that code as hostile.

| Subject | What warren does |
|---|---|
| Namespace | All sandboxes are in one namespace that has no other pods. Warren is in a different namespace. |
| Pod security | Each pod has the settings of the "restricted" Pod Security Standard. A policy cannot make them weaker. |
| Start command | A fixed command starts the server. It does not use files of the home directory. |
| Kubernetes API | A sandbox has no service account token. |
| Key | Each pod has its own random API key in its own Secret. The key is valid for that pod only. |
| Network, ingress | Only the pods of warren can connect to a sandbox, and only to the port of the server. |
| Network, egress | A sandbox can connect to public addresses, or to nothing. It cannot connect to the cluster. |
| Proof | Warren tests at its start that the cluster applies network policies. If the test fails, warren starts no sandbox. |

## 2. Pod security

Each sandbox pod has these settings. They are the same for all policies.

| Setting | Value |
|---|---|
| `runAsNonRoot` | `true` |
| `runAsUser` | `1001`, or the key `runAsUser` of the policy |
| `runAsGroup`, `fsGroup` | `1001` |
| `seccompProfile` | `RuntimeDefault` |
| `allowPrivilegeEscalation` | `false` |
| `capabilities` | all capabilities dropped |
| `readOnlyRootFilesystem` | `true` |
| `automountServiceAccountToken` | `false` |
| Host network, host process IDs, host IPC | Not used |

The sandbox can write to two directories only: the home directory `/home/user` and the temporary directory `/tmp`.

The user ID and the group ID are numbers. Thus they do not depend on the image.
The server of Open Terminal operates with each user ID. The user has no name in the image, thus a shell can show `I have no name!`.

Give the sandbox namespace the label `pod-security.kubernetes.io/enforce: restricted`.
Kubernetes then refuses each pod in the namespace that does not have these settings.

### 2.1 Start command

The default command is:

```text
/usr/bin/tini -- /usr/local/bin/python3 -I -u -m open_terminal run --host 0.0.0.0 --port 8000
```

- The command does not use the entry point of the image.
- Each program has an absolute path. The home directory is in the `PATH` of the image, and the user can write to it.
- The option `-I` makes Python ignore the modules of the home directory.

Thus a file in a persistent home directory cannot replace the server at the next start.

The key `command` of a policy replaces the default command. Use it for an image with a different layout.
The first item must be an absolute path. Warren adds the arguments `run --host 0.0.0.0 --port 8000`.
Do not use a script of the image or the program `open-terminal` as the command: they find programs through `PATH`.

| Image | Command |
|---|---|
| `open-terminal:<version>-openshift` (the default) | The default command |
| `open-terminal:<version>-slim` | The default command |
| `open-terminal:<version>-alpine` | `["/sbin/tini", "--", "/usr/local/bin/python3", "-I", "-u", "-m", "open_terminal"]` |

The fixed command protects the start of the server only.
After the start, the server and the user have the same user ID. The server starts programs of the home directory when the user asks for them.
Warren does not trust a sandbox at any time. Refer to section 3 of the threat model.

### 2.2 User namespace

The key `userNamespace: true` of a policy gives the pod its own user namespace (`hostUsers: false`).
The user IDs of the sandbox are then different user IDs on the node, and each sandbox has a different range.
If code gets out of the container, it has no user ID of a different sandbox and no user ID of the node.

The cluster must support user namespaces: the container runtime, the kernel, and the file system of the volumes.
If it does not, the pod does not start, and warren writes the cause to its log.

### 2.3 Runtime class

The key `runtimeClassName` of a policy selects a container runtime with a stronger boundary, for example gVisor or Kata Containers.
Use it when the users are not known persons.

## 3. Network policies

### 3.1 Policy of the namespace

Warren makes the policy `warren-default-deny` in the sandbox namespace. It selects all pods, and it permits no connection in the two directions.
A pod that has no policy of its own can thus open no connection, and no pod can connect to it.

Warren makes this policy only when the namespace has no pods of a different owner. The policy would stop all their connections.
Warren examines the policy each 30 seconds. If a person changes or deletes it, warren repairs it.

### 3.2 Policy of each sandbox

Each start of a sandbox has its own policy. It has the name of the Secret of that start, and it selects the pod of that start only.
Warren makes the policy before the pod. Kubernetes deletes it with the pod.

Ingress: the pods that have the labels of warren, in the namespace of warren, on TCP port 8000. No other source.

Egress: refer to the key `network` of the policy, in [policies.md](policies.md), section 14.

| `egress` | Permitted connections |
|---|---|
| `internet` (default) | The DNS service of the cluster, and all addresses that are not in a blocked network (section 3.3). |
| `none` | No connection. With `dns: true`, the DNS service of the cluster. |

The items of `allow` add destinations in the two modes.

### 3.3 Blocked networks

The `internet` mode does not permit these networks:

| Networks | Cause |
|---|---|
| `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7` | Private networks. The pods, the services, and the nodes of most clusters are here. |
| `169.254.0.0/16`, `fe80::/10` | Addresses of the same link. The metadata service of a cloud is here. |
| `168.63.129.16/32` | The platform service of Azure. |
| `127.0.0.0/8`, `::1/128`, `0.0.0.0/8`, `192.0.0.0/24`, `198.18.0.0/15`, `224.0.0.0/4`, `240.0.0.0/4`, `ff00::/8` | Special networks. |
| `64:ff9b:1::/48`, `2002::/16`, and the NAT64 form of each blocked IPv4 network | IPv6 addresses that contain an IPv4 address. |
| The address of each node that has a sandbox | A node can have a public address. |
| The networks of the flag `--egress-blocked-cidrs` | Refer to the text below. |

Your cluster can have addresses that are not in these networks. Add them with the flag `--egress-blocked-cidrs`:

- The pod network or the service network, if it is not a private network.
- The addresses of the nodes, if they are public. Warren finds only the nodes that have a sandbox. It does not find the control plane.
- A public address of the Kubernetes API.
- A NAT64 network that is not `64:ff9b::/96`.

At its start, warren refuses a configuration in which its own pod address or the service address of the Kubernetes API is not in a blocked network.
Warren also stops a sandbox whose pod address is not in a blocked network. The log gives the flag to set.

### 3.4 DNS

A sandbox with DNS can ask the DNS service of the cluster. The default selects the pods with the label `k8s-app=kube-dns` in the namespace `kube-system`, on port 53.

| Flag | Use |
|---|---|
| `--dns-namespace`, `--dns-pod-labels` | A DNS service with a different namespace or different labels. |
| `--dns-ports` | The port of the DNS pods, not of the service. On OpenShift it is `5353`. |
| `--dns-cidrs` | A DNS cache on each node, for example `169.254.20.10/32`. |

With DNS, a sandbox can find the names of the services of the cluster. It cannot connect to them.
A DNS server sends questions to the internet. Thus `egress: none` with `dns: true` is not "no connection": data can go out in DNS questions.

### 3.5 Destinations in the cluster

A `cidr` item is for a destination that is not in the cluster.
For pods of the cluster, use the form with `namespace` and `podLabels`. The addresses of pods change, and some network plugins do not compare a network with the address of a pod.

Most network plugins examine a connection after the change from the service address to the pod address.
Thus give the port of the pod, not the port of the service.

### 3.6 Other policies in the namespace

A network policy can only permit connections. A policy of a different owner in the sandbox namespace can thus permit what warren refuses.
Some platforms make such policies in each namespace.

Warren examines the namespace each 30 seconds. If it finds a policy that warren did not make, and that has a rule, warren starts no new sandbox and writes an error to its log.
To accept such a policy, give its name in the flag `--accepted-network-policies`.

Warren cannot see policies that are not NetworkPolicy objects of the namespace, for example the cluster-wide policies of a network plugin.

## 4. Test of the network plugin

A network policy is a request to the network plugin of the cluster. Some plugins ignore it, and Kubernetes gives no error.
Warren thus makes a test at its start, and again each hour:

1. Warren starts one small pod in the sandbox namespace, with a network policy as a sandbox has it.
2. Warren connects to the pod. The connection must be successful. This shows that the namespace and the pod labels in the configuration of warren are correct.
3. Warren deletes the policy of the pod. Only the policy of the namespace applies now.
4. Warren connects again. The connection must fail.
5. Warren deletes the pod.

| Result | What warren does |
|---|---|
| Step 2 fails | Warren is not ready and starts no sandbox. The log names the flags `--warren-namespace` and `--warren-pod-labels`. |
| Step 4 is successful | The cluster does not apply network policies. Warren is not ready and starts no sandbox. It does the test again each minute. |
| The test is not possible | The result of the previous test stays. With no previous test, warren is not ready. |

The readiness address of warren (`/readyz` on the admin port) gives the cause.

Limits of the test:

- It examines connections to a pod, not connections from a pod.
- It examines one node.
- Some network plugins apply a policy to a new pod some seconds after its start. In that time the pod has no isolation. If the test finds a slow change, warren writes a warning. On the AWS VPC CNI, use the strict mode.

The flag `--skip-network-policy-test` turns the test off. Do not use it on a cluster with users.

## 5. Checks at the start of warren

Warren stops with an error message in these conditions:

- Warren operates in the sandbox namespace.
- The service account of warren does not have all necessary permissions in the sandbox namespace. The message names them.
- The pod of warren does not have the labels of the flag `--warren-pod-labels`.
- The pod of warren uses the network of its node (`hostNetwork`). A network policy cannot select such a pod.
- The pod address of warren, or the service address of the Kubernetes API, is not in a blocked network.

Warren is not ready, and writes the cause to its log, in these conditions:

- The sandbox namespace has pods of a different owner, and the policy of the namespace does not exist. The flag `--allow-other-pods` accepts them.
- The test of the network plugin did not have the correct result.

## 6. Flags

| Flag | Default | Function |
|---|---|---|
| `--warren-namespace` | The namespace of the pod | The namespace of the warren pods. |
| `--warren-pod-labels` | `app.kubernetes.io/name=warren` | The labels of the warren pods. |
| `--ingress-cidrs` | None | More sources that can connect to a sandbox. Only for a warren that is not a pod, on a network where the pod addresses are reachable. |
| `--egress-blocked-cidrs` | None | More networks that the `internet` mode does not permit. |
| `--dns-namespace`, `--dns-pod-labels`, `--dns-ports`, `--dns-cidrs` | Refer to section 3.4 | The DNS service of the cluster. |
| `--accepted-network-policies` | None | Names of policies of a different owner that you accept. |
| `--allow-other-pods` | Off | Accept pods of a different owner in the sandbox namespace. |
| `--skip-network-policy-test` | Off | Do not test the network plugin. |

Each flag has an environment variable: `WARREN_` and the name of the flag in upper-case letters, with `_` in place of `-`.
The variables of the first two flags are `WARREN_NAMESPACE` and `WARREN_POD_LABELS`.

## 7. What stops a sandbox

Warren stops a pod in these conditions, in addition to the conditions of [policies.md](policies.md):

| Reason in the log and the metrics | Condition |
|---|---|
| `network_changed` | The policy file removed a network permission of the sandbox. A network plugin can let an open connection continue, thus only a stop closes it. |
| `no_policy` | The policy of the sandbox is not in the file, or it has a different scope. Warren does not wait for open connections. |
| `unreachable` | The pod is ready, and warren cannot connect to it for 60 seconds. |
| `start_error` | The container cannot start, for example because the command is not in the image. The log has the message of Kubernetes. |
| `public_address` | The pod address is not in a blocked network. |
| `no_network_policy` | A version of warren before the network policies started the pod. |

A new network permission needs no stop: warren changes the network policy of the pod in 30 seconds.

## 8. Upgrade from a version with no network policies

- The upgrade stops all sandboxes. Their volumes stay. Files of a sandbox with ephemeral storage are lost.
- If warren and the sandboxes are in one namespace, warren does not start. Keep the namespace with the volumes as the sandbox namespace, and move warren to a new namespace.
- To go back to the previous version: stop the new version, then delete the network policies of warren:

```bash
kubectl -n <sandbox namespace> delete networkpolicy -l app.kubernetes.io/managed-by=warren
```

## 9. Kubernetes objects and permissions

| Object | Name | Life |
|---|---|---|
| NetworkPolicy | `warren-default-deny` | As long as warren operates. |
| NetworkPolicy | The name of the Secret of the same start | The same as its pod. |
| Pod | `warren-canary-` and 8 hexadecimal characters | Some seconds, during the test of the network plugin. |

Permissions of the service account of warren in the sandbox namespace, in addition to the permissions of [policies.md](policies.md), section 12:

| Resource | Verbs |
|---|---|
| `networkpolicies` (`networking.k8s.io`) | `get`, `list`, `create`, `update`, `patch`, `delete` |

Optional permission in the namespace of warren, to let warren examine its own pod:

| Resource | Verbs |
|---|---|
| `pods` | `get` |
