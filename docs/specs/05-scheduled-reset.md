# Specification 05: scheduled reset

Status: proposal. The owner of the project must approve it.

## 1. Purpose

`feature-parity.md`, section 7, has this row with the plan value "Later":

> Scheduled reset. The orchestrator clears the files at a set time, each week, each month, or on a cron schedule. An administrator sets it in the orchestrator interface.

The public documentation gives these facts (`docs.openwebui.com/features/open-terminal/terminals/orchestration/`, section "Idle Cleanup, Lifetime and Scheduled Resets", read on 2026-11-28):

- A reset schedule clears the files of a terminal and refreshes its instances, also the instances that are in use.
- The schedule is a date and time for one reset, `@weekly`, `@monthly`, or a cron expression with five fields. A time zone is optional.
- An administrator sets the schedule in the template editor of the administrator interface of the orchestrator.

Warren can clear the files of a sandbox only when a client sends the reset or the delete operation.
This work lets an administrator give a schedule to a policy. At each time of the schedule, warren clears the files of all sandboxes of that policy.

## 2. Requirements

| No. | Requirement |
|---|---|
| R1 | A policy can have a schedule: one date and time, each week, each month, or a cron expression, with a time zone. |
| R2 | At a time of the schedule, warren stops each sandbox of the policy and deletes its files. Open requests and open WebSockets do not prevent this. |
| R3 | The sandbox stays in the instance list as a stopped instance. The next request of a user starts it again with an empty home directory. |
| R4 | A schedule never deletes files that are newer than its last time. |
| R5 | When an administrator adds or changes a schedule, no file goes away before the first time of the new schedule that comes after the change. |
| R6 | If warren does not operate at a time of the schedule, it does the reset after its start. |
| R7 | The state is in Kubernetes objects. A restart of warren loses nothing. |
| R8 | The delete delay of `--volume-delete-delay` applies: after a start of warren and after a change of the policy file, warren does no scheduled reset for that time. |
| R9 | Warren never does a scheduled reset for a volume that has no policy, or whose policy has a different scope (`policies.md`, section 11.1). |

## 3. Configuration

```yaml
policies:
  - id: classroom
    name: Classroom sandbox
    scope: user
    reset:
      schedule: "0 4 * * 1"
      timeZone: Europe/Berlin
```

| Key | Necessary | Default | Function |
|---|---|---|---|
| `reset.schedule` | Yes, if `reset` is there | | `@weekly`, `@monthly`, a cron expression with 5 fields, or one date and time. |
| `reset.timeZone` | No | `UTC` | A name of the IANA time zone database. |

The key `reset` is also possible in `defaults`. A policy that sets `reset` replaces the two values of `defaults`.
A policy can turn off the schedule of `defaults` with `reset: {schedule: none}`.

### 3.1 Schedule values

| Value | Meaning |
|---|---|
| `@weekly` | Sunday at 00:00. The same as `0 0 * * 0`. |
| `@monthly` | Day 1 of the month at 00:00. The same as `0 0 1 * *`. |
| Cron expression | Five fields: minute, hour, day of the month, month, day of the week. |
| One date and time | One reset at that time, for example `2027-01-04T04:00`. The form is ISO 8601, with seconds or with no seconds. |
| `none` | No schedule. |

The documentation does not give the day and the time of `@weekly` and `@monthly`. Warren uses the usual meaning of these words in cron.

Rules for one date and time:

- A value with no offset is a local time in the time zone of `reset.timeZone`. A value with an offset or with `Z` is an exact time, and `reset.timeZone` has no effect.
- A time that is in the past when warren reads it the first time causes no reset. Rule 2 of section 4.2 gives this result.

Rules for a cron expression:

- A field is `*`, a number, a list with `,`, a range with `-`, or a step with `/`. Names of months and days are not permitted. The only permitted words that start with `@` are `@weekly` and `@monthly`.
- The day of the week is a number from 0 to 7. The numbers 0 and 7 are Sunday.
- The minute field must be one number. Thus a schedule has a maximum of one time in each hour.
- If the day of the month and the day of the week are not `*`, a day agrees when one of the two fields agrees. This is the usual cron rule.
- A schedule that has no time in 5 years is an error, for example day 30 of month 2.
- The value must be a YAML string in quotation marks.

Time zone rules:

- An unknown name is an error.
- The binary contains the time zone database (the package `time/tzdata`). The result does not depend on the image.
- A local time that does not exist on a day, because the clocks go forward, is not a time of the schedule on that day.
- A local time that occurs two times on a day, because the clocks go back, is one time of the schedule: the first one.

## 4. Design

### 4.1 Package `internal/schedule`

A new package with no dependency outside the standard library. Rule 5 of `CONTRIBUTING.md`, section 6, applies: the project adds no cron library.

```go
// Parse reads a schedule value and a time zone name.
func Parse(value, zone string) (*Schedule, error)

// Prev returns the last time of the schedule that is not after t.
func (s *Schedule) Prev(t time.Time) time.Time

// String returns the text for the annotation: "<fields> <zone>".
func (s *Schedule) String() string
```

`Prev` looks back a maximum of 5 years. `Parse` refuses a cron expression for which `Prev` finds no time.
For one date and time, `Prev` returns that time if it is not after `t`. If it is after `t`, `Prev` returns the zero time, and no object is due.

### 4.2 The mark of a sandbox

A scheduled reset must not use the age of the files only: that would break R5.
Thus each object that a schedule can reset gets a mark. The mark records from which time the schedule applies to the object.

Annotation: `warren.coderpunch.io/reset-schedule`. Value: the text of `Schedule.String`, the character `|`, and a time in RFC 3339 form.

| Policy storage | Object that has the mark |
|---|---|
| Persistent | The volume (PersistentVolumeClaim) |
| Ephemeral | The pod |

Rules. The reaper applies them at each run, for each volume and for each pod of a policy with ephemeral storage:

1. The policy has no schedule: remove the mark, if the object has one.
2. The object has no mark, or the schedule text of the mark is different from the schedule of the policy: write the mark with the time of this run. Do nothing more for the object in this run.
3. The object has a correct mark with the time `M`. Calculate `D = Prev(now)`. The object is due when `D` is after `M`.

A new volume and a new pod get their mark a maximum of 30 seconds after warren makes them. Thus `M` is not before the time at which the object got its first file.
After a reset, the old object is gone. The new volume, or the next pod, gets a new mark with a time after `D`. Thus one time of the schedule gives one reset (R4).
After a change of the schedule, rule 2 writes a new mark. Thus R5 is true.
After a time with no warren process, `D` is after `M` for each object that missed a reset. Thus R6 is true.

A second warren process can write the same mark at the same time, for example in a rollout. The two processes write the same schedule text, and the difference of the two times is small. The patch has the resource version of the object, as `markVolume` does, and a conflict is not an error.

### 4.3 What a reset does

For an object that is due, warren does the reset operation of the management API (`Manager.Reset`, which is `clear` with `keep` = `true`):

- It stops the pod, if there is one.
- It deletes the volume, if there is one, and makes a new, empty volume with the settings of the policy.
- The sandbox stays in the instance list as a stopped instance, and it stays on the account of the same user (`policies.md`, section 10).
- Warren does not start the sandbox again. The next request of a user does that.

A sandbox of a policy with ephemeral storage has no volume. Warren stops its pod, and nothing stays.

If warren cannot make the new volume, the result is that of the delete operation, as `policies.md`, section 10, describes. Warren writes an error to its log.

The retention time must continue to count for a sandbox that no person uses. The new volume has no annotation `unused-since`, and without a rule its retention time would start again at each reset.
Thus, when the sandbox had no pod at the time of the reset, the new volume gets the `unused-since` value of the old volume.
`emptyVolume` gets a parameter for this value. The reset operation of the management API does not use it.

An installation with a schedule and no retention time keeps one empty volume for each sandbox that a user started one time. The documentation must say this, and must recommend a retention time together with a schedule.

The reaper does not do the reset in its own run. One delete can wait 30 seconds for the end of a pod.

- The reaper puts each due sandbox into a queue. A sandbox is in the queue one time only.
- A maximum of 4 resets operate at the same time.
- A sandbox with a start, a reset, or a volume change in progress is not due in this run. The conditions are those of `reapVolumes`: `starting`, `creating`, `resetting`, and `volumeBusy`.
- `clear` sets the mark `resetting` of the sandbox. No request can start the sandbox or get a lease in that time.
- The delete request for the volume and for the pod has the UID of the object that the reaper examined.

### 4.4 The delete delay

`reapVolumes` deletes no volume for the time of `--volume-delete-delay` after the start of warren and after a change of the policy file.
The scheduled reset uses the same rule and the same function `policiesSince`.

In that time, the reaper writes one log record with the number of sandboxes that are due, for each policy.
An administrator who sees a wrong number can correct the file before a file goes away.

### 4.5 Orphans

The reaper does not examine a volume or a pod whose policy is not in the file, or whose label `scope` is different from the scope of the policy (R9).
It also does not remove their mark. If the policy comes back with the same schedule, the old mark applies again. This agrees with R6.

### 4.6 What a user sees

The effects are those of `04-maximum-lifetime.md`, section 3.4, and the files of the home directory are gone also for a policy with persistent storage.
The name of the policy in Open WebUI must tell the users about the schedule. The template data has no field for it (`protocol.md`, section 4.6).

### 4.7 Records

| Record | Content |
|---|---|
| Log record `sandbox deleted` (audit) | The reason `scheduled_reset` |
| Log record `volume deleted` (audit) | The reason `schedule`, and the time `D` of the schedule |
| Log record `volume created` (audit) | The reason `scheduled_reset` |
| `warren_sandbox_reaps_total{reason="scheduled_reset"}` | Pods that a schedule stopped |
| `warren_volume_deletes_total{reason="schedule"}` | Volumes that a schedule deleted |

`clear` writes the reason `api` at this time. Give `clear` a parameter for the reason.

## 5. Changes

| File | Change |
|---|---|
| `internal/schedule/` | The new package and its tests. |
| `internal/policy/policy.go` | `Spec.Reset *schedule.Schedule`. Copy it in `clone` and `overlay`. |
| `internal/policy/decode.go` | The key `reset` with `schedule` and `timeZone`. |
| `internal/kube/objects.go` | The constant `AnnotationResetSchedule`. |
| `internal/kube/reaper.go` | The function `reapSchedules`, which `Reap` calls after `reapVolumes`. The queue and its workers. |
| `internal/kube/manager.go` | A reason parameter for `clear`. A parameter of `emptyVolume` for the `unused-since` value. The workers stop with the context of `Run`. |
| `internal/metrics/metrics.go` | No change. The two metrics have a `reason` label. |
| `docs/policies.md` | A new section "Scheduled reset". Section 4: the key. Section 11: one row. Section 13: the two reasons. |
| `docs/feature-parity.md`, section 7 | Change the plan value of the row to "v1". Note the difference: the schedule is in the policy file, and not in an interface. |
| `docs/threat-model.md`, section 4.5 | A wrong schedule deletes files. The protection is the delete delay and the log record. |

The service account of warren needs no new permission. It can already change pods and volumes with `patch`.

## 6. Tests

### 6.1 Package `internal/schedule`

| Test | What it shows |
|---|---|
| A table of expressions and times with the expected result of `Prev` | The fields, the lists, the ranges, and the steps |
| `@weekly` and `@monthly` give the times of section 3.1 | The two words |
| One date and time, with an offset and with a time zone: `Prev` before and after that time | The form for one reset |
| The last day of a month, 29 February, and a change of the year | The calendar |
| The two days of a year on which the clocks change, in `Europe/Berlin` and `America/New_York` | The time zone rules |
| Day of the month and day of the week together | The cron rule |
| Each error of section 3.1 gives a clear message | The checks |
| A fuzz test: `Parse` does not panic, and `Prev(t)` is never after `t` | Hostile input in the policy file |

### 6.2 Package `internal/kube`

All tests use the simulated clock.

| Test | What it shows |
|---|---|
| A volume with no mark gets a mark and stays. | Rule 2 |
| After the next time of the schedule, the pod goes away, and the sandbox has a new, empty volume with a different UID and a new mark. The instance list shows the sandbox as stopped. | R2, R3 |
| The new volume stays until the next time of the schedule. | R4 |
| A sandbox with no pod: the new volume has the `unused-since` value of the old volume. With a retention time of 10 days and a schedule for each week, warren deletes the volume after 10 days. | Section 4.3 |
| Kubernetes refuses the new volume: the sandbox is not in the instance list, and the log has an error. | Section 4.3 |
| A schedule that an administrator adds does not delete a volume that is 1 year old before the first time of the schedule. | R5 |
| A change of the schedule text writes a new mark. | R5 |
| A new manager finds a mark from before a time of the schedule, and does the reset after the delete delay. | R6, R7, R8 |
| In the delete delay, no volume goes away, and the log record has the number. | R8 |
| A pod with an open lease goes away. | R2 |
| A pod of a policy with ephemeral storage goes away, and its Secret and its network policy go away. | The ephemeral case |
| A volume of a policy that is not in the file stays, and keeps its mark. | R9 |
| A sandbox with a start in progress is not reset in this run, and is reset in a subsequent run. | Section 4.3 |
| 20 due sandboxes: a maximum of 4 resets operate at the same time, and all 20 complete. | The queue |

### 6.3 Cluster check

Add to `test/cluster/storage.py`: a policy with a schedule for each hour at the next minute that the script calculates.
The file of a user is there before that time and is gone after it. A file that the user writes after that time stays.

## 7. Open items

| No. | Item |
|---|---|
| O1 | Decided on 2026-11-28: a scheduled reset is the reset operation. Each sandbox keeps an empty volume and stays in the instance list (section 4.3). |
| O2 | Decision of the owner: the limit of one time in each hour (section 3.1). |
| O3 | Answered on 2026-11-28: the public documentation gives the forms `@weekly`, `@monthly`, a cron expression, and one date and time. It does not give the day and the time of the two words. Section 3.1 uses their usual meaning in cron. |
| O4 | Decision of the owner: after a long time with no warren process, R6 does a reset that can be many days late. The alternative is a maximum delay, after which warren waits for the next time of the schedule. |

## 8. Acceptance

1. The tests of section 6 pass.
2. A policy file with no `reset` key gives the same behaviour as before, and warren writes no new annotation.
3. The cluster check passes two times in sequence on the same cluster. This shows that a mark of a previous run does no damage.
