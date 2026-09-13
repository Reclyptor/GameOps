# SPEC: GameOps — the operations layer every Reclyptor game server shares

**Status:** IMPLEMENTED — v1.0.0.
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
| `backup` / `update` | The scheduled jobs, also runnable by hand; one shared file lock. `backup list` and `backup verify` read archives back. |
| `restore` | Verified archive → countdown → safety backup → graceful stop → staged swap → relaunch in place. |
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
- **Locks, not queues.** Backup and update share one lock. Overlaps are logged and skipped.
- **Atomic archives, torn-file detection.** Archives are written in process to a temporary file and
  renamed; a file that grows or shrinks while being read is detected and the cycle retries once.
- **No archive counts until it has been read back.** Every backup is verified (gzip checksum, every
  entry, every archived path present) before the rename; `backup verify` does the same on demand.
  A backup nobody has ever read is a hope, not a backup.
- **Restore swaps, never overwrites.** The archive is extracted to a staging directory and the
  live paths are exchanged by rename, so a failed restore leaves the world exactly as it was — and
  the restore takes its own safety backup first.
- **Plain-text Discord by default.** Embed cards are opt-in.
- **Metrics come from files.** Cron jobs are separate processes, so anything they count lives in
  `GAMEOPS_STATE/counters/` under a lock; the listener only reads. Counters reset with the
  container, which is what a scraper expects.

---

## 5. CI

`.github/workflows/build.yml`: `lint` (gofmt, go vet, go test, shellcheck) and `test`
(`tests/smoke.sh`) gate `build`, which pushes to ghcr on `master` with `latest`, a sortable
`<YYYYMMDDHHmmss>-<sha>` tag, and — on `v*` git tags — semver `1.0.0`, `1.0`, `1`. Game images pin
the full semver.

---

## 6. Verification

- `go test ./...`: cron parsing and scheduling, the RCON client against an in-process fake server
  (auth, single and multi-packet replies), JSON get/set/escape, notification rendering and payloads,
  the state store (player set, flags, lock, pid), the Steam manifest parser, countdown marks,
  archive writing, verification (truncated, corrupted, missing paths, unsafe names), restore
  (swap, untouched paths, rollback on a failed swap), listing and pruning, configuration validation,
  the health check, metrics rendering, counters under contention, and the listener (`/metrics`,
  `/healthz` 200 and 503).
- `tests/smoke.sh`: builds the toolkit and a fake game (a bash "server" on the console FIFO that
  reports joins, saves on command, and supports a fake update source) and drives it through
  start → JOIN/LEAVE → backup → verify → tamper → restore-in-place → update-in-place → SIGTERM →
  crash, asserting on the console log, the archive, the restored world, the version file, the
  webhook bodies captured by a receiver container, and `/metrics` values at every stage.
