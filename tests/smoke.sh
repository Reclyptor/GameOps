#!/usr/bin/env bash
# End-to-end smoke test against the fake game: start → ready → JOIN/LEAVE →
# backup → verify → restore-in-place → jobs waiting on the job lock →
# update-in-place → SIGTERM, plus the connection details START announces
# (configured and server-reported address and password), per-event silencing,
# and a crash exit, with /metrics and /healthz checked along the way. Webhook bodies go to
# a tiny HTTP receiver running in its own container on a private network, so
# real curl and real HTTP are exercised and nothing depends on the host.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${GAMEOPS_IMAGE:=gameops:test}"
fake=gameops-fake:test
runner=gameops-test-runner
name=gameops-smoke-$$
net="${name}-net"
hooks_ctr="${name}-hooks"
hook_port=18089
work=$(mktemp -d)
hooks="$work/webhooks.log"

cleanup() {
    docker rm -f "$name" "${name}-crash" "${name}-pw" "$hooks_ctr" >/dev/null 2>&1 || true
    docker network rm "$net" >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT

fail() {
    echo "SMOKE FAIL: $*" >&2
    echo "--- container log ---" >&2; docker logs "$name" 2>&1 | tail -150 >&2 || true
    echo "--- webhooks ---" >&2; sync_hooks; cat "$hooks" >&2
    exit 1
}
step() { echo "==> $*"; }

# The receiver prints one JSON body per line; mirror its log into $hooks.
sync_hooks() { docker logs "$hooks_ctr" 2>/dev/null > "$hooks" || true; }

wait_for() {
    local what=$1 pattern=$2 timeout=${3:-60} i
    for (( i = 0; i < timeout; i++ )); do
        sync_hooks
        grep -qE "$pattern" "$hooks" && return 0
        sleep 1
    done
    fail "timed out waiting for ${what}: /${pattern}/"
}
# wait_for_log <pattern> [timeout] [occurrences]
wait_for_log() {
    local pattern=$1 timeout=${2:-60} want=${3:-1} i
    for (( i = 0; i < timeout; i++ )); do
        (( $(docker logs "$name" 2>&1 | grep -cE "$pattern") >= want )) && return 0
        sleep 1
    done
    fail "timed out waiting for ${want}x log: /${pattern}/"
}
wait_for_exit() {
    local timeout=${1:-30} i
    for (( i = 0; i < timeout; i++ )); do
        [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == false ]] && return 0
        sleep 1
    done
    fail "container did not exit within ${timeout}s"
}

step "build images"
[[ -n "$(docker images -q "$GAMEOPS_IMAGE")" ]] || docker build -q -t "$GAMEOPS_IMAGE" . >/dev/null
docker build -q -t "$fake" --build-arg "GAMEOPS_IMAGE=${GAMEOPS_IMAGE}" tests/fake-game >/dev/null
docker build -q -t "$runner" -f tests/runner.Dockerfile tests >/dev/null

step "start webhook receiver"
docker network create "$net" >/dev/null
docker run -d --name "$hooks_ctr" --network "$net" "$runner" python3 -u -c "
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0))).decode()
        print(body, flush=True)
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', ${hook_port}), H).serve_forever()
" >/dev/null
sleep 1

# Every job below is run by hand; the schedules are off so a run that crosses
# the hour cannot slip a cron update in between the steps.
common_env=(
    --network "$net"
    -e BACKUP_ENABLED=false -e UPDATE_ENABLED=false
    -e "DISCORD_WEBHOOK_URL=http://${hooks_ctr}:${hook_port}/hook"
    -e SERVER_NAME=Smoke -e UPDATE_ON_BOOT=false -e READY_TIMEOUT=60 -e STOP_TIMEOUT=15
    -e BACKUP_RETAIN_DAYS=0 -e UPDATE_WARN_MINUTES=1 -e UPDATE_APPLY_TIMEOUT=5 -e LOG_LEVEL=debug
)

step "run the fake game"
docker run -d --name "$name" "${common_env[@]}" "$fake" >/dev/null
wait_for "START notification" '🟢 Smoke server is online' 60
health=$(docker inspect -f '{{.State.Health.Status}}' "$name")
if [[ "$health" != healthy ]]; then
    sleep 8
    health=$(docker inspect -f '{{.State.Health.Status}}' "$name")
    [[ "$health" == healthy ]] || fail "container health is '${health}'"
fi

# metric <name> → value line of that metric from /metrics inside the container
metric() { docker exec "$name" gameops http get http://127.0.0.1:9110/metrics | grep -E "^gameops_$1(\{[^}]*\})? " | sed 's/.* //'; }

step "metrics and healthz"
[[ "$(docker exec "$name" gameops http get http://127.0.0.1:9110/healthz)" == ok ]] || fail "/healthz did not answer ok"
[[ "$(metric server_up)" == 1 && "$(metric server_ready)" == 1 ]] || fail "server_up/server_ready not 1"
[[ "$(metric players_online)" == 0 ]] || fail "players_online should be 0"
[[ "$(metric backup_archives)" == 0 && "$(metric backups_total)" == 0 ]] || fail "backup metrics should start at 0"
docker exec "$name" gameops http get http://127.0.0.1:9110/metrics | grep -q 'gameops_info{game="fake",server_version="1.0.0",toolkit_version="' || fail "gameops_info missing"

step "player events"
docker exec "$name" gameops console fakejoin Alice
wait_for "JOIN notification" '\*\*Alice\*\* joined the Smoke server' 20
[[ "$(docker exec "$name" cat /tmp/gameops/players)" == "Alice" ]] || fail "player set should contain Alice"
[[ "$(metric players_online)" == 1 ]] || fail "players_online should be 1 while Alice is on"
docker exec "$name" gameops console fakeleave Alice
wait_for "LEAVE notification" '\*\*Alice\*\* left the Smoke server' 20
[[ -z "$(docker exec "$name" cat /tmp/gameops/players)" ]] || fail "player set should be empty"

step "backup"
backup_out=$(docker exec "$name" gameops backup 2>&1)
wait_for "BACKUP_POST notification" 'Manual backup of the Smoke server complete: /backups/fake-' 20
archive=$(docker exec "$name" sh -c 'ls /backups/fake-*.tar.gz')
docker exec "$name" tar -tzf "$archive" | grep -q 'world/world.dat' || fail "archive lacks world/world.dat"
docker exec "$name" grep -q '^saved ' /data/world/world.dat || fail "game_save was not honoured before archiving"
grep -qE 'backup verified: [0-9]+ files' <<< "$backup_out" || fail "archive was not verified on write"
sync_hooks; grep -q '"content":"💾 Manual backup of the Smoke server…","flags":4096' "$hooks" || fail "backup posts must be silent (flags 4096)"
grep -q '"content":"🟢 Smoke server is online.","flags":0' "$hooks" || fail "START must not be silent by default"
docker exec "$name" gameops backup verify latest | grep -q '^OK: /backups/fake-' || fail "backup verify did not pass"
[[ "$(docker exec "$name" gameops backup list | wc -l)" == 1 ]] || fail "backup list should show one archive"
[[ "$(metric backups_total)" == 1 && "$(metric backup_archives)" == 1 ]] || fail "backup metrics not updated"
[[ "$(metric backup_last_size_bytes)" -gt 0 ]] || fail "backup_last_size_bytes should be set"

step "restore in place"
docker exec "$name" sh -c 'echo tampered >> /data/world/world.dat'
docker exec "$name" gameops restore latest
wait_for "pre-restore backup" 'Pre-restore backup of the Smoke server complete: /backups/fake-' 60
wait_for "RESTORE_POST notification" 'Smoke server restored from /backups/fake-' 60
wait_for_log 'Smoke is ready' 30 2
[[ "$(docker inspect -f '{{.RestartCount}}' "$name")" == 0 ]] || fail "container restarted during restore"
docker exec "$name" grep -c tampered /data/world/world.dat >/dev/null && fail "world.dat still carries the tampered line after restore"
[[ "$(docker exec "$name" gameops backup list | wc -l)" == 2 ]] || fail "restore should have taken a safety backup first"
docker exec "$name" test ! -e /data/.restore-staging || fail "staging dir left behind"
[[ "$(metric restores_total)" == 1 && "$(metric server_restarts_total)" == 1 ]] || fail "restore counters not updated"

step "failed update relaunches the installed version and is not retried"
docker exec "$name" sh -c 'echo fail > /data/next-version'
docker exec "$name" gameops update
wait_for "UPDATE_FAILED notification" 'Update of the Smoke server to fail failed' 60
wait_for_log 'Smoke is ready' 30 3
[[ "$(docker inspect -f '{{.RestartCount}}' "$name")" == 0 && "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || fail "container should still be running after a failed update"
[[ "$(docker exec "$name" cat /opt/fake/VERSION)" == 1.0.0 ]] || fail "installed version must be untouched"
docker exec "$name" gameops update 2>&1 | grep -q "failed earlier; not retrying" || fail "the same failing target must not be retried"
[[ "$(docker exec "$name" gameops backup list | wc -l)" == 3 ]] || fail "the retry must not take another backup"

step "hung update is killed at UPDATE_APPLY_TIMEOUT and the server comes back"
docker exec "$name" sh -c 'echo hang > /data/next-version'
docker exec "$name" gameops update
# The runner holds the job lock from the apply until the relaunched server is
# ready, so a backup asked for now waits all of that out and then saves a live
# server; it never archives one that is half way through a relaunch.
wait_for_log 'applying update to hang' 30
docker exec -d "$name" sh -c 'gameops backup > /tmp/queued-backup.log 2>&1; echo $? > /tmp/queued-backup.rc.tmp && mv /tmp/queued-backup.rc.tmp /tmp/queued-backup.rc'
sleep 2
docker exec "$name" test ! -e /tmp/queued-backup.rc || fail "a backup ran while the runner was applying an update"
wait_for "UPDATE_FAILED notification" 'Update of the Smoke server to hang failed: install still running after 5s, killed' 90
wait_for_log 'Smoke is ready' 60 4
[[ "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || fail "container not running after a hung update"
[[ "$(docker exec "$name" cat /opt/fake/VERSION)" == 1.0.0 ]] || fail "installed version must be untouched after a hung update"
for (( i = 0; i < 30; i++ )); do docker exec "$name" test -e /tmp/queued-backup.rc && break; sleep 1; done
[[ "$(docker exec "$name" cat /tmp/queued-backup.rc 2>/dev/null)" == 0 ]] || fail "the queued backup did not succeed: $(docker exec "$name" cat /tmp/queued-backup.log)"
docker exec "$name" grep -q 'holds the job lock; waiting' /tmp/queued-backup.log || fail "the queued backup should have waited for the relaunch"
docker exec "$name" grep -q 'world saved' /tmp/queued-backup.log || fail "the queued backup ran before the relaunched server was up"

# The nightly backup and an hourly update check start in the same second; the
# one that loses the race must wait its turn, not vanish.
step "a job waits its turn behind a held lock instead of being skipped"
docker exec -d "$name" flock /tmp/gameops/lock sleep 6
sleep 1
before=$(docker exec "$name" gameops backup list | wc -l)
out=$(docker exec "$name" gameops backup 2>&1) || fail "a backup behind a held lock must run once the lock frees: ${out}"
grep -q 'holds the job lock; waiting' <<< "$out" || fail "the backup should say it is waiting for the lock"
grep -q 'skipping' <<< "$out" && fail "a backup must never be skipped"
[[ "$(docker exec "$name" gameops backup list | wc -l)" == $(( before + 1 )) ]] || fail "the waiting backup did not write an archive"

step "a job that never gets its turn fails loudly"
docker exec -d "$name" flock /tmp/gameops/lock sleep 5
sleep 1
if docker exec -e LOCK_TIMEOUT=1 "$name" gameops backup > "$work/lock-timeout.log" 2>&1; then
    fail "a backup that never got the lock must exit non-zero"
fi
grep -q 'timed out waiting for the job lock after 1s' "$work/lock-timeout.log" || fail "the timeout was not reported: $(cat "$work/lock-timeout.log")"
wait_for "BACKUP_FAILED notification" 'Manual backup of the Smoke server failed: not started: timed out waiting for the job lock' 20
[[ "$(metric backup_failures_total)" == 1 ]] || fail "backup_failures_total should count the missed backup"

step "update in place"
docker exec "$name" sh -c 'echo 1.1.0 > /data/next-version'
docker exec "$name" gameops update
wait_for "pre-update backup" 'Pre-update backup of the Smoke server complete: /backups/fake-' 60
wait_for "UPDATE_POST notification" 'Smoke server updated to 1.1.0' 60
wait_for_log 'fake server 1.1.0 booting' 30
wait_for_log 'Smoke is ready' 30 5
[[ "$(docker inspect -f '{{.RestartCount}}' "$name")" == 0 ]] || fail "container restarted during update"
[[ "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || fail "container not running after update"
[[ "$(docker exec "$name" cat /opt/fake/VERSION)" == 1.1.0 ]] || fail "version file not updated"
[[ "$(metric updates_total)" == 1 && "$(metric update_pending)" == 0 && "$(metric server_restarts_total)" == 4 ]] || fail "update counters not updated"
docker exec "$name" gameops http get http://127.0.0.1:9110/metrics | grep -q 'server_version="1.1.0"' || fail "server_version not refreshed after the update"

step "drain warns players, blocks new jobs, and returns once the server empties"
# The grace period a manifest needs is derived by the toolkit, never written
# down twice: 300 drain + STOP_TIMEOUT (15 here) + 60 margin.
grace=$(docker exec "$name" gameops drain --required-grace --deadline 300)
[[ "$grace" == 375 ]] || fail "drain --required-grace should be 375 with STOP_TIMEOUT=15, got ${grace}"
# Nobody online: a drain must not wait at all.
docker exec "$name" gameops drain --deadline 120 || fail "a drain on an empty server must exit 0 at once"
# With a player online it counts down in-game rather than dropping them.
docker exec "$name" gameops console fakejoin Alice
for (( i = 0; i < 15; i++ )); do [[ "$(docker exec "$name" gameops players)" == 1 ]] && break; sleep 1; done
[[ "$(docker exec "$name" gameops players)" == 1 ]] || fail "fakejoin did not register a player"
docker exec -d "$name" sh -c 'gameops drain --deadline 120 > /tmp/drain.log 2>&1; echo $? > /tmp/drain.rc.tmp && mv /tmp/drain.rc.tmp /tmp/drain.rc'
wait_for_log '\[broadcast\] Server restarting for maintenance in 2 minutes' 20
# No new job may start behind a drain. (One already running keeps the lock and
# is left to finish; the stop path waits for it.)
if out=$(docker exec "$name" gameops backup 2>&1); then
    fail "a backup must not start during a drain: ${out}"
fi
grep -q "stopping or draining" <<<"$out" || fail "a backup blocked by a drain should say why: ${out}"
# The player leaves; the drain must notice and return inside its budget.
docker exec "$name" gameops console fakeleave Alice
for (( i = 0; i < 90; i++ )); do
    docker exec "$name" test -f /tmp/drain.rc 2>/dev/null && break
    sleep 1
done
docker exec "$name" test -f /tmp/drain.rc 2>/dev/null || fail "the drain did not return after the server emptied"
drain_rc=$(docker exec "$name" cat /tmp/drain.rc)
[[ "$drain_rc" == 0 ]] || fail "drain exited ${drain_rc}: $(docker exec "$name" cat /tmp/drain.log)"
# ...and the flag must be gone, or the scheduler would stay silenced for good.
docker exec "$name" gameops backup >/dev/null 2>&1 || fail "a backup must run again once the drain is over"

step "graceful stop on SIGTERM"
docker stop -t 30 "$name" >/dev/null
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" == 0 ]] || fail "expected exit 0 after SIGTERM, got $(docker inspect -f '{{.State.ExitCode}}' "$name")"
wait_for "STOP notification" '💤 Smoke server has shut down' 10
docker logs "$name" 2>&1 | grep -c '^stopping$' >/dev/null || fail "game_shutdown console command was not delivered"

# The START message announces the password the server actually enforces:
# the configured one, or the one it made up when it was given none.
step "START announces a configured join password"
docker run -d --name "${name}-pw" "${common_env[@]}" -e SERVER_NAME=Configured -e GAME_PASSWORD=smoke-pass "$fake" >/dev/null
wait_for "START with the configured password" "Configured server is online[^\"]*\\(password: \`smoke-pass\`\\)" 60
docker rm -f "${name}-pw" >/dev/null

step "START announces a join password the server made up"
docker run -d --name "${name}-pw" "${common_env[@]}" -e SERVER_NAME=Generated -e FAKE_GENERATE_PASSWORD=true "$fake" >/dev/null
wait_for "START with a generated password" "Generated server is online[^\"]*\\(password: \`gen-[0-9]+\`\\)" 60
generated=$(docker exec "${name}-pw" cat /data/join-password)
sync_hooks; grep -qF "(password: \`${generated}\`)" "$hooks" || fail "START did not announce the password the server generated (${generated})"
docker rm -f "${name}-pw" >/dev/null

step "START announces the address the server reports, not the configured one"
docker run -d --name "${name}-pw" "${common_env[@]}" -e SERVER_NAME=Reported -e SERVER_ADDRESS=configured.example:1111 -e FAKE_ANNOUNCE_ADDRESS=203.0.113.7:2222 "$fake" >/dev/null
wait_for "START with the reported address" "Reported server is online[^\"]*connect to \`203.0.113.7:2222\`" 60
sync_hooks
grep -q 'configured.example' "$hooks" && fail "the configured address must not be announced when the server reports one"
docker rm -f "${name}-pw" >/dev/null

# ALL covers every event, including ones it was never told about; !EVENT keeps
# the ones players care about loud.
step "ALL silences every event and !EVENT keeps one loud"
docker run -d --name "${name}-pw" "${common_env[@]}" -e SERVER_NAME=Quiet -e "DISCORD_SILENT_EVENTS=ALL,!JOIN" "$fake" >/dev/null
wait_for "a silent START" '"content":"🟢 Quiet server is online.","flags":4096' 60
docker exec "${name}-pw" gameops console fakejoin Zoe
wait_for "a loud JOIN" '"content":"🟢 \*\*Zoe\*\* joined the Quiet server.","flags":0' 30
docker rm -f "${name}-pw" >/dev/null

step "crash propagates the exit code"
name_main=$name
name="${name}-crash"
docker run -d --name "$name" "${common_env[@]}" "$fake" >/dev/null
wait_for_log 'Smoke is ready' 60
docker exec "$name" gameops console crash
wait_for_exit 30
code=$(docker inspect -f '{{.State.ExitCode}}' "$name")
[[ "$code" == 3 ]] || fail "expected the server's exit code 3, got ${code}"
wait_for "CRASH notification" 'stopped unexpectedly \(exit code 3\)' 10
name=$name_main

echo "SMOKE OK"
