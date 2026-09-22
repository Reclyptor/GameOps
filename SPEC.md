# SPEC: GameOps — the operations layer every Reclyptor game server shares

**Status:** IMPLEMENTED — v1.0.0.
**Release policy:** `v1.0.0` is re-cut in place when the toolkit changes, rather than bumped. Game
images pin `1.0.0` and pick the change up on their next rebuild, which is triggered deliberately
(`workflow_dispatch`) because nothing in the game repo itself changed. The consequence to know
about: `1.0.0` does not identify fixed bytes, so a game image cannot report which toolkit build it
carries — its own `<timestamp>-<sha>` tag dates the build, and that is the only handle.
**Deliverable:** `ghcr.io/reclyptor/gameops`, a distribution-only image holding `/opt/gameops`, that
game images copy in. The interface it exposes is [`docs/CONTRACT.md`](docs/CONTRACT.md).

---

## 1. Purpose

Five dedicated game servers (Factorio, Palworld, Minecraft, Terraria, Core Keeper) run on the cluster.
A game server should look after itself: scheduled backups with retention, automatic updates that
warn the people playing and relaunch without the container exiting, notifications for lifecycle and
player events, a graceful stop, a health check that does not disturb the game. None of that is
game-specific, so it is written once, here, and every game contributes only what genuinely differs:
how to install, start, save, stop, count players and detect an update — through a small bash adapter.

### Non-goals
- **Not a game server.** It contains no game. A game image is a plain distribution base + this.
- **Not a controller.** No control API, no database. One binary, driven by a schedule and by
  signals. The only listener is the read-only `/metrics` + `/healthz` endpoint on `METRICS_PORT`;
  it changes nothing and accepts nothing.
- **Not multi-arch.** `linux/amd64` only; every game it serves ships x86-64 binaries and every
  cluster node is amd64.
- **Not a notification hub.** Discord webhooks only, behind a provider switch.
- **Not a privilege manager.** No `PUID`/`PGID` root-then-drop. Game images run as a fixed
  non-root user and volumes must already be writable by it.

---

## 2. Hard constraints

| # | Constraint |
|---|---|
| G1 | **Adapters are the only game-specific code.** If the toolkit needs an `if factorio` it is wrong. |
| G2 | **Degrade, never fail, on missing capabilities.** A game with no save command or no broadcast still gets backups and updates; the contract says exactly how (return code 2). |
| G3 | **Updates relaunch in place.** The container does not exit for an update. Crashes *do* exit — hiding a crash loop from the orchestrator is how it goes unnoticed. |
| G4 | **No secret touches a layer.** `RCON_PASSWORD`, `GAME_PASSWORD`, webhook URLs are env; adapters render them to disk at boot if the game insists on a file. |
| G5 | **Self-contained.** The toolkit depends on nothing at runtime: HTTP, JSON, RCON, archiving, scheduling and PID-1 duties are all its own code. Game images need only bash and coreutils for the adapter. |
| G6 | **Lean.** The toolkit image is one static binary and a shim; game images carry only the packages the game itself needs. |

---

## 3. The image

`FROM scratch` containing only `/opt/gameops`: `bin/gameops` (static, `CGO_ENABLED=0`, built from
the standard library alone in a `debian:trixie-slim` stage with the distribution's Go), `shim/`
(the bash helper library and the function invoker), and `docs/CONTRACT.md`.

| Subcommand group | Implementation |
|---|---|
| `run` | PID-1 init (re-execs itself, forwards signals, reaps orphans) around the supervisor loop. |
| `backup` / `update` | The scheduled jobs, also runnable by hand; they take turns on one shared file lock. `backup list` and `backup verify` read archives back. |
| `restore` | Verified archive → countdown → safety backup → graceful stop → staged swap → relaunch in place. |
| `drain` | Blocks until stopping the server is acceptable, then returns. The orchestrator's
pre-stop hook; see §5. |
| `notify` / `console` / `health` | Lifecycle helpers; `health` and `/healthz` share one check. |
| `/metrics`, `/healthz` | Served by `run` on `METRICS_PORT` (Prometheus text, rendered by hand); all values read from the state directory and `BACKUP_DIR` at request time. |
| `rcon` / `http` / `json` / `steam` / `players` / `wait-settled` / `tcp-open` | Adapter helpers, exposed as bash functions by the shim. |

---

## 4. Design decisions worth recording

- **A binary, bash adapters.** The hard parts — a binary network protocol, JSON, cron, process
  supervision under PID 1 — belong in a compiled language with a standard library that covers them
  and a unit test for each. The game-specific parts are shell by nature (`steamcmd`, `factorio
  --create`), so adapters stay bash, and the contract is a set of bash functions.
- **The shim keeps adapters simple.** Every helper an adapter calls is a bash function; the ones that
  do real work call back into the binary. An adapter never needs to know that.
- **Console FIFO for every game.** The runner always attaches a FIFO to the server's stdin. Games
  with RCON never use it; games without one get `gameops console` for free.
- **Tracked player set.** JOIN/LEAVE events maintain a set in `GAMEOPS_STATE`. It is the default
  `game_players`, which is what makes the update countdown work for games with no query API.
- **One lock, and jobs take turns.** Backups, update checks and restores share one lock, and a job
  that finds it held waits (up to `LOCK_TIMEOUT`) instead of skipping. Skipping was the original
  design and it was wrong: the default schedules start the nightly backup and an hourly update
  check in the same second, so a skip-on-contention lock lost roughly every other nightly backup,
  and exited 0 while doing it. The runner holds the lock from an update's apply or a restore's swap
  until the relaunched server is ready, so a job that waited never lands mid-relaunch; nothing
  starts on a stopping container; and a job that cannot get its turn fails loudly.
- **START announces what actually works, not what was configured.** Two optional adapter
  functions report the connection details in force — `game_server_address` and
  `game_join_password` — and what they report wins over `SERVER_ADDRESS` and `GAME_PASSWORD`.
  The environment says what the operator intended; the adapter says what the server is doing,
  and the channel needs the latter. Games where the two never differ implement neither.
- **START announces the password that actually works.** Some games make up a join password when
  none is configured, or when the configured one is invalid (Core Keeper in direct-connect mode
  does both). Announcing `GAME_PASSWORD` would then tell players nothing, or the wrong thing, so an
  adapter can report the password in force through `game_join_password`, and that report wins.
  Games that never invent one simply don't implement it.
- **No failure is quiet.** Every way a backup can fail goes through one path that logs it, counts
  it and posts `BACKUP_FAILED`. A backup nobody hears about failing is as bad as none.
- **Atomic archives, torn-file detection.** Archives are written in process to a temporary file and
  renamed; a file that grows or shrinks while being read is detected and the cycle retries once.
- **No archive counts until it has been read back.** Every backup is verified (gzip checksum, every
  entry, every archived path present) before the rename; `backup verify` does the same on demand.
  A backup nobody has ever read is a hope, not a backup.
- **Restore swaps, never overwrites.** The archive is extracted to a staging directory and the
  live paths are exchanged by rename, so a failed restore leaves the world exactly as it was — and
  the restore takes its own safety backup first.
- **Plain-text Discord by default.** Embed cards are opt-in.
- **Omission is a policy too, and the same policy.** `DISCORD_DISABLED_EVENTS` drops events
  entirely rather than posting them unpinged, and it reads the same `ALL` / `!EVENT` vocabulary as
  `DISCORD_SILENT_EVENTS` — one parser, `parseEventPolicy`, serves both. A channel that asks for
  joins and leaves only is asking for a policy, not a list of thirteen `DISCORD_<EVENT>_ENABLED`
  switches that a newly invented event type would slip straight past. `DISCORD_<EVENT>_ENABLED`
  still wins where it is set: the specific beats the blanket.
- **Silence is a policy, not a list to maintain.** `DISCORD_SILENT_EVENTS=ALL,!JOIN,!LEAVE` says
  what an operator actually means — ping me for players, nothing else — and keeps meaning it when
  a new event type appears, which an explicit list of every event would not.
- **Metrics come from files.** Cron jobs are separate processes, so anything they count lives in
  `GAMEOPS_STATE/counters/` under a lock; the listener only reads. Counters reset with the
  container, which is what a scraper expects.

---

## 5. Orchestrated restarts (`gameops drain`)

### The gap this closes

G3 says updates relaunch in place, and the countdown makes those restarts
polite: players are warned at `UPDATE_WARN_MINUTES` and the server empties on
its own where it can. None of that runs when the restart comes from *outside*
the container. A new image, a node drain or a rescheduled pod sends `SIGTERM`
straight to PID 1, and the first thing players know about it is the
disconnect. The toolkit's entire graceful-restart design is bypassed by the
layer above it.

`gameops drain` closes that gap by making the orchestrator's restart take the
same countdown the toolkit's own restart already takes.

### The subcommand

```
gameops drain [--deadline <seconds>]
gameops drain --required-grace
```

`drain` blocks until stopping the server is acceptable, then returns `0`. It
runs the same `Countdown()` the update path runs, with the reason
`for maintenance`, so what players see does not depend on where the restart
came from.

It is called from the orchestrator's pre-stop hook. Kubernetes runs the hook,
waits for it to return, and only then sends `SIGTERM`.

### Behaviour, by what the game supports (G2)

| Game can | Drain does |
|---|---|
| Nobody online | Returns immediately |
| `game_broadcast` | Counts down in-game at the `CountdownMarks`, returning the moment the server empties |
| No `game_broadcast` | Waits for an empty server up to `UPDATE_FORCE_AFTER_MINUTES`, then returns anyway |
| No player tracking at all | Reports 0 players and returns immediately |

Never fails, never blocks forever. A game that cannot warn its players still
drains; it just waits instead of counting down.

### The deadline is the safety property

This is the part that makes `drain` correct or dangerous, and it is worth
stating plainly: **a pre-stop hook runs inside the orchestrator's grace
period.** If `drain` is still counting down when that period expires, the
container is `SIGKILL`ed and the graceful stop — the save — never happens. A
drain that overruns its budget is strictly worse than no drain at all, because
it converts a clean save into a hard kill.

Therefore:

- `--deadline` bounds the wait absolutely. `drain` returns by then whatever the
  player count.
- The default deadline is derived, not guessed:
  `max(UPDATE_WARN_MINUTES, UPDATE_FORCE_AFTER_MINUTES) × 60`.
- The deadline must leave room for the stop that follows it. `drain
  --required-grace` prints `deadline + STOP_TIMEOUT + margin` — the minimum the
  orchestrator's grace period must be — derived the same way
  `lockTimeoutDefault` is derived, so the manifest and the toolkit cannot drift
  apart as those limits change.

A caller that sets a grace period below `--required-grace` is misconfigured, and
the number exists so that is checkable rather than discovered during an incident.

### No job may start during a drain

The lock design already guarantees that nothing starts on a *stopping*
container. A drain is not yet a stop: the hook runs before `SIGTERM`, so from
the scheduler's point of view the container is running normally. Without care, a
nightly backup firing during a fifteen-minute drain would still hold the lock
when `SIGTERM` lands, and the stop path — which waits for a running job — would
push past the grace period and be killed.

`drain` therefore sets a `drain.requested` flag that the scheduler honours the
way it honours a stopping container: **no new job starts once a drain is under
way.** A job already running is allowed to finish. The flag clears if the drain
returns without a stop following it, so a cancelled eviction leaves the
scheduler working normally.

### What `drain` deliberately does not do

It does not stop the server. It returns when stopping is acceptable; the stop
itself remains `SIGTERM` → notify `STOP` → `game_shutdown` → `STOP_TIMEOUT` →
`SIGKILL`, unchanged. `drain` adds no new stop semantics and no new way for the
server to go down.

It also does not decide *whether* to restart. That judgement belongs to whoever
sent the eviction.

### Verification

- `go test ./...`: the deadline is honoured exactly; an empty server returns
  immediately; the broadcast and no-broadcast paths both terminate; the
  `drain.requested` flag blocks a new job and clears afterwards;
  `--required-grace` tracks changes to `UPDATE_WARN_MINUTES`,
  `UPDATE_FORCE_AFTER_MINUTES` and `STOP_TIMEOUT`.
- `tests/smoke.sh`: a drain with a player online counts down and returns early
  when that player leaves; a drain with a player who stays returns at its
  deadline; a backup cannot start once a drain is under way; a drain followed by
  `SIGTERM` still produces a clean save within the grace period.

---

## 6. CI

`.github/workflows/build.yml`: `lint` (gofmt, go vet, go test, shellcheck) and `test`
(`tests/smoke.sh`) gate `build`, which pushes to ghcr on `master` with `latest`, a sortable
`<YYYYMMDDHHmmss>-<sha>` tag, and — on `v*` git tags — semver `1.0.0`, `1.0`, `1`. Game images pin
the full semver.

---

## 7. Verification

- `go test ./...`: cron parsing and scheduling, the RCON client against an in-process fake server
  (auth, single and multi-packet replies), JSON get/set/escape, notification rendering and
  payloads (a reported address or join password wins over `SERVER_ADDRESS` / `GAME_PASSWORD`,
  including when empty), the
  state store (player set, flags, pid, and the job lock: taken when free, waited for when
  held, timed out, abandoned on stop), the Steam manifest parser, countdown marks, archive writing,
  backup failures always being posted and counted, verification (truncated, corrupted, missing
  paths, unsafe names), restore (swap, untouched paths, rollback on a failed swap), listing and
  pruning, configuration validation including `LOCK_TIMEOUT` (derived default, override, `0`) and
  per-event silencing (`ALL`, `!EVENT`, an event listed both ways, an adapter's own event covered
  by `ALL`), the
  health check, metrics rendering, counters under contention, and the listener (`/metrics`,
  `/healthz` 200 and 503).
- `tests/smoke.sh`: builds the toolkit and a fake game (a bash "server" on the console FIFO that
  reports joins, saves on command, and supports a fake update source) and drives it through
  start → JOIN/LEAVE → backup → verify → tamper → restore-in-place → failed and hung updates, with a
  backup queued behind the hung one's relaunch (it must wait, then save the live server) → a backup
  behind a held lock (it must wait, not skip) → a backup that never gets the lock (non-zero,
  `BACKUP_FAILED`, counted) → update-in-place → SIGTERM → the connection details START announces
  (a configured and a server-generated join password, and a server-reported address overriding the
  configured one) → `ALL,!JOIN` silencing a START while a JOIN still pings → crash, asserting on the console log, the
  archive, the restored world, the version file, the webhook bodies captured by a receiver
  container, and `/metrics` values at every stage. Schedules are off during the run so a cron job
  can never interleave with the steps.
