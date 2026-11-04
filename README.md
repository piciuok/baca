# baca

A bar of your working day that turns into a worklog in a plain text file as you go, with logging to Tempo.

```
go run . -open                     # http://127.0.0.1:7788
go test ./...
```

Configuration: `config.json` next to the binary (with `go run`, in the working directory). See `config.json.dist`: `worklogFile`, `accountEmail`, `tokens.jira`, `tokens.temp`, `jiraBaseUrl`, `tempoBaseUrl`. Without tokens the app works, only logging to Tempo is off.

| flag | env | default |
|---|---|---|
| `-config` | `BACA_CONFIG` | `config.json` in the project |
| `-file` | `BACA_FILE` | `worklogFile` from the config, else `~/.local/share/baca/worklog.txt` |
| `-addr` | `BACA_ADDR` | `127.0.0.1:7788` (localhost only) |

## File format

```
@Wtorek 03.11
*[07:00 - 07:10] - ABC-752 - CR #~123456~#
[07:10 - 08:40] - ABC-868 - Write the setup guide, check screenshots
[08:40 - 09:00] - ABC-868 - Continue task
```

- Newest day on top. Day headers use Polish weekday names (`@Wtorek 03.11`); the year is optional (`@Wtorek 03.11.2026`). Without it, the latest year that keeps the file in descending order is taken.
- `*` at the start and `#~<tempoWorklogId>~#` at the end mark an entry logged to Tempo. Such a block is never sent twice and cannot be dragged. “Edit in Tempo” changes its ticket and description (`PUT` in Tempo, then the file). “Delete from Tempo” deletes the worklog in Tempo and keeps the block as open, so you can fix the time and log it again.
- A day's status follows from its entries: logged (all with `*`), partial or open.
- An empty description is written as `Continue task`, a missing ticket as `???` (such an entry is never logged).
- Lines the parser does not understand (`#` comments, notes) stay in the file. A save changes only the edited day and moves that day's comments under its header.

## Saving and logging

- The UI saves the day about 400 ms after the last change (`PUT /api/days/{date}` with the day's hash). If the day changed in the file meanwhile, the server answers 409 and the UI loads the current version.
- “Log to Tempo” (`POST /api/days/{date}/log`) sends the open entries and marks each in the file right after Tempo accepts it. A worklog already in Tempo (same issue, start and length) is only marked, no duplicate is created.

## Activity (probes)

The server runs probes that record what happened during the day (`-collect=false` turns them off). Each probe runs in its own loop and writes spans to `activity/YYYY-MM-DD.jsonl` (a local file, mode 600). The “Activity” bar under the timeline shows the apps and suggested blocks. A click adds a suggestion into the free time, “Accept all suggestions” does it for the whole day.

| probe | what it records | how often |
|---|---|---|
| `system` | the app in front, its window title, idle time (macOS: `lsappinfo`, `osascript`, `ioreg`; Linux X11: `xdotool`, `xprintidle`, `wmctrl`). An IDE title naming a watched repo (JetBrains `repo – file`, VS Code `file — repo`) gets the repo and its branch. An open call window (Slack huddle, Zoom, Teams) is recorded as a meeting | 5 s, call windows every 30 s |
| `git` | repositories from `activity.git.repos` whose git state changed (index, HEAD, reflog), with the branch | 1 min |
| `claude` | Claude Code sessions that wrote in that time, mapped to a watched repo | 1 min |
| `codex` | Codex sessions (`~/.codex/sessions`), the session's working directory, the branch of its repo | 1 min |
| `gitlab` | your GitLab events (push, MR comment and approval, merge) of today and yesterday; the ticket comes from the MR title or the branch. Token from the config, `GITLAB_TOKEN` or `glab` | 5 min |
| `jira` | your changes to issues (status, fields) and comments of today and yesterday. Worklog fields are skipped: logging hours is not the work. Uses the Jira credentials from the config | 10 min |
| `github` | events of the user `activity.github.user` (push, PR reviews and comments, opened and merged PRs, issue comments) of today and yesterday. Token from the config, `GITHUB_TOKEN` or `gh auth token --user <user>` | 5 min |
| `calendar` | meetings from private iCal links (`activity.calendar.urls`: Google “Secret address in iCal format”, Outlook “Publish calendar” → ICS), recurring ones included. Skips all-day, cancelled and declined ones (`email`, default `accountEmail`) | 15 min |
| `slack` | messages you sent today and yesterday (`search.messages from:me`); a ticket in the text counts for that time. Needs a `xoxp-…` user token with `search:read`. Time in Slack and huddles come from the `system` probe anyway | 10 min |

- Remote probes (GitLab, GitHub, Jira, calendar, Slack) fetch the whole day every few minutes and replace their own `YYYY-MM-DD.<probe>.jsonl`, so fetching again never duplicates. An event is a point in time, so its span reaches back by the usual length of the action (push 10 min, comment and approval 5 min).
- Window titles on macOS need the Accessibility permission for the app baca was started from. Without it only the app name is recorded. Apps in `activity.system.ignoreApps` (password managers by default) never get their titles recorded.
- Idle time longer than `idleAfterSeconds` (180) reaches back to the last mouse or keyboard input.
- Meetings are found by a window title pattern per app (`activity.system.meetings`; defaults for Slack `huddle|rozmowa`, `zoom.us`, `Microsoft Teams`, Polish titles included). A step where a call takes more than half of the time becomes its own “📞 …” suggestion with the call title as the description, even without mouse input.
- Suggestions use 5-minute steps. A step is active with at least 2 minutes at the computer or activity in a repo or Claude. The ticket comes from window titles (seconds) or from the branch of an active repo (weight ×2). Only ticket prefixes already used in the worklog count. A step without evidence continues the previous ticket.

Configuration (the `activity` section of `config.json`, all optional):

```json
"activity": {
  "dir": "~/.local/share/baca/activity",
  "system": { "intervalSeconds": 5, "idleAfterSeconds": 180, "ignoreApps": ["1Password"] },
  "git": { "repos": ["~/Projects/*"] },
  "claude": { "enabled": true },
  "codex": { "enabled": true },
  "gitlab": { "instances": [{ "baseUrl": "https://gitlab.example.com", "token": "" }] },
  "jira": { "enabled": true },
  "github": { "user": "octocat" },
  "calendar": { "urls": ["https://calendar.example.com/private-…/basic.ics"] },
  "slack": { "token": "xoxp-…" }
}
```
