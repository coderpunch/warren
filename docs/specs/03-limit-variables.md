# Specification 03: variables for the CPU limit and the memory limit

Status: proposal. The owner of the project must approve it.

## 1. Purpose

`feature-parity.md`, section 11, has this row with the plan value "v1":

> The orchestrator adds variables that give the CPU and memory limits to the processes in the instance.

The code does not do this. `buildPod` in `internal/kube/objects.go` sets only the key variable, `HOME`, `OPEN_TERMINAL_ISOLATED`, `OPEN_TERMINAL_MULTI_USER`, and the variables of the policy.

A process in a container cannot see its limits easily. A system prompt or a tool can read the variables, and a model then does not start work that needs more memory than the sandbox has.

## 2. Requirements

| No. | Requirement |
|---|---|
| R1 | Each sandbox pod has one variable with its CPU limit and one variable with its memory limit. |
| R2 | The names and the value forms are those of the public documentation of the orchestrator. Refer to open item O1. |
| R3 | The values are the limits of the container, after the defaults of `policy.complete` apply. |
| R4 | A policy cannot set the two variables. |
| R5 | A change of the limits in the policy file applies to pods that start after the change, as for all resources (`policies.md`, section 11). |

## 3. Design

### 3.1 Names and values

Use the names that the public documentation gives. Record them in `feature-parity.md`, section 11, with the address of the page.

If the documentation gives no value form, use these forms:

| Variable | Form | Example for a limit of `500m` and `2Gi` |
|---|---|---|
| CPU | A decimal number of processors, as the template field `cpu_limit` (`internal/api/api.go`, `toTemplate`) | `0.5` |
| Memory | A whole number of bytes | `2147483648` |

Move the CPU format code of `toTemplate` to one function that the two packages use. The two values must never be different.

### 3.2 Pod

In `buildPod`, add the two variables after the fixed variables of warren and before the variables of the policy.
Use literal values from `pol.Resources.Limits`. The values have no `$` character, thus they need no escape.

### 3.3 Reserved names

Add the two names to these two lists:

- `reservedEnv` in `internal/policy/policy.go`, with the cause "warren sets it from the limits of the policy".
- `reservedVariables` in `internal/kube/objects.go`, as the second protection.

A policy file that sets one of the names is then not valid. This is a change for a file that sets them at this time.
Say so in the release notes.

### 3.4 Visibility in a shell

Open Terminal does not give the variables that start with `OPEN_TERMINAL_` to the shells of the user (`policies.md`, section 8).
If the names of the documentation start with that prefix, a shell does not see them. The server process sees them.
Record in `open-terminal-api.md` if Open Terminal 0.18.1 reads them. `feature-parity.md` says that it does not.

## 4. Changes

| File | Change |
|---|---|
| `internal/kube/objects.go` | The two variables in `buildPod`. The two names in `reservedVariables`. |
| `internal/policy/policy.go` | The two names in `reservedEnv`. |
| `internal/api/api.go` | Use the shared CPU format function. |
| `docs/policies.md`, section 8 | Two rows in the table of variables that a policy cannot set. One paragraph about the variables that warren sets. |
| `docs/feature-parity.md`, section 11 | The names and the source. |

## 5. Tests

| Test | Package | What it shows |
|---|---|---|
| A pod of a policy with `500m` and `512Mi` has the values `0.5` and `536870912`. | `internal/kube` | R1, R3 |
| A pod of a policy with no limits has the values of the default limits. | `internal/kube` | R3 |
| A policy file that sets one of the names is not valid, and the error gives the line. | `internal/policy` | R4 |
| The CPU value of the pod and the `cpu_limit` of the template are the same text for `700m`, `1`, and `2500m`. | `internal/api` | Section 3.1 |
| In the sandbox of the policy `tuned`, the server process has the two variables. | `test/cluster/lifecycle.py` | R1 on a cluster |

## 6. Open items

| No. | Item |
|---|---|
| O1 | The names and the value forms of the two variables. The review had no access to the public documentation. Read the page about the environment variables of the orchestrator. Do not use the README of the `open-webui/terminals` repository. |
| O2 | If the documentation gives no names, the owner selects them. Proposal: `WARREN_CPU_LIMIT` and `WARREN_MEMORY_LIMIT`. A shell of the user sees these names. |

## 7. Acceptance

1. The tests of section 5 pass.
2. `feature-parity.md` has the names with their source, or the decision of the owner for O2.
