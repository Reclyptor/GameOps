# GameOps

The operations layer shared by every [Reclyptor](https://github.com/Reclyptor) game-server image:
a supervised lifecycle, scheduled backups with retention, in-place auto-updates that warn players
first, Discord notifications, player join/leave events, a console pipe, and a health check — as
one static binary that any game image copies in.

```dockerfile
COPY --from=ghcr.io/reclyptor/gameops:1.0.0 /opt/gameops /opt/gameops
```

Games that build on it: [Factorio](https://github.com/Reclyptor/Factorio) ·
[Palworld](https://github.com/Reclyptor/Palworld) · [Minecraft](https://github.com/Reclyptor/Minecraft) ·
[Terraria](https://github.com/Reclyptor/Terraria) · [Core Keeper](https://github.com/Reclyptor/CoreKeeper)

**[`docs/CONTRACT.md`](docs/CONTRACT.md) is the interface** — what the toolkit provides, what an
adapter implements, the environment every game image shares, and exactly how the toolkit degrades
for games that lack a save command, a broadcast, or any control channel at all. **[`SPEC.md`](SPEC.md)**
records why it exists and the decisions behind it.

## What a game gets

| | |
|---|---|
| **Lifecycle** | `gameops run` is PID 1: it reaps orphans, forwards signals, launches the server, watches for readiness, and stops it gracefully on `SIGTERM` with a save first. Crashes propagate the server's exit code so the orchestrator sees them. |
| **Backups** | `BACKUP_CRON` → save → archive of the adapter's paths → **read back and verified** → atomic rename → prune older than `BACKUP_RETAIN_DAYS`. Also `gameops backup` by hand; `gameops backup list` and `gameops backup verify latest` to check on them. Backups, updates and restores take turns on one lock — a backup is never skipped, and one that fails for any reason posts `BACKUP_FAILED`. |
| **Restore** | `gameops restore latest` (or an archive name): verify → warn players → safety backup → graceful stop → staged swap → relaunch in the same container. A failed swap leaves the world untouched. |
| **Updates** | `UPDATE_CRON` → check → warn players in-game at 15/10/5/2/1 min and 30/10 s (re-checking whether anyone is still online) → backup → stop → apply → **relaunch in place**. The container never exits for an update; a failed install relaunches the installed version, posts `UPDATE_FAILED`, and is not retried for that build. `UPDATE_ON_BOOT` covers the first start. |
| **Notifications** | Discord webhook for start, stop, crash, update, backup, join and leave — plain text by default, embed cards with `DISCORD_EMBEDS=true`, every message overridable. |
| **Player events** | The adapter streams `JOIN`/`LEAVE`; the toolkit keeps the online set, which is also the player count for games with no query API. |
| **Console** | A FIFO is attached to the server's stdin. `gameops console <line>` types at it from anywhere in the container — the only control channel some games have. |
| **Health** | `gameops health`: process alive, ready, port open. Wire it as `HEALTHCHECK`. The same check answers `GET /healthz`. |
| **Metrics** | `GET /metrics` on `METRICS_PORT` (9110): up/ready, players online, last backup age and size, pending update, counters for backups, updates, restores, restarts and failed notifications. |
| **Helpers** | RCON, HTTPS, JSON get/set, SteamCMD install and update detection — available to adapters as bash functions, so game images need no extra tools. |

## Configuration

Common to every game image. Game-specific variables are documented in each game's README.

| Variable | Default | Meaning |
|---|---|---|
| `DATA_DIR` | `/data` | Persistent game data |
| `BACKUP_DIR` | `/backups` | Archive destination (any mount) |
| `SERVER_NAME` | game name | Used in notifications, and as the in-game server name where the game has one |
| `BACKUP_ENABLED` / `BACKUP_CRON` | `true` / `0 4 * * *` | Schedule |
| `BACKUP_RETAIN_DAYS` | `14` | `0` keeps everything |
| `BACKUP_ON_UPDATE` | `true` | Back up before applying an update |
| `UPDATE_ENABLED` / `UPDATE_CRON` | `true` / `0 * * * *` | Schedule (games that support updates) |
| `UPDATE_ON_BOOT` | `true` | Check and apply before the first launch |
| `UPDATE_WARN_MINUTES` | `15` | In-game countdown when players are online |
| `UPDATE_SKIP_IF_PLAYERS` | `false` | Defer instead of counting down |
| `UPDATE_FORCE_AFTER_MINUTES` | `30` | For games without broadcast: wait this long for an empty server |
| `UPDATE_APPLY_TIMEOUT` | `3600` | Hard cap on an update install; a hung install is killed and the old version relaunched |
| `STALL_SECONDS` | `300` | SteamCMD is killed and retried after this long without output |
| `STOP_TIMEOUT` | `120` | Seconds between the graceful stop and `SIGKILL` |
| `READY_TIMEOUT` | `900` | Seconds to wait for the server to become joinable |
| `DISCORD_SILENT_EVENTS` | `BACKUP_PRE,BACKUP_POST` | Events posted without pinging; `ALL` for every event, `!EVENT` to exempt one (`ALL,!JOIN,!LEAVE`) |
| `DISCORD_DISABLED_EVENTS` | — | Events not sent at all, same `ALL`/`!EVENT` vocabulary: `ALL,!JOIN,!LEAVE` posts joins and leaves and nothing else. |
| `LOCK_TIMEOUT` | derived | Seconds a backup, update or restore waits its turn on the job lock before failing loudly; defaults to outlasting any update or restore in progress |
| `PLAYER_EVENTS_ENABLED` | `true` | Join/leave tracking and notifications |
| `METRICS_PORT` | `9110` | `/metrics` and `/healthz`; `0` disables |
| `DISCORD_WEBHOOK_URL` | — | Empty disables notifications |
| `DISCORD_SUPPRESS_NOTIFICATIONS` | `false` | Send every post silently (no pings) |
| `DISCORD_SILENT_EVENTS` | `BACKUP_PRE,BACKUP_POST` | Events that never ping |
| `DISCORD_EMBEDS` | `false` | Embed cards instead of plain text |
| `DISCORD_<EVENT>_ENABLED` / `_MESSAGE` | see contract | Per-event switch and text |
| `SERVER_ADDRESS` | — | Where players connect. Every START message carries the address and join password actually in force — these values, or whatever the adapter reports instead (an address a client can really use, a password the game made up) — so the channel always has what players need |
| `LOG_LEVEL` | `info` | `debug` `info` `warn` `error` |
| `TZ` | `UTC` | Schedules and timestamps |

The full table, defaults and placeholder words are in [`docs/CONTRACT.md` §7](docs/CONTRACT.md#7-environment-contract).

## Adding a game

Implement the adapter — a handful of bash functions: how to install, start, know it's ready,
save, stop, list paths to back up, and (optionally) broadcast, count players, detect and apply
updates, and stream join/leave events. Anything the game cannot do returns `2` and the toolkit
degrades deliberately. The checklist is [`docs/CONTRACT.md` §9](docs/CONTRACT.md#9-adding-a-game--checklist);
`tests/fake-game/adapter.sh` is a complete example that exercises every hook.

## Development

```sh
go test ./...     # unit tests
tests/smoke.sh    # end-to-end: start → join/leave → backup → update in place → SIGTERM → crash
```

Nothing but Go and `docker` is needed locally. CI runs the same, plus gofmt, vet and shellcheck,
before publishing to `ghcr.io/reclyptor/gameops` — `latest` and a sortable timestamp tag on every
`master` push, and semver (`1.0.0`, `1.0`, `1`) on `v*` tags.

## License

MIT.
