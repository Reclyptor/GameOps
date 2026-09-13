#!/usr/bin/env bash
# A stand-in "game server" for the smoke test: talks on stdin/stdout like a
# console-driven game, saves a world file, reports joins and leaves, and
# stops cleanly on `stop` or SIGTERM.
set -uo pipefail

world="${DATA_DIR}/world/world.dat"
mkdir -p "$(dirname "$world")"
[[ -f "$world" ]] || printf 'created %s\n' "$(date +%s)" > "$world"

save() {
    printf 'saved %s\n' "$(date +%s)" >> "$world"
    echo "world saved"
}

on_term() {
    echo "SIGTERM received; saving and exiting"
    save
    exit 0
}
trap on_term TERM INT

echo "fake server $(cat /opt/fake/VERSION) booting"
sleep 1
echo "READY"

while IFS= read -r line; do
    case "$line" in
        save)        save ;;
        say\ *)      echo "[broadcast] ${line#say }" ;;
        fakejoin\ *) echo "[JOIN] ${line#fakejoin } joined" ;;
        fakeleave\ *) echo "[LEAVE] ${line#fakeleave } left" ;;
        crash)       echo "crashing on request"; exit 3 ;;
        stop)        echo "stopping"; save; exit 0 ;;
        *)           echo "unknown command: ${line}" ;;
    esac
done
