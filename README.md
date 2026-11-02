# SCLogSync

Windows client for **[SC Log Viewer](https://sclog.app)**, a site that turns Star Citizen's `Game.log` files into statistics:
money in and out, missions, blueprints, deaths, trade runs, travel and more.

SCLogSync uploads your finished game logs (`logbackups\`) and streams the running `Game.log` live while you
play, either in a console window or as a background Windows service. It never touches the game: it only reads
the log files the game already writes.

> Unofficial fan project. Not affiliated with Cloud Imperium Games or Roberts Space Industries.

## Download

Get `sclogsync.exe` from the [latest release](https://github.com/cihanenescolak/ScLogAgent/releases/latest).
Each release also has a `SHA256SUMS` file. To check your download in PowerShell:

```powershell
Get-FileHash .\sclogsync.exe -Algorithm SHA256
```

The hash must match the one in `SHA256SUMS`.

The exe is not code-signed yet, so Windows SmartScreen may show "Windows protected your PC" on the first start:
choose **More info → Run anyway**.

Requirements: Windows 10 or 11, 64-bit. No installer or runtime needed.

## First start

Double-click `sclogsync.exe`.

1. Press Enter to use [sclog.app](https://sclog.app), or type the address of another SC Log Viewer server (asked once).
2. A browser opens: sign in on the site and approve the device with the code shown in the console.
3. Pick the Star Citizen folder (detected on all drives and from the RSI Launcher settings), or type its path.
4. Finished logs in `logbackups` are uploaded; files the server already has are skipped.
5. Answer **Y** to "Install as a Windows service" (UAC prompt) to copy the program to
   `%ProgramFiles%\SCLogSync\` and run it in the background at every boot. Answer **N** to keep watching
   `Game.log` in the console until you press Ctrl+C.

If PTU / EPTU folders are found, the client asks whether to upload those logs too.

When the service is running, starting the program again does not start a second watcher; it shows a menu to
check the status, reconfigure or uninstall the service.

## Commands and flags

```text
sclogsync [command] [flags]

Commands:
  (none)           interactive setup, upload and live watching
  run              headless mode (used by the Windows service)
  status           show service state, server, account, folder, last upload
  start | stop     start / stop the Windows service
  uninstall        remove the Windows service (--purge also deletes settings)
  logout           delete the saved login token
  version          print the version

Flags:
  --server URL       server address (e.g. https://sclog.example.com)
  --folder PATH      Star Citizen folder (the one with Game.log / logbackups)
  --ptu-folder PATH  also upload this PTU / EPTU folder
  --no-ptu           do not upload PTU logs
  --lang CODE        console language (en, tr, de, fr, es, it, pt, pl, ru, zh, ja, ko)
  --once             upload backups and exit (no live watching)
  --verbose          more output
  --no-update        do not update automatically
```

The console language follows the Windows display language unless `--lang` is given.

Exit codes: `0` ok, `1` error, `2` bad arguments, `3` another instance is running, `4` not configured,
`5` updated (the service restarts with the new version), `130` cancelled.

## Automatic updates

On every start, and every 6 hours in service mode, the client asks the server which version is current. The
server takes it from this repository's latest release, together with the exe's download URL, size and SHA-256
(from `SHA256SUMS`). When a newer version is announced, the client:

1. downloads the exe from the release (only GitHub release assets or the server itself are accepted),
2. checks its size and SHA-256 and discards it if either doesn't match,
3. moves the running exe aside (`sclogsync.exe.old`, deleted on the next start) and puts the new one in place,
4. restarts: a console window continues with the new version; the service exits with code 5 and Windows
   restarts it (recovery action "restart").

If anything fails, the old exe stays. Turn updates off with `--no-update`.

## Privacy

Only filtered game lines are sent: shop and commodity transactions, contracts and rewards, blueprints,
travel, deaths, session info, plus the build, environment, session id and a short hardware summary
(CPU / GPU / RAM) from the log header. The filter patterns come from the server
(`GET /api/agent/config`) and are applied on your PC.

Never sent: your computer name, local and server IP addresses, Windows user name, audio devices, file paths
and every other log line.

The login token is stored DPAPI-encrypted (machine scope, so the service can read it) in
`%ProgramData%\SCLogSync\config.json`; the folder is restricted to SYSTEM, Administrators and the installing
user. You can revoke the device at any time in the site's settings.

## Files

| Path | Content |
|---|---|
| `%ProgramData%\SCLogSync\config.json` | server address, game folders, encrypted token |
| `%ProgramData%\SCLogSync\state.json` | local cache of files already sent (safe to delete; the server is the source of truth) |
| `%ProgramData%\SCLogSync\agent.log` | service log, rotated at ~5 MB |
| `%ProgramFiles%\SCLogSync\sclogsync.exe` | the installed copy when running as a service |

## Building

Requires Go (see `go.mod` for the version).

```sh
VERSION=1.0.0 ./build.sh                                   # → dist/sclogsync.exe and dist/SHA256SUMS
VERSION=1.0.0 SERVER=https://sclog.example.com ./build.sh  # also bakes in a default server address
```

Tests run on macOS and Linux too (the Windows-only parts are behind build tags):

```sh
go vet ./... && go test ./...
GOOS=windows GOARCH=amd64 go build ./...
```

On macOS / Linux (development only) the data folder is `$SCLOGSYNC_HOME` or the user config dir, and the
token is only base64-encoded in a 0600 file.

## Releasing

Push a version tag:

```sh
git tag v1.0.1
git push origin v1.0.1
```

The [release workflow](.github/workflows/release.yml) runs the tests, builds `sclogsync.exe` with that version
and publishes it with `SHA256SUMS` as a GitHub release. Servers pick it up within an hour, and clients update
themselves.

## Protocol

The client speaks the SC Log Viewer ingest protocol, version 1 (`internal/api`):

- `GET /api/agent/config`: prefilter patterns, current / minimum client version, update offer.
- Device login: `POST /api/agent/device`, then `POST /api/agent/token` until the user approves in the browser.
- Backups: `check` (50 files per call) → `begin` → `chunk` → `commit`. The payload hash is the SHA-256 (hex) of
  `"<lineNo>\t<text>\n"` for every kept line. `409 in_progress` is retried on the next pass, `409 reset` drops the
  local cache and starts over with the new data epoch, `410` restarts the file, `422` marks the file failed until
  it changes, `429 weekly_limit` pauses backups until the reported reset time.
- Live: `Game.log` is polled every 2 s and new kept lines are posted to `/api/ingest/live` every ~5 s.
  `409 resync` resends the file from line 1, `409 complete` stops live for that session.
- Heartbeat every 60 s.

Kept lines are cut to 16384 UTF-16 code units before hashing; request bodies are capped at ~3 MB.

## License

[MIT](LICENSE)
