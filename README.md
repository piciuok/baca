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
