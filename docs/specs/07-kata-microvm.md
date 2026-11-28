# Specification 07: sandboxes in Kata microVMs

Status: proposal. The owner asked for this specification on 2026-11-28. The design and the open items of section 9 need the approval of the owner.

## 1. Purpose

Without a runtime class, all sandboxes use the kernel of their node (`threat-model.md`, section 4.2).
An error in that kernel can let code of a sandbox get to the node.

Kata Containers starts each pod in a small virtual machine with its own kernel. Code that breaks the kernel of the virtual machine is still in the virtual machine.

Warren can already give a runtime class to a sandbox. This work makes Kata a tested and documented configuration, and corrects three weak points that the review found.

## 2. What exists

| Item | Where |
|---|---|
| The policy key `runtimeClassName`. It is also possible in `defaults`. | `policies.md`, section 4 |
| Warren copies the value to the pod. | `internal/kube/objects.go`, `buildPod` |
| The parser examines only the form of the name. | `internal/policy/policy.go`, `Validate` |
| One sentence about gVisor and Kata Containers. | `isolation.md`, section 2.3 |

No test starts a sandbox with a runtime class on a cluster. The kind cluster of `test/cluster/` cannot start a virtual machine.

## 3. Facts

Sources, all read on 2026-11-28:

- The repository `github.com/kata-containers/kata-containers`, branch `main`: `docs/Limitations.md`, `docs/design/architecture/networking.md`, `docs/design/architecture/storage.md`.
- `kata-containers.github.io/kata-containers/kata-deploy-runtimeclasses/`.
- `kubernetes.io/docs/concepts/containers/runtime-class/` and `kubernetes.io/docs/concepts/workloads/pods/user-namespaces/`.

| No. | Fact | Source |
|---|---|---|
| F1 | The program `kata-deploy` installs Kata on the nodes and makes RuntimeClass objects. | `kata-deploy-runtimeclasses` |
| F2 | For general use, the documentation recommends the class `kata-qemu-runtime-rs`. The default installation also has classes for Cloud Hypervisor and Dragonball. | Same |
| F3 | The class `kata-fc` (Firecracker) needs a devmapper snapshotter that the operator of the cluster configures. | Same |
| F4 | Kata connects the `veth` interface of the pod to a `tap` interface of the virtual machine with traffic control rules. The `veth` interface that the network plugin made stays on the host. | `networking.md` |
| F5 | Kata does not support the network of the host, and it does not support `subPath` in a volume mount. A privileged container is different from a usual one. | `Limitations.md` |
| F6 | With the block modes of `emptyDir`, Kata does not use `emptyDir.sizeLimit` as the size of the device. A pod with a small limit can exceed the limit with no data of its own. | `Limitations.md` |
| F7 | Without a block driver, Kata shares the files of a container with the virtual machine through `virtio-fs`. One daemon on the host does this for each virtual machine. | `storage.md` |
| F8 | If the RuntimeClass of a pod does not exist, or the node cannot use its handler, the pod gets the phase `Failed`. | `runtime-class` |
| F9 | A RuntimeClass can have a node selector and tolerations (`scheduling`). Kubernetes adds them to the pod. | Same |
| F10 | A RuntimeClass can have an `overhead`. Kubernetes adds it to the resources of the pod for the scheduler and for a ResourceQuota. | Same |
| F11 | The page about user namespaces applies to runtimes that use Linux namespaces. It names Kata as a runtime that uses virtual machines. | `user-namespaces` |

Results for warren:

1. A sandbox pod uses no host network, no `subPath`, and no privileged container (`buildPod`). F5 causes no conflict.
2. Inference from F4: the network plugin applies the network policies on the host side, as for a usual pod. A test on a cluster must make sure of this (section 6).
3. A policy with ephemeral storage has an `emptyDir` with a size limit for the home directory. F6 can stop such a pod. This depends on the Kata configuration of the cluster. The cluster check must show the result.
4. Because of F8, a wrong class name gives a pod that fails immediately. Refer to section 5.3.
5. Because of F11, the result of `userNamespace: true` together with a Kata class is not defined. Refer to section 5.4.
6. Because of F10, each sandbox uses more memory than its limit. A ResourceQuota for the sandbox namespace counts the overhead.

## 4. Requirements

| No. | Requirement |
|---|---|
| R1 | An administrator can run the sandboxes of one policy, of all policies, and of all templates of users in Kata microVMs. No new policy key is necessary. |
| R2 | The test of the network plugin uses the runtime class of the sandboxes. |
| R3 | When Kubernetes cannot start a pod with the runtime class of its policy, the log of warren gives the cause, and warren does not start and delete pods with no end. |
| R4 | The cluster checks pass on a cluster with Kata, with a Kata class in `defaults`. |
| R5 | The documents give the requirements, the configuration, and the limits. |
| R6 | A policy file with no runtime class gives the same behaviour as before. |

## 5. Design

### 5.1 Configuration

No change of the policy file form. The three places for the value are:

```yaml
defaults:
  runtimeClassName: kata-qemu-runtime-rs   # all policies of the file

policies:
  - id: personal
    name: Personal sandbox
    scope: user
    runtimeClassName: kata-qemu-runtime-rs # one policy

templates:
  base:
    runtimeClassName: kata-qemu-runtime-rs # all templates of users (specification 06)
```

A user cannot change the value of `templates.base`. A template of a user then always starts in a microVM.

Put the node selector and the tolerations of the Kata nodes into the RuntimeClass object (F9), and not into each policy.

### 5.2 Test of the network plugin

At this time, the test pod gets the pull secrets, the node selector, and the tolerations of one policy (`internal/kube/canary.go`). It does not get the runtime class.
Thus the test shows the network isolation of a usual container, also when all sandboxes are microVMs.

Change: the test pod also gets the `runtimeClassName` of the policy that the test selects.

The rule that selects the policy stays. If the policies of a file have different runtime classes, one test covers one class. Refer to open item O3.

### 5.3 A pod that fails immediately

At this time, `tryAcquire` deletes a pod with the phase `Failed` and starts a new one (`internal/kube/manager.go`, case `podTerminal`).
It writes no message of Kubernetes to the log, and it does not wait between two starts.
With a wrong class name, or on a node with no Kata, each request thus makes and deletes pods until its time limit. The user gets the status 503, and the log has no cause.

Changes:

1. Before warren deletes a pod with the phase `Failed`, it writes one log record with `status.reason` and `status.message` of the pod, the policy, and the runtime class.
2. Warren counts the pods of one sandbox that fail before they are ready, one after the other. After 2 such pods, it waits before the next start. The wait and its maximum are those of a sandbox that refuses its key (`badKey.backoff`).
3. In that wait, a request gets the status 502 with a text for the user, and the log names the policy.
4. A pod that becomes ready sets the count to zero.

The count is in the memory of warren, as the count of refused keys is.

### 5.4 User namespace together with a runtime class

`userNamespace: true` gives the pod `hostUsers: false`. The Kubernetes documentation does not define this for Kata (F11).

- Warren does not refuse the combination. A different runtime class, for example a second `runc` class, can support it.
- Warren writes one warning to its log when it reads a policy that has the two keys. The warning says that the cluster check did not prove the combination.
- If the Kubernetes API ignores `hostUsers`, the code of this time already deletes the pod and reports an error (`createObjects`).

### 5.5 Resources

- Kata makes the virtual machine with the CPU and the memory of the limits of the pod. A warren sandbox always has these limits (`policies.md`, section 6).
- The memory request of a policy is less than its limit by default. A virtual machine can use all the memory of its limit. With many microVMs, set the request near the limit, so that the scheduler puts on a node only the microVMs that the node has memory for.
- The start of a microVM takes more time than the start of a container. The time limits of warren (50 seconds for a request, `--start-timeout` for the pod) need no change. The metric `warren_sandbox_cold_start_seconds` shows the real time.

### 5.6 Chart

The chart of specification 02 needs no new value. The installation document gets one paragraph that refers to `isolation.md`.

## 6. Tests

### 6.1 Unit tests

| Test | Package | What it shows |
|---|---|---|
| The test pod has the runtime class of the policy that the test selects, and no class when that policy has none. | `internal/kube` | R2, R6 |
| A pod with the phase `Failed`: the log has the reason and the message one time. | `internal/kube` | R3 |
| Three pods that fail in sequence: the third start waits, and the request gets an error with no new pod. | `internal/kube` | R3 |
| After a pod that became ready, a failed pod does not cause a wait. | `internal/kube` | R3 |
| A policy with `userNamespace: true` and a runtime class is valid, and the log has one warning. | `internal/policy`, `cmd/warren` | Section 5.4 |

### 6.2 Cluster check with Kata

The kind cluster cannot do this check. It needs a cluster whose nodes can start virtual machines.

- `test/cluster/run.sh` gets the variable `RUNTIME_CLASS`. With a value, the script puts it into `defaults.runtimeClassName` of the policy file.
- The three scripts `lifecycle.py`, `storage.py`, and `isolation.py` run with no other change.
- `isolation.py` gets one more check with `RUNTIME_CLASS`: the kernel version in the sandbox is different from the kernel version of the node.

The checks that matter most for Kata:

| Subject | Why |
|---|---|
| The network rules of `isolation.py` | Result 2 of section 3 is an inference. |
| A write to a persistent home directory, a stop, and a new start with the same files | The volume goes through `virtio-fs` (F7). The group of the files must be correct. |
| A policy with ephemeral storage and a small size | F6 |
| The read-only root file system and the writes to `/tmp` | The security settings in the virtual machine |
| The memory limit: what occurs when a process uses more, and if the sandbox stays usable | The limit is the size of the virtual machine. The result can be different from a usual container. |
| A terminal WebSocket, the port proxy, and a file transfer of 64 MiB | The data path through the `tap` interface |
| The time of a cold start | Section 5.5 |

The check is manual. The CI does not have such a cluster. Record the Kata version, the class, the Kubernetes version, and the result in `isolation.md`.

## 7. Changes

| File | Change |
|---|---|
| `internal/kube/canary.go` | The runtime class of the test pod. |
| `internal/kube/manager.go` | The log record and the count for pods that fail (section 5.3). |
| `internal/policy/policy.go` or `cmd/warren/main.go` | The warning of section 5.4. |
| `test/cluster/run.sh`, `isolation.py`, `README.md` | The variable `RUNTIME_CLASS` and the kernel check. |
| `docs/isolation.md`, section 2.3 | Refer to section 8. |
| `docs/threat-model.md`, section 4.2 | The row about an error of the kernel refers to the new section. |
| `docs/policies.md`, section 4 | The row of `runtimeClassName` refers to `isolation.md`. |

## 8. Documentation

`isolation.md`, section 2.3, becomes a full section "Runtime class and Kata Containers" with these parts:

1. What a microVM adds, and what it does not add. The network rules, the limits, and the rules for the response of a sandbox continue to be necessary.
2. Requirements of the cluster: nodes that can start virtual machines, an installation of Kata, and a RuntimeClass object.
3. Which class to use (F2), and why Firecracker is not the first selection (F3).
4. The three places for the key (section 5.1).
5. Resources: the overhead (F10), the memory request, and the ResourceQuota.
6. Limits: the user namespace (section 5.4), ephemeral storage (F6), and the functions that the cluster check did not prove.
7. The versions and the result of the cluster check.

## 9. Open items

| No. | Item |
|---|---|
| O1 | Fact to get from the owner: the platform of the nodes. Kata needs nodes that can start virtual machines: hardware with no hypervisor below it, or virtual machines of a cloud that permit a second level of virtual machines. Some managed Kubernetes services have their own Kata product. The answer selects the installation method and the class. |
| O2 | Decision of the owner: the hypervisor. The proposal is the class that the Kata documentation recommends for general use (F2). |
| O3 | Decision of the owner: if the policies of one file have different runtime classes, the test of the network plugin covers one class. The alternative is one test for each class, which takes more time and more code. |
| O4 | Fact to get from the cluster check: the result of F6 for a policy with ephemeral storage, with the Kata configuration of the cluster of the owner. |
| O5 | Fact to get from the cluster check: if the default image and the default start command operate in a Kata virtual machine with no change. |
| O6 | Fact to get from the Kata installation guide: the exact requirements of a node. This session did not read that page. |

## 10. Acceptance

1. The unit tests of section 6.1 pass.
2. On a cluster with Kata, the three cluster checks pass with `RUNTIME_CLASS`, and the kernel check shows a different kernel.
3. On a cluster with no Kata, a policy with a Kata class gives one clear log record, an error for the user, and no sequence of pods with no end.
4. `isolation.md` has the new section with the versions of the check.
5. With no runtime class in the policy file, all tests of this time pass with no change.
