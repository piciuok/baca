# Security policy

## Reporting a vulnerability

Please report security issues privately, through
[GitHub's private vulnerability reporting](https://github.com/piciuok/baca/security/advisories/new),
not in a public issue. Include what you found, how to reproduce it, and which version (`baca version`)
and platform you use. You will get an answer within a week.

## What counts

baca keeps your worklog, records what is on your screen and holds Jira, Tempo, GitLab, GitHub and
Slack tokens, so these matter most:

- reading the worklog, the activity data (window titles) or the settings through the local server
  (127.0.0.1) without the UI token, from another user, process or web page;
- getting a token or a private calendar link out of `config.json` other than by reading the file;
- making baca send or change worklogs in Tempo without your click;
- tampering with updates, i.e. getting a binary other than the release's onto someone's machine.

Known limits:

- anyone who can read your files (your user account) can read `config.json`, the token and the data;
- the activity probes run local tools (`osascript`, `lsappinfo`, `xdotool`, `glab`, `gh`) from `PATH`.

## Supported versions

Only the latest release gets fixes. baca updates itself; see [Releases and updates](README.md#releases-and-updates).
