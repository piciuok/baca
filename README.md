<p align="center"><img src="assets/baca.svg" alt="baca: a highlander hat inside a ring that fills up with the working day" width="160"></p>

<h1 align="center">baca</h1>

<p align="center">
<a href="https://github.com/piciuok/baca/actions/workflows/ci.yml"><img src="https://github.com/piciuok/baca/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
<a href="https://github.com/piciuok/baca/releases"><img src="https://img.shields.io/github/v/release/piciuok/baca?sort=semver" alt="Release"></a>
<a href="LICENSE"><img src="https://img.shields.io/github/license/piciuok/baca" alt="MIT license"></a>
</p>

> Counts your day the way a shepherd counts the flock, so the worklog is ready when the day ends.

**baca** (in the Polish highlands, the head shepherd who counts the sheep and settles the accounts; the
one [Juhas](https://github.com/piciuok/juhas) works for) is a small menu-bar (macOS) / system-tray (Linux)
app for people who jump between tasks all day. It records what you actually worked on: windows and IDE
projects, git branches, Claude Code and Codex sessions, GitLab and GitHub activity, Jira changes, meetings.
It suggests a block per ticket on a timeline of your day. You drag the blocks into shape, and the day is in
a plain-text worklog that you log to Tempo with one click.

- **Nothing to remember**: probes run in the background, and the day's ring next to the clock shows how far you are from the target.
- **A timeline you can drag**: 5-minute steps, edges push the neighbour, a second bar sums time per ticket.
- **Suggestions per ticket**: the ticket comes from window titles, the branch you were on, the MR you reviewed or the issue you moved. Every ticket gets its own block, because each one becomes its own worklog.
- **A plain-text log**: one readable file, written atomically, never a database to repair (see [why](#a-plain-text-worklog)).
- **Tempo in one click**: open blocks are sent and marked in the file. Logged ones can be edited or removed in Tempo from the UI.
- **Local and private**: the UI is served on 127.0.0.1 behind a token, and nothing leaves your machine except the calls to the services you configure and the update check.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/piciuok/baca/main/scripts/install.sh | sh
```

The script downloads the latest release and checks it against `checksums.txt`. It puts `baca` in
`~/.local/bin` (no sudo), enables start at login and starts it. On Linux it offers the tools the activity
probes use (`xdotool`, `xprintidle`, `wmctrl`, `notify-send`). Options: `BACA_VERSION=v0.1.0`, `BACA_YES=1`,
`BACA_NO_DEPS=1`, `BACA_NO_START=1`.

By hand: download `baca_<version>_<os>_<arch>.tar.gz` from [Releases](https://github.com/piciuok/baca/releases),
check it against `checksums.txt`, then run `./baca install && baca start`. From source (Go, and Xcode command
line tools on macOS): `make install`.

On macOS, window titles need the Accessibility permission for `~/.local/bin/baca` (System Settings → Privacy &
Security → Accessibility). Without it baca records app names only.

```
baca                 the icon and the UI server (from a terminal it starts in the background)
baca start           start it in the background if it is not running
baca open            open the UI in the browser
baca update          update to the latest release (--check only looks)
baca install         copy to ~/.local/bin and start at login (uninstall turns that off)
baca serve -open     the UI server without the icon (development)
```

## A plain-text worklog

```
@Tuesday 03.11
*[07:00 - 07:10] - ABC-752 - Code review #~123456~#
[07:10 - 08:40] - ABC-868 - Write the setup guide, check screenshots
[08:40 - 09:00] - ABC-868 - Continue task
```

The worklog is one text file (`worklogFile`, by default `~/.local/share/baca/worklog.txt`). You can read it at
any time, grep it, edit it in any editor, keep it in git, or paste a day straight into a report. Every save
writes a temporary file and renames it over the old one, so a crash or a power loss leaves either the old day
or the new one, never a half-written file. There is no SQLite file to lock or repair. Edits made in an editor
are picked up when you come back to the window. The activity data is append-only JSON lines, one file per
day.

- Newest day on top. A header is `@<weekday> DD.MM` with an optional year (`@Tuesday 03.11.2026`). Only the date is read, so the weekday can be in any language. New headers use the names from the settings (English by default, e.g. `@星期二 03.11` if you prefer).
- `*` at the start and `#~<tempoWorklogId>~#` at the end mark an entry logged to Tempo. Such an entry is never sent twice and its block cannot be dragged.
- An empty description is written as `Continue task`, a missing ticket as `???` (never logged).
- Lines the parser does not understand (`#` comments, notes) stay in the file. A save changes only the edited day.
- A long entry wrapped when copied from a terminal is joined back when the next line continues it.

## The day in the UI

- **Timeline** (top): drag on empty space to add a block, drag an edge to resize (touching neighbours share the edge, a gap fills first), drag the middle to move. Logged blocks are striped and locked.
- **Activity** (middle): the apps you used and suggested blocks. A click adds a suggestion into the free time, “Accept all suggestions” fills the day. Hover shows where the ticket came from, the apps and the window titles.
- **Totals** (bottom): time per ticket, the daily target as a red line, overtime past it.
- **Worklog**: the day as text, ready to copy, and “Log to Tempo”.

## Activity probes

Each probe runs in its own loop and writes spans to `activity/YYYY-MM-DD.jsonl` next to the worklog
(mode 600). Remote probes rebuild today and yesterday on every poll and replace their own
`YYYY-MM-DD.<probe>.jsonl`, so polling never duplicates anything.

| probe | what it records | how often |
|---|---|---|
| `system` | the app in front, its window title, idle time. An IDE title naming a watched repo (JetBrains `repo – file`, VS Code `file — repo`) gets that repo's branch. An open call window (Slack huddle, Zoom, Teams) is a meeting (macOS: `lsappinfo`, `osascript`, `ioreg`; Linux X11: `xdotool`, `xprintidle`, `wmctrl`) | 5 s, call windows 30 s |
| `git` | repositories from `activity.git.repos` whose git state changed (index, HEAD, reflog), with the branch | 1 min |
| `claude` | Claude Code sessions that wrote in that time, mapped to a watched repo | 1 min |
| `codex` | Codex sessions, by their working directory and its branch | 1 min |
| `gitlab` | your pushes, MR comments, approvals and merges; the ticket from the MR title or the branch. Token from the settings, `GITLAB_TOKEN` or `glab` | 5 min |
| `github` | the same for each configured GitHub account. Token from the settings, `GITHUB_TOKEN` or `gh auth token --user <login>` | 5 min |
| `jira` | your status and field changes and comments (worklog fields are skipped: logging hours is not the work) | 10 min |
| `calendar` | meetings from private iCal links (Google, Outlook), recurring ones included; all-day, cancelled and declined ones are skipped | 15 min |
| `slack` | messages you sent; a ticket in the text counts for that time. Needs a `xoxp-…` user token with `search:read` | 10 min |

How suggestions are made:

- The day is split into 5-minute steps. A step counts with at least 2 minutes at the computer, or activity in a repo, an agent session, a web service or a meeting.
- A step's ticket is the one with the most evidence: seconds of window titles naming it, double for a repo, agent or service event on its branch or about it. Only ticket prefixes already used in your worklog count, so `UTF-8` never becomes a ticket. A step without evidence continues the previous ticket.
- A step mostly in a call becomes its own “📞 …” suggestion with the call title as the description.
- Idle time longer than `idleAfterSeconds` (180) reaches back to your last input. Apps in `ignoreApps` (password managers by default) never get their titles recorded.

## Logging to Tempo

- “Log to Tempo” sends the open entries of the day and marks each in the file right after Tempo accepts it. A worklog already in Tempo (same issue, start and length) is only marked, so an interrupted run never logs twice.
- “Edit in Tempo” changes a logged entry's ticket or description, in Tempo first and then in the file. “Delete from Tempo” removes the worklog and turns the block back into an open one, so you can fix its time and log it again.
- “Worklog in Tempo ↗” opens the week in Tempo with that worklog in focus.

## Settings

Everything is in the UI under ⚙ (and in `~/.config/baca/config.json`, see `config.json.dist`): working hours
(timeline and daily target), weekday names, the worklog file, Jira and Tempo, GitLab instances, GitHub
accounts, calendars, Slack, the local probes and updates. Saving reloads the Tempo client and the probes
without a restart. Tokens and private links never go back to the browser: the UI shows `••••••`, and leaving
it as is keeps the stored value.

| flag | env | default |
|---|---|---|
| `-config` | `BACA_CONFIG` | `~/.config/baca/config.json` |
| `-file` | `BACA_FILE` | `worklogFile` from the settings, else `~/.local/share/baca/worklog.txt` |
| `-addr` | `BACA_ADDR` | `127.0.0.1:7788` (localhost only) |
| `-collect=false` | | turns the probes off |

## Security

- The server listens on localhost and answers only to a loopback `Host` (no DNS rebinding).
- Every request needs the token from `~/.local/state/baca/token` (mode 600): the page as `/?t=<token>`, API calls in the `X-Baca-Token` header. Other users and processes cannot read the worklog or the activity, and other web pages cannot call the API. The icon and `baca open` open the page with the token.
- The page is never framed and sends no `Referer`. `config.json` and the activity files are mode 600.
- Report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## Updates and releases

- baca checks for a new release every 6 h, tells you once and shows “⬆ Update” in the menu and the UI footer. Updating downloads the archive, checks it against `checksums.txt`, swaps the binary in place and restarts (under the LaunchAgent through `launchctl kickstart`). A development build never replaces itself (`baca update --force`).
- A pushed `vX.Y.Z` tag (`vX.Y.Z-rc.N` for a pre-release) runs the release workflow: tests, macOS (cgo) and Linux builds for arm64 and amd64, a release with `baca_<version>_<os>_<arch>.tar.gz` archives and `checksums.txt`. Release tags are immutable.
- Update status and tray logs are in `~/.local/state/baca`.

## Development

```sh
make run      # UI server without the icon, from the working tree
make test     # go test ./...
make vet      # also for Linux without cgo
make build    # bin/baca with the version from git describe
```

## License

[MIT](LICENSE)
