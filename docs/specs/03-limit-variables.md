# Specification 03: variables for the CPU limit and the memory limit

Status: proposal. The owner of the project must approve it.

## 1. Purpose

`feature-parity.md`, section 11, had this row with the plan value "v1" before the review:

> The orchestrator adds variables that give the CPU and memory limits to the processes in the instance.

The code does not do this. `buildPod` in `internal/kube/objects.go` sets only the key variable, `HOME`, `OPEN_TERMINAL_ISOLATED`, `OPEN_TERMINAL_MULTI_USER`, and the variables of the policy.

A process in a container cannot see its limits easily. A system prompt or a tool can read the variables, and a model then does not start work that needs more memory than the sandbox has.

## 2. Requirements

| No. | Requirement |
|---|---|
| R1 | Each sandbox pod has two variables with its CPU limit and two variables with its memory limit. |
| R2 | The names are those of the public documentation of the orchestrator (section 3.1). |
| R3 | The values are the limits of the container, after the defaults of `policy.complete` apply. |
| R4 | A policy cannot set the two variables. |
| R5 | A change of the limits in the policy file applies to pods that start after the change, as for all resources (`policies.md`, section 11). |

## 3. Design

### 3.1 Names and values

Source: `docs.openwebui.com/features/open-terminal/terminals/orchestration/environment-variables`, section "Automatically Injected Resource Vars", read on 2026-11-28.

The documentation gives four names and these descriptions:

| Variable | Description in the documentation |
|---|---|
| `OPEN_TERMINAL_CPU_LIMIT` | CPU limit from the CPU field of the terminal |
| `OPEN_TERMINAL_CPU_COUNT` | CPU count that comes from the CPU field |
| `OPEN_TERMINAL_MEMORY_LIMIT` | Memory limit from the Memory field of the terminal |
| `OPEN_TERMINAL_MEMORY_BYTES` | Memory limit in bytes |

The documentation says that the orchestrator sets a variable only for a field that has a value.
A warren sandbox always has a CPU limit and a memory limit (`policies.md`, section 6). Thus warren always sets the four variables.

The documentation does not give the exact form of the values. Warren uses these forms:

| Variable | Form | Example for a limit of `500m` and `2Gi` |
|---|---|---|
| `OPEN_TERMINAL_CPU_LIMIT` | A decimal number of processors, the same text as the template field `cpu_limit` (`internal/api/api.go`, `toTemplate`) | `0.5` |
| `OPEN_TERMINAL_CPU_COUNT` | A whole number: the CPU limit, rounded up. The minimum is 1. | `1` |
| `OPEN_TERMINAL_MEMORY_LIMIT` | A size, the same text as the template field `memory_limit` | `2Gi` |
| `OPEN_TERMINAL_MEMORY_BYTES` | A whole number of bytes | `2147483648` |

The count rounds up, because a program that starts one process for each processor must start a minimum of one.

Move the CPU format code of `toTemplate` to one function that the two packages use. The values of the pod and of the template must never be different.

### 3.2 Pod

In `buildPod`, add the four variables after the fixed variables of warren and before the variables of the policy.
Use literal values from `pol.Resources.Limits`. The values have no `$` character, thus they need no escape.

### 3.3 Reserved names

Add the four names to these two lists:

- `reservedEnv` in `internal/policy/policy.go`, with the cause "warren sets it from the limits of the policy".
- `reservedVariables` in `internal/kube/objects.go`, as the second protection.

A policy file that sets one of the names is then not valid. This is a change for a file that sets them at this time.
Say so in the release notes.

### 3.4 Visibility in a shell

Open Terminal does not give the variables that start with `OPEN_TERMINAL_` to the shells of the user (`policies.md`, section 8).
The four names start with that prefix. Thus a shell of the user does not see them. The server process sees them.
`feature-parity.md`, section 11, says that Open Terminal 0.18.1 does not read these variables. Make sure of this before the release, and record the result in `open-terminal-api.md`.

## 4. Changes

| File | Change |
|---|---|
| `internal/kube/objects.go` | The four variables in `buildPod`. The four names in `reservedVariables`. |
| `internal/policy/policy.go` | The four names in `reservedEnv`. |
| `internal/api/api.go` | Use the shared CPU format function. |
| `docs/policies.md`, section 8 | One row in the table of variables that a policy cannot set. One paragraph about the variables that warren sets. |
| `docs/feature-parity.md`, section 11 | The row has the names and the source. Remove the words "Not in the code at this time" from the note of the row. |

## 5. Tests

| Test | Package | What it shows |
|---|---|---|
| A pod of a policy with `500m` and `512Mi` has the values `0.5`, `1`, `512Mi`, and `536870912`. | `internal/kube` | R1, R3 |
| The count is `1` for `100m`, `2` for `1500m`, and `4` for `4`. | `internal/kube` | Section 3.1 |
| A pod of a policy with no limits has the values of the default limits. | `internal/kube` | R3 |
| A policy file that sets one of the names is not valid, and the error gives the line. | `internal/policy` | R4 |
| The CPU value of the pod and the `cpu_limit` of the template are the same text for `700m`, `1`, and `2500m`. | `internal/api` | Section 3.1 |
| In the sandbox of the policy `tuned`, the server process has the four variables. | `test/cluster/lifecycle.py` | R1 on a cluster |

## 6. Open items

| No. | Item |
|---|---|
| O1 | Answered on 2026-11-28: the public documentation gives the four names of section 3.1. |
| O2 | The documentation does not give the exact value forms. Section 3.1 has the forms of warren. Change them if a later version of the documentation defines them. |

## 7. Acceptance

1. The tests of section 5 pass.
2. The note of the row in `feature-parity.md`, section 11, does not say that the code does not have the function.
