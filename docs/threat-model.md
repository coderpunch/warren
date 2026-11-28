# Threat model

This document gives the threats to a warren installation, what warren does against each threat, and what stays open.
The mechanisms are in [isolation.md](isolation.md), [policies.md](policies.md), and [protocol.md](protocol.md).

## 1. System

```text
browser --> Open WebUI --> warren --> sandbox pod (Open Terminal)
                             |
                             +--> Kubernetes API (sandbox namespace only)
```

| Part | Trust |
|---|---|
| Open WebUI server | Trusted. It authenticates the users and sets the identity headers. It has the connection key. |
| Administrator of warren | Trusted. The administrator writes the policy file and the configuration. |
| Administrator of the cluster | Trusted. |
| User of Open WebUI | Not trusted. A user controls the commands of a sandbox and some identifiers (section 4.3). |
| Model | Not trusted. A model writes commands for a sandbox. Text from a web page or a file can change what a model does. |
| Sandbox pod | Hostile. Each byte that comes from a sandbox is hostile data. |
| Manager of a connection in Open WebUI | Trusted in part. Such a person can send instance operations for all instances of a terminal. |

## 2. What warren protects

| Asset | Protection goal |
|---|---|
| Connection key | Only the Open WebUI server has it. A sandbox never gets it. |
| Files of a user | Only the sandbox of that user, chat, or model can read them. |
| Other sandboxes | A sandbox cannot connect to a different sandbox. |
| Warren | A sandbox cannot connect to warren. |
| Cluster | A sandbox cannot connect to the Kubernetes API, the nodes, and the services of the cluster. |
| Nodes | A sandbox cannot use all resources of a node. |

## 3. Assumptions

Warren is safe only if these statements are true:

1. Only the Open WebUI server has the connection key, and only Open WebUI can connect to the port of warren. Use a network policy for the namespace of warren, and use TLS.
2. Open WebUI authenticates each user correctly.
3. The network plugin of the cluster applies NetworkPolicy objects. Warren tests this (isolation.md, section 4).
4. The sandbox namespace is only for sandboxes, and it enforces the "restricted" Pod Security Standard.
5. The nodes and the container runtime have the current security corrections.
6. Only the administrator can change the policy file, the Secret with the connection key, and the objects in the two namespaces.

Warren does not trust a sandbox for a decision. The key of a pod is in the environment of the pod, and the user of the sandbox can read it.
The server in a sandbox and the user have the same user ID. Thus the server of a sandbox is not a trusted program after its start.

## 4. Threats

### 4.1 False identity (header spoofing)

| Threat | Protection | What stays open |
|---|---|---|
| A client with no key sends a request with identity headers. | Each request needs the connection key. Warren compares it in constant time, and a key has a minimum of 32 characters. | |
| A user sends identity headers through Open WebUI. | Open WebUI sets the identity headers on its server. Warren refuses a header that occurs two times and a header with control characters. | Warren trusts each client that has the connection key. A stolen key gives access to all sandboxes. Keep the key in a Secret, replace it regularly (warren reads a new key file with no restart), and limit the network access to warren. |
| A sandbox sends requests to warren with false headers. | A sandbox has no connection key, and its network policy does not permit a connection to warren. | |
| A sandbox sends a response that makes Open WebUI send the connection key to a different address. | Warren removes or corrects redirects. For the routes of Open Terminal, it sends only permitted response headers. For the routes of an application in a sandbox, it uses a list of refused headers. It removes `Set-Cookie` and the headers that change the state of the browser for the full origin. Warren gives Open WebUI a tool list from a document that is part of warren. A sandbox can only remove tools from the list. | |
| A sandbox request gets to the management API. | The management API refuses each request that has an identity header. | |
| A user sends the chat ID or the model ID of a different user. | Open WebUI examines the access to a saved chat and to a model. Warren uses the IDs that the Open WebUI server sends. | Open WebUI does not examine each model ID (protocol.md, section 6). All users who can send a model ID share the sandbox of that model. Use the scope `model` only for data that these users can share. |
| A manager sends an instance operation with the user ID of a different user. | The operations stop, start, or delete a sandbox. They give no access to its files. A sandbox that a manager starts for a chat or a model is on the account of no user. | A manager can delete the files of each instance of the terminal. This is the function of the delete operation. |

### 4.2 Escape from the sandbox

| Threat | Protection | What stays open |
|---|---|---|
| Code in a sandbox gets the privileges of root in the container. | The pod is not root, cannot get new privileges, has no capabilities, and uses the default seccomp profile. The root file system is read-only. | |
| Code in a sandbox uses an error of the kernel or of the container runtime to get to the node. | The settings above make this more difficult. A policy can set a runtime class (gVisor, Kata Containers) and a user namespace. | Without a runtime class, all sandboxes use the kernel of the node. Use a runtime class, and put the sandboxes on nodes that have no other workloads (`nodeSelector` and `tolerations` of a policy). |
| Code that got to the node reads the files of other sandboxes. | With `userNamespace: true`, each sandbox has different user IDs on the node. | Without a user namespace, all sandboxes are the user 1001 and the group 1001 on the node. |
| Code in a sandbox uses the Kubernetes API. | A sandbox has no service account token, and the network policy does not permit the address of the API. | |
| A user puts a program or a Python module into a persistent home directory to replace the server at the next start. | The start command has absolute paths and makes Python ignore the home directory (isolation.md, section 2.1). | After the start, the server runs programs of the home directory when the user asks for them. This gives the user nothing new: the user already controls the sandbox. For the scopes `shared` and `model`, each user of the sandbox can change what the other users of the same sandbox get. |
| A policy file gives a sandbox more privileges. | A policy cannot change the security settings. It cannot set the variables that select the libraries or the mode of the server. | The administrator selects the image and the command. An image is trusted software. |

### 4.3 Movement to other systems (lateral movement)

| Threat | Protection | What stays open |
|---|---|---|
| A sandbox connects to a different sandbox. | Only the pods of warren can connect to a sandbox. | |
| A sandbox connects to warren, to the Kubernetes API, to a node, or to a service of the cluster. | The policy of the namespace refuses all connections. The policy of a sandbox permits only public addresses, or nothing (isolation.md, section 3). | Addresses of the cluster that are public: add them with `--egress-blocked-cidrs`. Warren finds the nodes that have a sandbox, and its own pod network. It does not find the control plane. |
| A sandbox reads the metadata service of a cloud. | The `internet` mode does not permit the address `169.254.169.254`, the link-local networks, and the platform address of Azure. | |
| The cluster ignores network policies. | Warren tests the network plugin at its start and each hour, and starts no sandbox if the test fails. | The test examines connections to a pod on one node. It does not examine connections from a pod. Some network plugins apply a policy to a new pod some seconds after its start. |
| A different program adds a network policy that permits more. | Warren finds such policies in the sandbox namespace and starts no new sandbox. | Warren cannot see cluster-wide policies of a network plugin. |
| A person deletes or changes a policy of warren. | Warren repairs its policies each 30 seconds. | For a maximum of 30 seconds, the change applies. |
| An administrator removes a network permission, and a sandbox has an open connection. | Warren stops the sandbox. | |
| A sandbox uses DNS to find services or to send data out. | With `egress: none`, DNS is off unless the policy sets `dns: true`. | With DNS, a sandbox can read the names of the services of the cluster, and it can send data out in DNS questions. |
| A user reads the files of a different user. | Each sandbox has its own volume. The name of the volume comes from the policy ID and the scope key. Warren never gives a volume to a different sandbox. | A model ID can get a new owner in Open WebUI. A volume of the scope `model` then goes to the new model with that ID. Volumes of the scopes `chat` and `model` must have a retention time. |
| A sandbox with internet access attacks other systems, or sends the files of the user to the internet. | The mode `none` and the `allow` list. | With the `internet` mode, all that a user or a model does comes from the addresses of your cluster. Text in a web page or a file can make a model send data out. Use `none` for data that must not leave. |

### 4.4 Use of all resources (resource exhaustion)

| Threat | Protection | What stays open |
|---|---|---|
| A sandbox uses all CPU, memory, or disk space of a node. | Each sandbox has limits for CPU, memory, and ephemeral storage. A policy cannot remove them. | Kubernetes stops a pod some time after it is above the limit for ephemeral storage. A sandbox can make very many small files. Use nodes only for sandboxes. |
| A sandbox starts very many processes. | | Kubernetes has no limit for processes in a pod specification. Set `podPidsLimit` in the configuration of the kubelet. |
| A sandbox fills its volume, or the storage of all volumes. | The size of the volume in the policy. | The storage class must enforce the size (policies.md, section 7.2). |
| A user starts very many sandboxes. | Limits for the sandboxes of each user and for all sandboxes. Warren stops a sandbox that has no use. An open WebSocket is not a use, and data that a sandbox sends is not a use. The WebSocket of an agent session is the exception: it keeps its sandbox while it is open. | The idle time is a rule for resources, and not a security boundary. While a client of Open WebUI has a connection to a sandbox, the sandbox can make that client send requests and messages, and these are a use: a close after which the client connects again, a change of a file that the page shows, output in a visible terminal. The Open WebUI backend keeps the WebSocket of an agent session open for one chat turn, with no browser, and it has no time limit for a turn. Thus an agent process that does not end its turn keeps its sandbox until the user stops the turn, Open WebUI stops, or a person sends a stop operation. A connection that breaks does not end the turn. After each close, the backend connects again after 1 second, and the new WebSocket keeps the sandbox again. The backend stops only when, at a close, it got no message on the WebSocket for more than 60 seconds. The sandbox sends these messages (protocol.md, section 4.7). With no client that has a connection, a sandbox cannot cause a use. But warren applies the idle stop only to a sandbox that is ready. A sandbox that makes itself not ready does not stop after its idle time. It stops after the time of the flag `--start-timeout`, and that time starts again each time the pod changes from ready to not ready. Warren has no maximum lifetime for a sandbox. The limits on the number of sandboxes are the hard limit. Refer to policies.md, section 10.4. |
| A user makes very many volumes, with new chats or model IDs. | Limits for the volumes of each user and for all volumes. The scopes `chat` and `model` have no volume by default, and a retention time is necessary for them. | The limit for all volumes has the default "no limit". Set `--max-volumes`, or a ResourceQuota for the sandbox namespace. |
| A client opens very many connections to warren before it authenticates. | Limits for connections with no authentication, a time limit for the authentication, and a size limit for messages. | Put a rate limit before warren if clients that are not Open WebUI can connect. |
| A sandbox sends a very large or slow response. | Size limits for the tool list, time limits for writes, and no buffers for file transfers. | |
| A sandbox writes very much to its log. | | Set the log size limits of the container runtime. |
| All sandboxes use shared limits of the kernel for one user ID, for example file watches. | A policy with `userNamespace: true` gives each sandbox its own user IDs. | Without a user namespace, set the `fs.inotify` limits of the nodes for the number of sandboxes. |
| A sandbox uses all network capacity. | | Kubernetes has no standard limit. Use the functions of your network plugin. |

### 4.5 Data at rest and records

| Threat | Protection | What stays open |
|---|---|---|
| A person with access to the storage reads the files of users. | | Warren does not encrypt volumes. Use a storage class that does. |
| A person with access to the Kubernetes data reads the keys. | Each pod key is valid for one pod, and only from a pod of warren. | Turn on the encryption of Secrets in the cluster. |
| A deleted volume keeps its data. | Warren deletes the PersistentVolumeClaim. | The storage class decides what occurs with the data. Use the reclaim policy `Delete`. |
| The audit records show personal data. | Warren does not write tokens, keys, or request bodies to its log. It hides the token in the path of a browser view. | The records have user IDs, chat IDs, and paths. Protect the log. |

## 5. What warren does not protect against

- An Open WebUI server that an attacker controls. It has the connection key and can use all sandboxes.
- An administrator of the cluster or of warren who does damage.
- An error in the kernel when no runtime class is in use.
- What a user or a model does with the data in its own sandbox.
- Attacks from a sandbox on the internet, when the policy permits internet access.
- Loss of data. Warren makes no copies of volumes.

## 6. Checklist for an installation

1. Put warren and the sandboxes in two namespaces. Use the sandbox namespace for nothing else.
2. Give the sandbox namespace the label `pod-security.kubernetes.io/enforce: restricted`.
3. Use a network plugin that applies network policies, and read the result of the test in the log of warren.
4. Permit connections to warren only from Open WebUI, and use TLS between them.
5. Keep the connection key in a Secret. Give it only to Open WebUI and to warren.
6. Give the service account of warren only the permissions of isolation.md, section 9, and policies.md, section 12.
7. If the cluster has public addresses, set `--egress-blocked-cidrs`.
8. Put the sandboxes on their own nodes, and use a runtime class if the users are not known persons.
9. Set `podPidsLimit` on the nodes, and a ResourceQuota for the sandbox namespace.
10. Use a storage class that enforces the size of a volume and deletes the data of a deleted volume.
11. Set `egress: none` for policies whose data must not leave the cluster.
