# Specification 04: maximum lifetime of a sandbox pod

Status: proposal. The owner of the project must approve it.

## 1. Purpose

`feature-parity.md`, section 7, has this row with the plan value "Later":

> Maximum lifetime, for the orchestrator and for each template. The orchestrator stops the instance, also during use.

Warren stops a sandbox only when it is idle. A sandbox with an open terminal can operate with no end.
A maximum lifetime gives each pod an end. The next pod starts with the current image, the current policy, and a clean container file system.

## 2. Requirements

| No. | Requirement |
|---|---|
| R1 | An administrator can set a maximum lifetime for all policies and for one policy. |
| R2 | When a pod is older than the maximum lifetime, warren stops it. Open requests and open WebSockets do not prevent the stop. |
| R3 | The volume of the sandbox stays. The next request starts a new pod. |
| R4 | The age of a pod does not depend on a restart of warren. |
| R5 | A change of the value in the policy file applies immediately, also to the pods that operate. This is the rule of the idle time (`policies.md`, section 11). |
| R6 | The default is "no maximum lifetime". An installation that does not set the value has no change in behaviour. |

## 3. Design

### 3.1 Configuration

| Place | Name | Default | Notes |
|---|---|---|---|
| Flag | `--max-lifetime` | `0` | For a policy that sets no value. `0` means no maximum lifetime. |
| Variable | `WARREN_MAX_LIFETIME` | | The same as the flag. |
| Policy key | `maxLifetime` | The flag | A time, as `idleTimeout`. `0` means no maximum lifetime for this policy. |
| `defaults` of the policy file | `maxLifetime` | | A policy uses it when it sets no value. |

Rules for the value:

- The form is the form of all times in the file (`policies.md`, section 4): a number with the unit `s`, `m`, `h`, or `d`.
- The minimum is `1m`. A smaller value that is not `0` is an error. The reaper runs each 30 seconds, and a pod must have time to start.
- A negative value is an error.

### 3.2 Age of a pod

The age is the time since `metadata.creationTimestamp` of the pod. Kubernetes keeps this time, thus R4 is true with no new annotation.

A restart of the container in the pod does not change the age.
A restart operation, an idle stop, and each other stop make a new pod. The age then starts again.

### 3.3 Stop

Add one case to the `switch` in `Reap` (`internal/kube/reaper.go`), after the case `not_ready` and before the case `idle`:

- Condition: the maximum lifetime of the pod is more than `0`, and the age is more than the maximum lifetime.
- Action: `deletePod` with the reason `max_lifetime` and `idleOnly` = `false`.

Add the function `maxLifetime(pod)`. It reads the value from the policy of this time, as `idleTimeout(pod)` does.
If the policy is not in the file, the case `no_policy` stops the pod first. Thus the function needs no annotation on the pod.

The stop can be a maximum of 30 seconds late. That is the time between two runs of the reaper.

### 3.4 What a user sees

- A process in the sandbox gets the signal of Kubernetes. The grace period of the pod is 10 seconds (`buildPod`).
- An open WebSocket closes when the pod goes away. The browser client of Open WebUI tries again after the code 1011 (`protocol.md`, section 4.2.3). The new connection starts a new pod.
- A shell session and a process of the old pod do not continue. Files in the home directory stay, if the policy has persistent storage. Files in `/tmp` and files of a policy with ephemeral storage are lost.
- An HTTP request that is in progress on the old pod gets the status 502.

The documentation must give this warning: with ephemeral storage, the maximum lifetime deletes the files of the user during use.

### 3.5 Interaction with other functions

| Function | Interaction |
|---|---|
| Idle time | Independent. The condition that is true first stops the pod. |
| Scheduled reset (specification 05) | Independent. A reset deletes files. The maximum lifetime keeps the volume. |
| Instance list | The sandbox shows as stopped until the next request, if it has a volume. |
| Limits for sandboxes | A stopped pod does not count. |
| A pod of the test of the network plugin | It does not have the label `managed-by` of a sandbox. The reaper does not see it. |

### 3.6 Management API

The template data of Open WebUI has no field for a lifetime (`protocol.md`, section 4.6). Warren does not show the value to Open WebUI.

## 4. Changes

| File | Change |
|---|---|
| `internal/policy/policy.go` | `Spec.MaxLifetime *time.Duration`. Copy it in `clone` and `overlay`. Examine it in `Validate`. |
| `internal/policy/decode.go` | The key `maxLifetime` in `specFields`. |
| `cmd/warren/main.go` | The flag, the variable, and the field of `kube.Config`. Include it in the check for negative values. |
| `internal/kube/manager.go` | `Config.MaxLifetime`. |
| `internal/kube/reaper.go` | The function `maxLifetime` and the new case. |
| `deploy/helm/warren` (specification 02) | The value `limits.maxLifetime`. |
| `docs/policies.md` | Section 4: the key. Section 11: one row in the table of changes. Section 13: the reason `max_lifetime`. |
| `docs/feature-parity.md`, section 7 | Change the plan value of the row to "v1". |

## 5. Tests

All unit tests use the simulated clock of the package `internal/kube`.

| Test | What it shows |
|---|---|
| A pod that is older than the value goes away with the reason `max_lifetime`. Its volume stays. | R2, R3 |
| A pod with an open lease goes away. The lease owner gets the end of the connection. | R2 |
| A pod that is younger than the value stays. | R2 |
| With the value `0` in the flag and no key in the policy, an old pod stays. | R6 |
| The policy value `0` turns the function off when the flag has a value. | R1 |
| A new manager with a new start time stops an old pod at its first run. | R4 |
| A change of the policy from no value to `1h` stops a pod that is 2 hours old at the next run. | R5 |
| `maxLifetime: 30s` and `maxLifetime: -1h` are errors that give the line. | Section 3.1 |
| The metric `warren_sandbox_reaps_total{reason="max_lifetime"}` increases. | Section 3.3 |
| Cluster check: a policy with `maxLifetime: 2m`. A terminal WebSocket closes after 2 to 3 minutes. A new request starts a new pod with the file of the first pod. | R2, R3 on a cluster |

## 6. Open items

| No. | Item |
|---|---|
| O1 | Decision of the owner: warren can close the WebSockets of the pod with the code 1012 and a clear text before it deletes the pod. This needs a path from the manager to the proxy. The proposal is to not do this in the first version. |
| O2 | Decision of the owner: a warning in the log when the maximum lifetime of a policy is less than its idle time. |

## 7. Acceptance

1. The tests of section 5 pass.
2. An installation with no new configuration has the same behaviour as before.
