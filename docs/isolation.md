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

The cluster must support user namespaces: the Kubernetes API (the feature `UserNamespacesSupport`), the container runtime, the kernel, and the file system of the volumes.

- If the Kubernetes API ignores the setting, warren deletes the pod, and the request gets an error. The log of warren gives the cause. Kubernetes before version 1.33 ignores the setting by default.
- If a node cannot make the user namespace, the pod does not become ready. Warren stops the pod after the time of the flag `--start-timeout`. Warren does not read the events of Kubernetes: `kubectl describe pod` shows the cause (`FailedCreatePodSandBox`).
- If the container cannot start in the user namespace, warren writes the message of Kubernetes to its log and stops the pod (section 7, `start_error`).

Kubernetes gives a pod 65536 user IDs by default. With `userNamespace: true`, use a `runAsUser` value from 1 to 65535.

### 2.3 Runtime class

The key `runtimeClassName` of a policy selects a container runtime with a stronger boundary, for example gVisor or Kata Containers.
Use it when the users are not known persons.

## 3. Network policies

### 3.1 Policy of the namespace

Warren makes the policy `warren-default-deny` in the sandbox namespace. It selects all pods, and it permits no connection in the two directions.
A pod that has no policy of its own can thus open no connection, and no pod can connect to it.

Warren makes this policy only when the namespace has no pods of a different owner. The policy would stop all their connections.
Warren examines the policy each 30 seconds. If a person changes or deletes it, warren repairs it.
If warren cannot repair it, warren is not ready and starts no sandbox until the policy is correct again.

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
| The address of each node that has a sandbox, if it is not in one of the networks above | A node can have a public address. Refer to the limits below. |
| The networks of the flag `--egress-blocked-cidrs` | Refer to the text below. |

Your cluster can have addresses that are not in these networks. Add them with the flag `--egress-blocked-cidrs`:

- The pod network or the service network, if it is not a private network.
- The addresses of the nodes, if they are public. Warren finds only the nodes that have a sandbox, and only the address in the field `status.hostIPs` of the pod. That is usually the internal address of the node, not an external address that a cloud gives to it. Warren does not find the control plane.
  Warren knows the node of a new sandbox only after Kubernetes selected it. Warren adds the address to the policy of the sandbox before the first request gets to the sandbox. It adds the address to the policies of the other sandboxes immediately after that. If your nodes have public addresses, do not rely on this function only: set the flag.
- A public address of the Kubernetes API.
- A NAT64 network that is not `64:ff9b::/96`.

At its start, warren examines its own pod address and the service address of the Kubernetes API.
If one of them is not in a blocked network, warren stops (section 5).
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

Warren examines the namespace each 30 seconds. It looks for policies that it did not make and that have a rule.
If it finds one, it starts no new sandbox and writes an error to its log.
To accept such a policy, give its name in the flag `--accepted-network-policies`.

Warren cannot see policies that are not NetworkPolicy objects of the namespace, for example the cluster-wide policies of a network plugin.

## 4. Test of the network plugin

A network policy is a request to the network plugin of the cluster. Some plugins ignore it, and Kubernetes gives no error.
Warren thus makes a test at its start, and again each hour:

1. Warren starts one small pod in the sandbox namespace, with a network policy as a sandbox has it.
2. Warren connects to the pod. The connection must be successful. This shows that the namespace and the pod labels in the configuration of warren are correct.
3. Warren deletes the policy of the pod. Only the policy of the namespace applies now.
4. Warren connects again, 3 times or more. The connection must fail 3 times, one after the other. One failure is not sufficient: a connection can fail one time for a different cause.
5. Warren makes the policy again and connects again. The connection must be successful. This shows that the network policy was the cause of the failures in step 4. A pod that went away, or a fault of warren, was not the cause.
6. Warren deletes the pod.

A connection is successful only when the HTTP server of the test pod answers with the status 200. An open TCP connection is not sufficient.
A proxy of a service mesh on the pod of warren can accept a connection that does not get to the test pod.

The test pod uses the image of the flag `--sandbox-image`, and the program `/usr/local/bin/python3` of that image.
It does not use the `command` of a policy.
It gets the `imagePullSecrets`, the node selector, and the tolerations of one policy that uses this image.
Warren selects the policy with the smallest number of node selector labels. If two policies have that number, warren selects the first one in the order of the IDs.
If the test pod cannot start, the test is not possible. The log of warren then gives the cause of Kubernetes.

| Result | What warren does |
|---|---|
| Step 2 fails | The result of the previous test stays, and warren writes an error to its log. With no previous test, warren is not ready and starts no sandbox. The log names the flags `--warren-namespace` and `--warren-pod-labels`. |
| A connection in step 4 is still successful after 20 seconds | The cluster does not apply network policies. Warren is not ready and starts no sandbox. |
| Step 5 fails, or the test is not possible | The result of the previous test stays. With no previous test, warren is not ready. |

Warren does the test again each minute while it has no proof that the cluster applies network policies.
While the proof of a previous test stays, warren does the next test after one hour. This is also the case after a test that had no result.

The test is not possible in these conditions, for example:

- Kubernetes refuses the test pod.
- The test pod does not become ready in 4 minutes.
- The test pod goes away before the end of the test.
- The policy of the namespace (section 3.1) is absent, or it changes before the end of step 4. Only that policy refuses the connections of step 4.

A second warren process can do its test at the same time, for example during a rollout. Warren thus removes a test pod of a different test only when it stopped or when it is older than 7 minutes.

While warren is not ready, it continues to stop idle sandboxes and to repair the network policies of the sandboxes that operate.
The management API of warren still stops and deletes sandboxes. A start and a restart are not possible.
Warren also refuses each new request to a sandbox, also when the sandbox operates. A terminal that is open stays open.

- An HTTP request gets the status 503.
- A new WebSocket does not get the status 503. Warren accepts it with the status 101, and then closes it with the close code 1013. The browser client of Open WebUI tries again after that code. The log record `access` of the WebSocket has the status 101 and the detail `sandbox not ready`.
- An HTTP request or a WebSocket that waits for the start of a sandbox gets the same result as a new one. It gets no sandbox after warren became not ready. The result can come some seconds later.
- Warren does not refuse `GET /openapi.json`. It replies with the status 200 and its complete tool list, and it sends no request to the sandbox.

But a request must get to warren first. If the readiness probe of the warren pod uses `/readyz`, Kubernetes sends no request to a warren that is not ready. In that condition, stop the sandboxes with `kubectl delete pod`.

The readiness address of warren (`/readyz` on the admin port) gives the cause.

Limits of the test:

- It examines connections to a pod, not connections from a pod.
- It examines one node.
- Some network plugins apply a policy to a new pod some seconds after its start. In that time the pod has no isolation. Warren writes a warning if a connection in step 4 is successful 3 seconds or more after the delete of the policy. Warren waits 4 seconds between two attempts, thus it does not find a shorter delay. On the AWS VPC CNI, use the strict mode.

The flag `--skip-network-policy-test` turns the test off. Do not use it on a cluster with users.

## 5. Checks at the start of warren

Warren stops with an error message in these conditions:

- Warren operates in the sandbox namespace.
- The service account of warren does not have all necessary permissions in the sandbox namespace. The message names them.
- A namespace or a pod label in the flags is not a name that Kubernetes accepts.
- The service address of the Kubernetes API is not in a blocked network.

Warren also stops in these conditions, if it can read its own pod:

- The pod of warren does not have the labels of the flag `--warren-pod-labels`.
- The pod of warren uses the network of its node (`hostNetwork`). A network policy cannot select such a pod.
- The pod address of warren is not in a blocked network.

To read its pod, warren needs the permission of section 9 and the name of the pod.
Set the variable `WARREN_POD_NAME` from the field `metadata.name` of the pod:

```yaml
env:
  - name: WARREN_POD_NAME
    valueFrom: {fieldRef: {fieldPath: metadata.name}}
```

Without the variable, warren uses its host name. The host name is not the pod name in these conditions:

- The pod uses the network of its node. The host name is then the name of the node.
- The pod name has more than 63 characters.
- The pod sets a different host name.

If warren cannot read its pod, it writes a warning and does not do these three checks.
The test of the network plugin (section 4) then finds the first two conditions, and warren is not ready.

Warren is not ready, and writes the cause to its log, in these conditions:

- The sandbox namespace has pods of a different owner, and the policy of the namespace does not exist. This applies at the start, and also after a person deleted the policy. The flag `--allow-other-pods` accepts the pods.
- Warren cannot make or repair the policy of the namespace.
- The test of the network plugin did not have the correct result.

## 6. Flags

| Flag | Default | Function |
|---|---|---|
| `--warren-namespace` | The namespace of the pod | The namespace of the warren pods. |
| `--warren-pod-labels` | `app.kubernetes.io/name=warren` | The labels of the warren pods. |
| `--pod-name` | The host name | The name of the pod of warren. Refer to section 5. |
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
| `network_changed` | The policy file removed a network permission of the sandbox. Section 7.1 gives the changes that remove a permission. A network plugin can let an open connection continue, thus only a stop closes it. |
| `no_policy` | The policy of the sandbox is not in the file, or it has a different scope. Warren does not wait for open connections. |
| `unreachable` | The pod is ready, and the key check of warren fails for more than 60 seconds. In the key check, warren sends one request with the key of the pod to the pod. Warren makes the check only when a request needs the sandbox. It makes the check at the first use of the pod, and again after a restart of warren or of the container. A time of more than 60 seconds with no check starts the count again. A refused key is not such a failure: warren stops that pod immediately with the reason `bad_key`. After a successful check, warren makes no new check, and a connection that fails does not stop the pod. An HTTP request then gets the status 502 or 504. Each such request is a use of the sandbox, thus the idle time starts only when the requests stop. |
| `start_error` | The container cannot start, for example because the command is not in the image. The log has the message of Kubernetes. Warren stops the pod when the condition stays for 2 minutes. An image that Kubernetes cannot pull is not such a condition: warren writes the message, and the 2 minutes start at the first cause that is such a condition. |
| `public_address` | An address of the pod is not in a blocked network. |
| `no_network_policy` | A version of warren before the network policies started the pod. Warren uses this reason only in the conditions of section 8. |

### 7.1 Changes of the network settings

Warren compares the network settings that a sandbox has with the settings of the new policy file.

Only these changes are a new permission. A new permission needs no stop. Warren changes the network policy of the pod a maximum of 30 seconds after it reads the new file.

- A new item of `allow`.
- `egress` changes from `none` to `internet`.
- `dns` changes from `false` to `true`.

These changes remove a permission. Warren stops the sandboxes of the policy, also when the same change of the file adds a permission.

- `egress` changes from `internet` to `none`.
- `dns` changes from `true` to `false`.
- An item of `allow` that the sandbox has is not in the new file, or it is there with a change.

For `dns`, the default is a value: `true` for `internet`, and `false` for `none`.

Warren compares complete items of `allow`. It does not calculate which of two items permits more.
Thus each change of an item that exists stops the sandboxes: a change of `ports`, `cidr`, `namespace`, or `podLabels`.
This is also the case for a change that makes the item wider. Examples are one more port, an item with no `ports` key, a larger network, and fewer labels.
The same ports in a different sequence are also a change. A different sequence of the items is not a change.

To add a port or a network with no stop, add a second item and keep the first item:

```yaml
allow:
  - cidr: 203.0.113.0/24
    ports: [443]
  - cidr: 203.0.113.0/24
    ports: [8443]
```

## 8. Upgrade from a version with no network policies

- The upgrade stops all sandboxes. Their volumes stay. Files of a sandbox with ephemeral storage are lost.
- The new version stops the sandboxes of the previous version with the reason `no_network_policy` only when these two conditions are true:
  - The sandbox namespace has its policy (section 3.1).
  - The new version has the proof that the cluster applies network policies (section 4). With the flag `--skip-network-policy-test`, the new version does not wait for a test.
- While one of the two conditions is not true, the new version is not ready, and it does not use that reason. It makes no network policy for those sandboxes.
  It stops them with these reasons, as it stops a sandbox of the new version: `idle`, `failed`, `not_ready`, `volume_gone`, `no_policy`, and `start_error`.
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
| Pod, NetworkPolicy | `warren-canary-` and 8 hexadecimal characters | Some seconds, during the test of the network plugin. |

Permissions of the service account of warren in the sandbox namespace, in addition to the permissions of [policies.md](policies.md), section 12:

| Resource | Verbs |
|---|---|
| `networkpolicies` (`networking.k8s.io`) | `get`, `list`, `create`, `update`, `patch`, `delete` |

Optional permission in the namespace of warren, to let warren examine its own pod:

| Resource | Verbs |
|---|---|
| `pods` | `get` |
