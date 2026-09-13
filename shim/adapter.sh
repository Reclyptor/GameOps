#!/usr/bin/env bash
# The helper library every adapter sees (docs/CONTRACT.md §6). Sourced by
# shim/adapter-exec before the adapter itself. Anything non-trivial is a
# call into the gameops binary; the rest is plain bash.
# shellcheck shell=bash

: "${GAMEOPS_HOME:=/opt/gameops}"
: "${GAMEOPS_STATE:=/tmp/gameops}"
: "${GAME_ADAPTER:=/opt/game/adapter.sh}"
: "${DATA_DIR:=/data}"
: "${BACKUP_DIR:=/backups}"
: "${GAME_LOG:=${DATA_DIR}/logs/console.log}"
: "${LOG_LEVEL:=info}"
GAMEOPS_BIN="${GAMEOPS_HOME}/bin/gameops"

# ── logging (stderr; stdout is for values) ──────────────────────────────────
_log_level_num() {
    case "${1,,}" in debug) echo 0 ;; info) echo 1 ;; warn|warning) echo 2 ;; error) echo 3 ;; *) echo 1 ;; esac
}
_log() {
    local level=$1; shift
    (( $(_log_level_num "$level") < $(_log_level_num "$LOG_LEVEL") )) && return 0
    printf '%s [gameops] %-5s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "${level^^}" "$*" >&2
}
log_debug()  { _log debug "$@"; }
log_info()   { _log info  "$@"; }
log_warn()   { _log warn  "$@"; }
log_error()  { _log error "$@"; }
log_action() { _log info  "==> $*"; }
die()        { log_error "$@"; exit 1; }

# ── small helpers ───────────────────────────────────────────────────────────
is_true() { case "${1,,}" in true|1|yes|on) return 0 ;; *) return 1 ;; esac; }
is_int()  { [[ "$1" =~ ^[0-9]+$ ]]; }
require_var() {
    local name=$1 why=${2:-}
    [[ -n "${!name:-}" ]] || die "${name} must be set${why:+: $why}"
}

# ── runtime state (files under GAMEOPS_STATE, shared with the binary) ───────
state_file() { printf '%s/%s' "$GAMEOPS_STATE" "$1"; }
server_pid() {
    local f pid
    f=$(state_file server.pid)
    [[ -r "$f" ]] || return 1
    pid=$(<"$f")
    [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || return 1
    printf '%s' "$pid"
}
flag_set()    { printf '%s' "${2:-1}" > "$(state_file "$1")"; }
flag_get()    { local f; f=$(state_file "$1"); [[ -r "$f" ]] && cat "$f"; return 0; }
flag_clear()  { rm -f "$(state_file "$1")"; }
flag_is_set() { [[ -e "$(state_file "$1")" ]]; }
players_count() { "$GAMEOPS_BIN" players; }

# ── control channels ────────────────────────────────────────────────────────
console_send() { "$GAMEOPS_BIN" console "$@"; }
rcon()         { "$GAMEOPS_BIN" rcon "$@"; }
tcp_port_open() { "$GAMEOPS_BIN" tcp-open "$1" "$2"; }

# ── HTTP and JSON ───────────────────────────────────────────────────────────
http_get()   { "$GAMEOPS_BIN" http get "$@"; }
json_get()   { "$GAMEOPS_BIN" json get "$@"; }      # json_get <file|-> <path>
json_set()   { "$GAMEOPS_BIN" json set "$@"; }      # json_set <file> <path> <value> [--raw]
json_escape() { "$GAMEOPS_BIN" json escape "$@"; }
json_array() { "$GAMEOPS_BIN" json array "$@"; }

# ── files and processes ─────────────────────────────────────────────────────
# Follow a log from its current end, exiting when the server exits.
follow_log() {
    local path=${1:-$GAME_LOG} pid
    [[ -e "$path" ]] || touch "$path" 2>/dev/null || true
    if pid=$(server_pid); then
        tail -n0 -F --pid="$pid" "$path" 2>/dev/null
    else
        tail -n0 -F "$path" 2>/dev/null
    fi
}
wait_settled() { "$GAMEOPS_BIN" wait-settled "$@"; }   # <path> [quiet] [timeout]
notify()       { "$GAMEOPS_BIN" notify "$@"; }         # <EVENT> [key=value ...]

# ── Steam ───────────────────────────────────────────────────────────────────
steam_install()          { "$GAMEOPS_BIN" steam install "$@"; }        # <dir> <appid...>
steam_update_available() { "$GAMEOPS_BIN" steam update-check "$@"; }   # <dir> <appid> <depot>

# ── optional contract functions: defaults return 2 (unsupported) ────────────
__default_game_update_available() { return 2; }
__default_game_update_apply()     { return 2; }
__default_game_save()             { return 2; }
__default_game_broadcast()        { return 2; }
__default_game_players()          { return 2; }
__default_game_join_password()    { return 2; }
__default_game_server_address()   { return 2; }
__default_game_events()           { return 2; }
__default_game_backup_begin()     { return 0; }
__default_game_backup_end()       { return 0; }
__default_game_healthy()          { return 2; }
__default_game_shutdown()         { return 2; }

game_update_available() { __default_game_update_available "$@"; }
game_update_apply()     { __default_game_update_apply "$@"; }
game_save()             { __default_game_save "$@"; }
game_broadcast()        { __default_game_broadcast "$@"; }
game_players()          { __default_game_players "$@"; }
game_join_password()    { __default_game_join_password "$@"; }
game_server_address()   { __default_game_server_address "$@"; }
game_events()           { __default_game_events "$@"; }
game_backup_begin()     { __default_game_backup_begin "$@"; }
game_backup_end()       { __default_game_backup_end "$@"; }
game_healthy()          { __default_game_healthy "$@"; }
game_shutdown()         { __default_game_shutdown "$@"; }

# True when the adapter overrode the named optional function.
adapter_supports() {
    local fn=$1
    declare -F "$fn" >/dev/null 2>&1 || return 1
    [[ "$(declare -f "$fn")" != *"__default_${fn}"* ]]
}

# Source the adapter and settle the variables every caller relies on. This is
# the one way to load an adapter — adapter-exec uses it, and so should anyone
# poking at a server by hand:
#   docker exec <ctr> bash -c 'source /opt/gameops/shim/adapter.sh; adapter_load; game_players'
adapter_load() {
    [[ -r "$GAME_ADAPTER" ]] || die "adapter not found or unreadable: ${GAME_ADAPTER}"
    # shellcheck disable=SC1090
    source "$GAME_ADAPTER"
    : "${GAME_PORT:=}"
    : "${GAME_PORT_PROTO:=tcp}"
    : "${SERVER_NAME:=${GAME_NAME:-}}"
    export GAME_NAME GAME_DIR GAME_PORT GAME_PORT_PROTO GAME_LOG SERVER_NAME
}
