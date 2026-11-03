# Epics

Status: planned, not started.

## Why

Epics assigned to you already show up on the board, but as ordinary rows: taskboard never reads a ticket's issue type or parent. Nothing says a row is an epic, and an epic's child tickets are scattered across the stage sections with no link back to it. Marking epics and grouping their children makes it possible to follow an epic as a whole, including the parts other people are working on.

**A separate Epics section, not grouping inside the stage sections.** The stage sections are the urgency view: "Needs you" must show everything that needs you. If children lived only inside a collapsible epic group, collapsing it could hide a failing CI run or a plan waiting for review. So the stage sections keep every task you own, and the Epics section is an additional view.

## Design

**What acli gives us** (checked against the live site):
- `workitem search --fields ...,issuetype` returns `issuetype.name` and `issuetype.hierarchyLevel`. An epic has level 1.
- `parent` is not allowed in search fields. `workitem view --fields parent` returns `parent.key` and `parent.fields.{summary,status,issuetype}`.
- `search --jql "parent = MM-68189"` lists an epic's children.

**Where things appear.**
- An epic with no work of its own (`StageTicket`) moves from Not started into the Epics section.
- An epic with its own plan, PR, worktree or session also stays in its stage section, like any task.
- Done epics stay in Done and get no group.
- Children you own stay in their stage sections as full rows with actions. In the Epics section they appear as compact rows (key, summary, stage pill, no buttons). Clicking one jumps to its full row, so each task has only one place with actions.
- Children assigned to others appear only in the Epics section, read-only, with their Jira status and assignee.
- A stage-section row whose parent is an epic gets an `epic KEY` link (titled with the epic's summary). It points to the epic's group when that exists, otherwise to the epic in Jira.
- Every epic row carries an `Epic` badge after its key.

**Collapsing.** Each epic group is a `<details>` that starts open. Collapsed keys are stored in `localStorage["taskboard.epicsClosed"]`, following the `taskboard.doneOpen` pattern, so collapsing is remembered per browser.

**Naming.** `Task` gets generic fields (`Epic`, `Parent`, `ParentSummary`), so another tracker can fill them later. Only `jira.go` knows how Jira expresses them.

## Changes

**`jira.go`**
- `JiraIssue`: add `Epic bool`, `Parent`, `ParentSummary` and `Assignee` (display name). `Parent` is set only when the parent's `hierarchyLevel` is 1, so a subtask's story parent is ignored.
- `jiraIssueJSON`: decode `issuetype {name, hierarchyLevel}`, `parent {key, fields{summary, issuetype}}` and `assignee {displayName}`.
- `jiraSearch` adds `issuetype` to `--fields`. `jiraGet` adds `issuetype,parent`.
- New `jiraChildren(ctx, epic)`: `search --jql "parent = KEY ORDER BY updated DESC" --fields key,summary,status,issuetype,assignee --paginate --json`, decoded with `decodeIssues`.

**`board.go`**
- Replace `jiraUpdated map[string]time.Time` with a cached detail (`Updated`, `Parent`, `ParentSummary`), filled by the `view` that `fillJiraUpdated` already runs. Re-fetching is unchanged: moving a ticket to another epic changes its `updated`, so the existing `updated >= -Nm` check picks it up. Issues that came from `jiraGet` (the `!is.Updated.IsZero()` branch) store their parent too.
- Once the issue set is complete, fetch the children of each open epic concurrently (a semaphore of 8, as in `fillJiraUpdated`) into a new `fetched.children map[string][]JiraIssue`, never modified once stored. Errors go to `Snapshot.Errors` as `Jira children of KEY: ...`.
- This costs one Jira search per open epic per refresh. If that turns out heavy, cache children behind a changed-check later.
- `Snapshot` gains `Epics []EpicGroup`, built wherever `buildTasks` is called (refresh and `LocalChanged`) and included in `snapshotVersion`.

**`tasks.go`** (pure, tested)
- `buildTasks` copies `Epic`, `Parent` and `ParentSummary` into `Task`.
- `buildEpics(tasks, children)` returns one `EpicGroup` per open epic task, holding `EpicChild{Key, Summary, JiraURL, Status, Done, Assignee, OnBoard}` plus stage label and tone for on-board children. Order: on-board children by stage, then open off-board children, then done ones (dimmed). Each group carries open and done counts.
- The decision of which stage group a task goes in moves into a small pure function, with a new `groupEpics` between Not started and Done in `groupOrder` and the nav.

**`templates/index.html`**
- Epic badge, the Epics section, compact child rows, the `epic KEY` link on stage rows, and the collapse state script.
- The epic row inside its group keeps the highlight button, `data-id` and `taskControls`. Child rows get no `.task` class or `data-id`, so highlight navigation doesn't count them twice.
- On-board child keys reuse the existing jump-and-scroll code.
- CSS uses the existing tokens.

**Docs.** A README paragraph on epics, covering the badge, the section, read-only children, the extra Jira search per open epic and the remembered collapse state. A line in CLAUDE.md's data flow about `fetched.children` and `buildEpics`.

## Verification

- Tests in `taskboard_test.go`:
  - Decoding `issuetype`, `parent` and `assignee`, where a non-epic parent is ignored.
  - `buildTasks` copying the new fields.
  - `buildEpics` ordering and counts, with on-board and off-board children.
  - Grouping that moves a `StageTicket` epic to Epics and keeps an epic with a PR in its stage.
- `go vet ./...`, `go test ./...`, gofmt on edited files.
- Against the real site:
  - MM-68189 (Cross Guard plugin for CDS) shows in Epics with MM-71014, MM-71020 and MM-71024 under it, and those rows' stage entries show `epic MM-68189`.
  - The other assigned epics (MM-70599, MM-67346, MM-60889) appear in Epics too.
  - A collapsed group stays collapsed after a reload.
  - A viewer (not local, no certificate) sees the section read-only.
