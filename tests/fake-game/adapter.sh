#!/usr/bin/env bash
# Adapter for the fake game. Exercises every optional capability so the smoke
# test covers the whole contract; a real adapter is usually smaller.
# shellcheck shell=bash
# shellcheck disable=SC2034  # GAME_* and GAME_CMD are consumed by the toolkit

GAME_NAME=fake
GAME_DIR=/opt/fake

game_install() {
    [[ -f "${GAME_DIR}/VERSION" ]] || echo "1.0.0" > "${GAME_DIR}/VERSION"
}

game_version() { cat "${GAME_DIR}/VERSION"; }

# The "update source" is a file the test writes.
game_update_available() {
    local next="${DATA_DIR}/next-version"
    [[ -r "$next" ]] || return 1
    local target
    target=$(<"$next")
    [[ "$target" != "$(game_version)" ]] || return 1
    printf '%s' "$target"
}

game_update_apply() {
    [[ "$1" != fail ]] || { echo "refusing to install 'fail'" >&2; return 1; }
    [[ "$1" != hang ]] || { echo "installing 'hang' forever" >&2; sleep 600; }
    echo "$1" > "${GAME_DIR}/VERSION"
}

game_start_cmd() {
    GAME_CMD=(bash /opt/fake/server.sh)
}

game_ready() { grep -q '^READY$' "$GAME_LOG"; }

game_save()      { console_send save; }
game_broadcast() { console_send "say $*"; }
game_shutdown()  { console_send stop; }

game_backup_paths() { echo world; }

game_events() {
    follow_log | sed -un \
        -e 's/^\[JOIN\] \(.*\) joined$/JOIN \1/p' \
        -e 's/^\[LEAVE\] \(.*\) left$/LEAVE \1/p'
}
