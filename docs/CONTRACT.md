# The GameOps contract

This document is the interface between the **toolkit** (`ghcr.io/reclyptor/gameops`) and a
**game image** that builds on it. If you are adding a new game, this is the whole spec: implement
the adapter functions below, meet the base requirements, and the toolkit gives you backups, updates,
notifications, player events, a console pipe, a health check and a supervised lifecycle for free.

Everything here is versioned with the toolkit image. A game pins the toolkit
(`COPY --from=ghcr.io/reclyptor/gameops:1.0.0 /opt/gameops /opt/gameops`), so a contract change is
a toolkit release, never a surprise.

---

## 1. What the toolkit provides

Installed at `/opt/gameops`: one static binary and a small bash shim. Game images add
`/opt/gameops/bin` to `PATH`.

| Command | Role |
|---|---|
| `gameops run` | Entrypoint. Acts as PID 1 (reaps orphans, forwards signals), then supervises the server (§4) and serves `/metrics` and `/healthz` on `METRICS_PORT` (§4.6). |
| `gameops backup` | One backup cycle: quiesce → save → archive → verify → prune → notify. Also the scheduled job. |
| `gameops backup list` · `backup verify [archive\|latest]` | List this game's archives, newest first; read one back end to end and confirm it holds every current backup path. |
| `gameops restore <archive\|latest> [--no-backup]` | Put an archive back: verify, warn players, safety backup, graceful stop, swap, relaunch in place (§4.5). |
| `gameops update` | One update check: detect → warn players → back up → request restart. Also the scheduled job. |
| `gameops notify <EVENT> [key=value…]` | Send one lifecycle notification. |
| `gameops console <line>` | Write a line to the server's stdin. |
| `gameops health` | Health check; exit 0 when healthy. Wire it as the image `HEALTHCHECK`. |
| `gameops rcon <command…>` | Source-RCON client against `127.0.0.1:${RCON_PORT}` with `RCON_PASSWORD`. |
| `gameops http get [-o file] <url>` | HTTPS GET with retries, to stdout or a file. |
| `gameops json get\|set\|escape\|array …` | JSON for adapters (§6). |
| `gameops steam install\|update-check …` | SteamCMD install and depot-manifest update detection. |
| `gameops players` · `wait-settled` · `tcp-open` | Small helpers, all reachable through the shim as bash functions. |

The toolkit has no runtime dependencies of its own: it does HTTP, JSON, RCON, archiving and
scheduling itself.

---

## 2. What a game image must provide

### 2.1 Base requirements
`bash` ≥ 5 and `coreutils` (the adapter is a bash file; `follow_log` uses `tail`), plus
`ca-certificates` when the game talks HTTPS through the toolkit. Anything the game itself needs
(SteamCMD, a JRE, an X server, …) is the game image's business. No `curl`, `jq` or `tar` is needed
for the toolkit's sake.

### 2.2 A fixed non-root user
The image sets `USER <uid>:<gid>` and never starts as root. The toolkit has no `PUID`/`PGID` path
and never `chown`s: a volume that is not writable by the image's user is a configuration error, and
`gameops run` refuses to start with a clear message. If no process is ever root, a root-owned save
file is not merely avoided — it is unrepresentable.

### 2.3 The adapter
A single bash file at `/opt/game/adapter.sh` (override with `GAME_ADAPTER`). It is **sourced**, not
executed, by every toolkit command, so it must have no side effects at source time beyond defining
functions and variables. Start it with `# shellcheck shell=bash` and `# shellcheck disable=SC2034`
(its variables are consumed by the toolkit, which shellcheck cannot see). It must set:

| Variable | Meaning |
|---|---|
| `GAME_NAME` | Short lowercase identifier (`factorio`). Used in archive names and logs. |
| `GAME_DIR` | Where the server binaries live (`/opt/factorio`). Must be writable when updates are supported. |
| `GAME_PORT` / `GAME_PORT_PROTO` | Primary player port and `tcp`\|`udp`, for the default health check. Omit for games with no inbound port. |
| `GAME_LOG` | *(optional)* Console log path. Default `${DATA_DIR}/logs/console.log`. |

### 2.4 Dockerfile shape
```dockerfile
FROM debian:trixie-slim
COPY --from=ghcr.io/reclyptor/gameops:1.0.0 /opt/gameops /opt/gameops
COPY adapter/ /opt/game/
ENV PATH="/opt/gameops/bin:${PATH}" DATA_DIR=/data BACKUP_DIR=/backups
USER 1000:1000
VOLUME ["/data", "/backups"]
HEALTHCHECK --interval=60s --start-period=10m CMD ["gameops", "health"]
ENTRYPOINT ["gameops", "run"]
```

---

## 3. The adapter interface

Every function is a bash function defined by `adapter.sh`. Exit codes carry meaning:

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Failure — the toolkit logs it and treats the operation as failed. |
| `2` | **Unsupported by this game.** The toolkit degrades as described per function. Never a failure. |

Functions marked *required* must exist; `gameops run` refuses to start otherwise. Optional
functions have a built-in default (usually "return 2").

### 3.1 Install and version
| Function | Required | Contract |
|---|---|---|
| `game_install` | yes | Install the server into `GAME_DIR` if it is not already installed. Idempotent; called on every boot. |
| `game_version` | yes | Print the installed version on stdout (`2.0.77`). |
| `game_update_available` | no | Print the **target** version on stdout and return `0` when an update exists; return `1` when current. Default: `2`. |
| `game_update_apply <target>` | no | Replace the installation with `<target>`. The server is stopped. Return `0` on success. Default: `2`. |

### 3.2 Process
| Function | Required | Contract |
|---|---|---|
| `game_start_cmd` | yes | Populate the array `GAME_CMD` with the server command line. The runner execs it with stdin ← console FIFO and stdout+stderr → console log and container stdout. Working directory is `GAME_DIR`. |
| `game_ready` | yes | Return `0` once the server accepts players. Polled every 5 s up to `READY_TIMEOUT`. |
| `game_shutdown` | no | Ask the server to stop gracefully, saving first. Return `0` when the request was accepted; the runner then waits up to `STOP_TIMEOUT` for the process to exit before `SIGKILL`. Default: `SIGTERM` to the server process. |
| `game_healthy` | no | Override the health check. Default: server process alive **and** ready flag set **and** (if `GAME_PORT` is TCP) the port accepts a connection. Override it for games whose listener must not be probed. |

### 3.3 Control
| Function | Required | Contract |
|---|---|---|
| `game_save` | no | Force a world save now. Default: `2` — backups then rely on the game's own autosave and the settled-file guard (§5). |
| `game_broadcast <message>` | no | Show a message to online players. Default: `2` — update warnings then go only to the notifier and the countdown becomes "wait until empty" (§4.3). |
| `game_server_address` | no | Print the address players actually connect to, when it is not what `SERVER_ADDRESS` says — a client that cannot resolve hostnames, a tunnel the game must be told about by IP. Print nothing when there is none. Called after `game_ready`, before START. Default: `2` — START announces `SERVER_ADDRESS`. |
| `game_join_password` | no | Print the join password the server is enforcing right now; print nothing when it has none. For games that make one up when `GAME_PASSWORD` is empty or invalid, so START announces the password that actually lets players in. Called after `game_ready`, before START. Default: `2` — START announces `GAME_PASSWORD`. |
| `game_players` | no | Print the number of online players. Default: the size of the toolkit's tracked player set, which is fed by `game_events`. If neither exists the count is `0` and the toolkit says so at startup. |

### 3.4 Events
| Function | Required | Contract |
|---|---|---|
| `game_events` | no | Long-running. Emit one line per event on stdout: `JOIN <name>` or `LEAVE <name>`. Exit when the server exits (use `follow_log`, §6, which does this for you). Default: no player events. |

### 3.5 Backups
| Function | Required | Contract |
|---|---|---|
| `game_backup_paths` | yes | Print paths to archive, one per line, **relative to `DATA_DIR`**. Missing paths are skipped with a warning. |
| `game_backup_begin` / `game_backup_end` | no | Quiesce hooks around the archive step. Default: no-op. |

---

## 4. Lifecycle

### 4.1 Boot (`gameops run`)
1. Validate the environment (§7): booleans, integers, cron expressions.
2. Load the adapter; verify required functions and variables.
3. Verify `DATA_DIR`, `BACKUP_DIR` (when backups are enabled) and, when updates are supported,
   `GAME_DIR` are writable by the current user. Refuse to start otherwise.
4. `game_install`.
5. If `UPDATE_ON_BOOT=true` and updates are supported: `game_update_available` → `game_update_apply`.
6. Start the scheduler for the enabled jobs.
7. Enter the runner loop.

### 4.2 The runner loop
```
loop:
  open console FIFO
  game_start_cmd → launch; record pid; copy output to GAME_LOG and stdout
  start game_events → player set + JOIN/LEAVE notifications
  wait game_ready (READY_TIMEOUT) → set ready flag → game_server_address + game_join_password → notify START
  wait for server exit
  if an update was requested:  take the job lock → game_update_apply → notify UPDATE_POST → loop
                               (apply failed → notify UPDATE_FAILED → loop on the installed version;
                                that target is not retried until a different one appears)
  if a restore was requested:  take the job lock → swap the archive in → notify RESTORE_POST → loop
  if a stop was requested:      exit 0
  otherwise:                    log the crash, notify CRASH, exit with the server's code
```
Updates therefore relaunch **in place** — the container does not exit and the orchestrator sees
nothing. The job lock taken before an apply or swap is held until the relaunched server is ready
(§4.8). Crashes are *not* retried inside the container; that is the orchestrator's job, and hiding
crash loops from it is how they go unnoticed.

`SIGTERM` to PID 1 → stop requested → notify STOP → `game_shutdown` → wait `STOP_TIMEOUT` → `SIGKILL`
→ wait for a running job to finish (no job starts once the stop is requested) → exit 0. Set the orchestrator's grace period above
`STOP_TIMEOUT`.

### 4.3 Update (`gameops update`, scheduled)
1. Take the job lock (§4.8).
2. `game_update_available` → exit quietly when current.
3. `notify UPDATE_PRE` with `version`, `warn_minutes` and `restart_note` ("restarting now" when
   nobody is online, "restarting in N minutes" when the game can broadcast, otherwise "restarting
   once the server is empty" — the message never promises a countdown the game cannot give).
4. If players are online:
   - `UPDATE_SKIP_IF_PLAYERS=true` → notify `UPDATE_DEFERRED`, exit; the next scheduled run retries.
   - `game_broadcast` supported → countdown: announce at `UPDATE_WARN_MINUTES`, then at 10, 5, 2, 1
     minutes, 30 s and 10 s (only the marks ≤ the configured warning). Re-check the player count each
     minute and stop counting down as soon as the server is empty.
   - `game_broadcast` unsupported → wait until the server is empty, polling every minute, up to
     `UPDATE_FORCE_AFTER_MINUTES`, then proceed anyway.
5. If `BACKUP_ON_UPDATE=true` → run a backup cycle; a failed one is reported (§4.4) and the update
   goes ahead.
6. Write the update request (target version) and call `game_shutdown`. The runner does the rest.

### 4.4 Backup (`gameops backup`, scheduled or by hand)
Every cycle carries its trigger as `backup_kind` — `Scheduled` (the cron), `Manual` (`gameops backup`
by hand), `Pre-update`, `Pre-restore` — so a post never looks like the nightly run firing at the
wrong time.
1. Take the job lock (§4.8). 2. `notify BACKUP_PRE`. 3. `game_backup_begin`. 4. `game_save` (if supported).
5. Archive `game_backup_paths` into `${BACKUP_DIR}/${GAME_NAME}-<YYYY-MM-DD_HH-MM-SS>.tar.gz`
   (`…-<n>.tar.gz` when that second already has one), written in process to a temporary file. 6. `game_backup_end`. 7. **Verify** the temporary file:
   read it back in full (gzip checksum, every tar entry) and require every path that was archived
   to be present; a failure is a failed backup (`BACKUP_FAILED`, nothing left behind). 8. Rename it
   into place atomically. 9. Prune this game's archives older than `BACKUP_RETAIN_DAYS` (when > 0).
10. `notify BACKUP_POST` with `file_path`.

A backup never fails quietly. Every way one can fail — `BACKUP_DIR` not writable, no backup path
present, the archive, its verification, the final rename, and for `gameops backup` never getting
the job lock — sends `BACKUP_FAILED` with the reason and counts in `backup_failures_total`;
`gameops backup` then exits non-zero. (A failed pre-update backup is reported the same way and the
update carries on, as step 5 of §4.3 says.)

`gameops backup verify [archive|latest]` runs step 7 against an existing archive, requiring the
adapter's backup paths that exist on disk right now. `gameops backup list` prints the archives
newest first.

### 4.5 Restore (`gameops restore <archive|latest> [--no-backup]`, by hand)
1. Take the job lock (§4.8). 2. Resolve the archive (`latest`, a name in `BACKUP_DIR`, or a path) and verify
   it; refuse anything that does not pass. 3. If no server is running, swap now and `notify
   RESTORE_POST`. Otherwise: 4. `notify RESTORE_PRE` with `file_path`, `warn_minutes` and
   `restart_note`. 5. The same
   player countdown as an update ("Server restarting to restore a backup in …"). 6. A safety backup
   of the live data, unless `--no-backup` (a failed safety backup aborts the restore). 7. Write the
   restore request and call `game_shutdown`; the runner swaps and relaunches in place.

The swap is staged: the archive is extracted under `${DATA_DIR}/.restore-staging/`, then each of
its top-level paths is renamed over the live one (the previous tree parked under `.restore-old/`
until every rename has succeeded). A failure mid-swap moves the old paths back, sends
`RESTORE_FAILED`, and the container exits 1 on the pre-restore data. Paths the archive does not
contain are never touched.

### 4.6 Metrics and HTTP health (`METRICS_PORT`, default 9110)
The runner's one listener. `GET /metrics` is Prometheus text; `GET /healthz` is `200 ok` or
`503 <reason>` from the same check as `gameops health`. Nothing is accepted or changed. `0` disables
it. Every value is read from `GAMEOPS_STATE` and `BACKUP_DIR` at request time, so work done by the
cron jobs (separate processes) is visible.

| Metric | Meaning |
|---|---|
| `gameops_info{game,toolkit_version,server_version}` | Always 1; `server_version` is `game_version` at the last launch. |
| `gameops_server_up` · `gameops_server_ready` | Process running · `game_ready` passed. |
| `gameops_server_start_timestamp_seconds` | Launch time of the current server process. |
| `gameops_players_online` | The tracked player set. |
| `gameops_backup_last_timestamp_seconds` · `_last_size_bytes` · `gameops_backup_archives` | Newest archive's mtime and size; archives on disk. |
| `gameops_update_pending{version}` | 1 while a newer version is known and not yet applied. |
| `gameops_backups_total` · `backup_failures_total` · `updates_total` · `restores_total` · `notify_failures_total` · `server_restarts_total` · `gate_passed_total` · `gate_dropped_total` | Counters since the container started. |

### 4.7 The TCP gate (`GATE_ENABLED`)
Some servers cannot survive a connection that opens and drops before it says anything (vanilla
Terraria dies of an `ObjectDisposedException` in its network loop). With the gate on, the runner
listens on the public port itself and the game listens on a loopback port; a client is forwarded
only after it has sent its first bytes — containing `GATE_EXPECT` when set — within
`GATE_TIMEOUT_SECONDS`. Port scanners, health probes and half-open connections are closed at the
door; real clients pass through untouched (their first bytes are replayed to the game). The adapter
points the game at `GATE_TARGET_PORT` and binds it to `127.0.0.1`.

### 4.8 The job lock
Backups, update checks and restores share one lock under `GAMEOPS_STATE`, and they **take turns**:
a job that finds it held waits for it, retrying every second, up to `LOCK_TIMEOUT`. The default
schedules put the nightly backup and an hourly update check in the same second, so this is the
normal case, not a corner — a job that simply skipped would lose the nightly backup to a coin flip
without anyone noticing.

- **The runner holds it across a relaunch.** An update or restore releases the lock once it has
  asked the server to stop; the runner takes it before `game_update_apply` or the swap and keeps it
  until the relaunched server is ready. A job that was waiting therefore runs against a live server,
  never one that is half way through a relaunch. If the runner cannot get the lock within
  `LOCK_TIMEOUT` it relaunches anyway and logs a warning; the relaunch is never held hostage.
- **Nothing starts on a stopping container.** Once a stop is requested a waiting job gives up, and
  so does one that would have started; it logs a warning and exits non-zero. The shutdown saves
  the world itself.
- **Giving up is loud.** A job that is still waiting after `LOCK_TIMEOUT` exits non-zero with
  `… not started: timed out waiting for the job lock after Ns`; a backup also sends
  `BACKUP_FAILED` and counts in `backup_failures_total`.

`LOCK_TIMEOUT` defaults to the longest a legitimate holder can keep the lock — the update
countdown (`max(UPDATE_WARN_MINUTES, UPDATE_FORCE_AFTER_MINUTES)`), `STOP_TIMEOUT`,
`UPDATE_APPLY_TIMEOUT` and `READY_TIMEOUT`, plus an hour for the backups taken along the way
(10020 s with the defaults) — so raising any of those limits never makes a job give up on a holder
that is still doing its job. `0` means a single attempt.

## 5. The settled-file guard
Games without `game_save` (or whose save is asynchronous) can be mid-write when the archive is
taken. The archiver detects a file that changed while it was being read and retries once after
`BACKUP_SETTLE_SECONDS`. Adapters can also use `wait_settled <path>` (§6) inside `game_shutdown` to
avoid stopping a server mid-autosave.

---

## 6. Helpers available to adapters
All of these are defined (by `shim/adapter.sh`) by the time `adapter.sh` is sourced. The shim also
settles `DATA_DIR`, `BACKUP_DIR`, `GAME_ADAPTER` and `GAME_LOG` before the adapter runs, and
`adapter_load` — the one way to load an adapter — sources it and fills in `GAME_PORT`,
`GAME_PORT_PROTO` and `SERVER_NAME` afterwards. To poke at a running server by hand:

    docker exec <container> bash -c 'source /opt/gameops/shim/adapter.sh; adapter_load; game_players'

| Helper | Purpose |
|---|---|
| `log_info` / `log_warn` / `log_error` / `log_debug` / `log_action` / `die` | Log lines on stderr; respect `LOG_LEVEL`. |
| `is_true <value>` · `is_int <value>` | Case-insensitive `true`/`1`/`yes`/`on`; digits only. |
| `require_var <NAME> [why]` | Fail fast when an env var is empty. |
| `server_pid` | PID of the running server process, or empty. |
| `flag_set` / `flag_get` / `flag_clear` / `flag_is_set <name>` | Small state flags under `GAMEOPS_STATE`, shared with the toolkit. |
| `follow_log [path]` | `tail -n0 -F` a log file, exiting when the server exits. The basis of every log-driven `game_events`. |
| `console_send <line>` | Write a line to the server's stdin. |
| `rcon <command…>` | Run a command over RCON against `127.0.0.1:${RCON_PORT}` with `RCON_PASSWORD`. Prints the reply. |
| `tcp_port_open <host> <port>` | True when a TCP connect succeeds. |
| `http_get [-o file] <url>` | HTTPS GET with retries, to stdout or a file. |
| `json_get <file\|-> <path>` | Value at a path (`.stable.headless`, `.mods[1].name`, `.[0]`, `["1963720"].depots`). Strings unquoted; objects/arrays as compact JSON. Exit 1 when missing. |
| `json_set <file> <path> <value> [--raw]` | Write a string (or, with `--raw`, a JSON value) at a path, creating the file or intermediate objects. |
| `json_escape <string>` · `json_array [items…]` | Build JSON literals safely. |
| `wait_settled <path> [quiet-seconds] [timeout]` | Block until nothing under the path has changed for N seconds. |
| `steam_install [--keep path]… <dir> <appid…>` | SteamCMD anonymous install/update with validation (`STEAMCMD_DIR`, default `/home/steam/steamcmd`). In place first (app-info refreshed, stalls killed, 3 attempts); if SteamCMD keeps failing against the existing installation, the build is installed fresh into a staging directory and swapped in by rename, with every `--keep` path (the game's own data inside the install dir, e.g. `Pal/Saved`) carried across. A failed swap is rolled back. |
| `steam_update_available <dir> <appid> <depot>` | Compare the local app manifest with the public one; prints the remote manifest id and returns 0 when they differ, 1 when current. |
| `players_count` | The toolkit's own count from the tracked player set. |
| `notify <EVENT> [key=value…]` | Send a notification; game-specific events are fine (set `DISCORD_<EVENT>_MESSAGE` as the default text). |

---

## 7. Environment contract
Common to every game image. Game settings are **not** namespaced by the game: the same concept has
the same name in every image (§7.1), and a setting only one game has still gets a plain descriptive
name (`DLC_SPACE_AGE`, `SEASON`, `JVM_OPTS`), never a `<GAME>_` prefix.

| Variable | Default | Meaning |
|---|---|---|
| `DATA_DIR` | `/data` | Persistent game data. |
| `BACKUP_DIR` | `/backups` | Archive destination. A plain mount point — a PVC, an NFS share, anything. |
| `SERVER_NAME` | `${GAME_NAME}` | Human name used in notifications. |
| `BACKUP_ENABLED` | `true` | Schedule backups. `gameops backup` works by hand regardless. |
| `BACKUP_CRON` | `0 4 * * *` | Schedule (5-field cron, container `TZ`). |
| `BACKUP_RETAIN_DAYS` | `14` | Prune archives older than this. `0` keeps everything. |
| `BACKUP_ON_UPDATE` | `true` | Back up before applying an update. |
| `BACKUP_SETTLE_SECONDS` | `10` | Retry delay for the settled-file guard. |
| `UPDATE_ENABLED` | `true` | Schedule update checks (no effect when the adapter does not support updates). |
| `UPDATE_CRON` | `0 * * * *` | Schedule. |
| `UPDATE_ON_BOOT` | `true` | Check and apply before the first launch. |
| `UPDATE_WARN_MINUTES` | `15` | Countdown length when players are online and broadcast is supported. |
| `UPDATE_SKIP_IF_PLAYERS` | `false` | Defer instead of counting down. |
| `UPDATE_FORCE_AFTER_MINUTES` | `30` | Without broadcast: how long to wait for an empty server before updating anyway. |
| `UPDATE_APPLY_TIMEOUT` | `3600` | Seconds a `game_update_apply` may run; on expiry it is killed, the installed version relaunches, `UPDATE_FAILED` is sent and that build is not retried. |
| `STALL_SECONDS` | `300` | `steam_install`: a SteamCMD run that prints nothing for this long is killed and retried (up to 3 attempts, app-info cache refreshed between them). |
| `READY_TIMEOUT` | `900` | Seconds to wait for `game_ready` before declaring the launch failed. |
| `STOP_TIMEOUT` | `120` | Seconds between `game_shutdown` and `SIGKILL`. |
| `LOCK_TIMEOUT` | derived (10020) | Seconds a backup, update check or restore waits for the job lock before failing loudly (§4.8). `0` makes a single attempt. |
| `PLAYER_EVENTS_ENABLED` | `true` | Run `game_events` and send JOIN/LEAVE notifications. |
| `METRICS_PORT` | `9110` | `/metrics` and `/healthz` listener (§4.6). `0` disables it. |
| `GATE_ENABLED` | `false` | Put the TCP gate (§4.7) in front of the game port. |
| `GATE_PORT` | `${PORT}` | Port the gate listens on — the public one. |
| `GATE_TARGET_PORT` | — | Loopback port the game itself listens on when gated. |
| `GATE_EXPECT` | — | Marker a client's first bytes must contain to be forwarded (e.g. `Terraria`); empty forwards any first bytes. |
| `GATE_TIMEOUT_SECONDS` | `3` | How long a client has to send its first bytes. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. |
| `TZ` | `UTC` | Schedules and log timestamps. |
| `RCON_PORT`, `RCON_PASSWORD` | — | Used by `rcon` when the game has RCON. Env only; the adapter writes them to disk if the game needs a file. |
| `GAME_PASSWORD` | — | Join password, where the game has one. Env only. START announces it — unless the adapter's `game_join_password` reports the one actually in force, which wins. |
| `SERVER_ADDRESS` | — | How players reach the server (`host:port`, or any text). Only used in notifications, as `server_address` — unless the adapter's `game_server_address` reports the address actually in use, which wins. |

### 7.1 Shared game vocabulary
An adapter that exposes one of these concepts must use exactly this name. Games without the concept
simply do not read the variable.

| Variable | Meaning |
|---|---|
| `SERVER_NAME` | The server's name: used in every notification, and written into the game's own server-name field where one exists (Factorio `name`, Palworld `ServerName`). |
| `SERVER_DESCRIPTION` | Server description / listing text. |
| `MAX_PLAYERS` | Player slots. |
| `GAME_PASSWORD` | Join password. |
| `ADMIN_PASSWORD` | Admin / management password, where distinct from RCON. |
| `ADMINS` · `WHITELIST` | Comma-separated player names: operators, and the allow-list (non-empty turns it on). |
| `MOTD` | Message of the day. |
| `WORLD_NAME` · `WORLD_SEED` · `WORLD_SIZE` · `WORLD_MODE` · `WORLD_INDEX` | The world / save being played, and how it is generated. |
| `DIFFICULTY` | Difficulty, in the game's own values. |
| `PUBLIC` · `LAN` | Advertise on the public server browser / on the LAN. |
| `BIND` · `PORT` · `QUERY_PORT` | Bind address, game port, query port. |
| `RCON_ENABLED` · `RCON_PORT` · `RCON_PASSWORD` · `REST_API_ENABLED` · `REST_API_PORT` | Control channels. |
| `VERSION` · `CHANNEL` · `SERVER_TYPE` | Version pin or `latest`; release channel; server flavour (e.g. vanilla / paper / fabric). |
| `GAME_ID` · `LANGUAGE` | Where the game has them. |

### Notifications
| Variable | Default | Meaning |
|---|---|---|
| `NOTIFY_PROVIDER` | `discord` | `discord` or `none`. The switch exists so another provider can be added without touching adapters. |
| `DISCORD_WEBHOOK_URL` | — | Empty disables all notifications silently. |
| `DISCORD_SUPPRESS_NOTIFICATIONS` | `false` | Send every post with Discord's silent flag (no pings). |
| `DISCORD_SILENT_EVENTS` | `BACKUP_PRE,BACKUP_POST` | Events posted silently (no ping). `ALL` covers every event, including ones an adapter invents, so a new event type can never arrive loud by accident; `!EVENT` exempts one from it — `ALL,!JOIN,!LEAVE` pings only for players arriving and leaving. Listing an event both ways is a configuration error. |
| `DISCORD_DISABLED_EVENTS` | — | Events **not sent at all**, in the same vocabulary: `ALL,!JOIN,!LEAVE` posts joins and leaves and nothing else, and keeps meaning that when a new event type appears. Silencing and omitting are different acts — a silenced event still arrives, unpinged. `DISCORD_<EVENT>_ENABLED` wins over this when it is set, because a switch aimed at one event beats a blanket policy. Listing an event both ways is a configuration error. |
| `DISCORD_EMBEDS` | `false` | `true` sends a coloured embed card; `false` sends plain text. |
| `DISCORD_USERNAME` | — | Override the webhook's display name. |
| `DISCORD_<EVENT>_ENABLED` | `true` | Per-event switch. |
| `DISCORD_<EVENT>_MESSAGE` | see below | Per-event text. |

`EVENT` ∈ `START`, `STOP`, `CRASH`, `UPDATE_PRE`, `UPDATE_POST`, `UPDATE_DEFERRED`, `BACKUP_PRE`,
`UPDATE_FAILED`, `BACKUP_POST`, `BACKUP_FAILED`, `RESTORE_PRE`, `RESTORE_POST`, `RESTORE_FAILED`, `JOIN`, `LEAVE`,
plus any game-specific event an adapter sends.
Messages may use bare placeholder words, replaced verbatim: `server_name`, `player_name`, `version`,
`file_path`, `backup_kind` (`Scheduled`, `Manual`, `Pre-update`, `Pre-restore`), `warn_minutes`,
`restart_note`, `reason`, `server_address` (what `game_server_address` reports, else
`SERVER_ADDRESS`) and `game_password` (what `game_join_password` reports, else `GAME_PASSWORD`),
plus any `key=value` pairs passed to `notify`. A reported `server_address=…` or `game_password=…`
is authoritative even when empty: a server that reports no password gets no password fragment,
whatever the environment says. **A START message always carries the connection details:** when `SERVER_ADDRESS`
or a join password is in force and the message text does not place `server_address` / `game_password`
itself, they are appended before the closing full stop (`… — connect to host:port (password: …).`); when
no address or no password is configured, that part of the sentence is dropped, never left dangling. The channel a webhook posts
to is treated as the invited players' channel, so the password belongs there. Defaults:

```
START            🟢 server_name server is online — connect to `server_address` (password: `game_password`).
STOP             💤 server_name server has shut down.
CRASH            💥 server_name server stopped unexpectedly (reason).
UPDATE_PRE       ⏳ server_name server is updating to version — restart_note.
UPDATE_POST      🚀 server_name server updated to version.
UPDATE_DEFERRED  ⏸️ server_name server update to version deferred: players online.
UPDATE_FAILED    ❌ Update of the server_name server to version failed: reason — still running the installed version.
BACKUP_PRE       💾 backup_kind backup of the server_name server…
BACKUP_POST      ✅ backup_kind backup of the server_name server complete: file_path
BACKUP_FAILED    ❌ backup_kind backup of the server_name server failed: reason
RESTORE_PRE      ♻️ server_name server is restoring file_path — restart_note.
RESTORE_POST     ♻️ server_name server restored from file_path.
RESTORE_FAILED   ❌ Restore of the server_name server from file_path failed: reason
JOIN             🟢 **player_name** joined the server_name server.
LEAVE            🔴 **player_name** left the server_name server.
```

---

## 8. Runtime state
`GAMEOPS_STATE` (default `/tmp/gameops`) holds: `server.pid`, `server.started`, `server.version`,
`console.fifo`, `ready`, `players` (one name per line, lock-guarded), `update.requested` (target
version), `update.available` (a known newer version), `update.failed` (a target whose install
failed; not retried), `restore.requested` (archive path),
`stop.requested`, `lock`, and `counters/` (the `/metrics` counters). It is ephemeral and recreated on every boot.

## 9. Adding a game — checklist
1. New public repo `Reclyptor/<Game>` with the layout in the toolkit README.
2. `adapter/adapter.sh` implementing §3; put parsers in `adapter/lib/` so they are unit-testable.
3. `tests/*.bats` with fixtures for every log line you parse.
4. `Dockerfile` per §2.4, pinning a toolkit version, on a plain distribution base with only the
   packages the game itself needs.
5. `README.md` leading with the env table; `SPEC.md`.
6. A local run proving: install → ready → save → backup → shutdown, and the update-check source.
