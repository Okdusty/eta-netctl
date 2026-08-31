#!/system/bin/sh

ETA_ROOT=/data/adb/eta-net
STATE_DIR="$ETA_ROOT/state"
SUPERVISOR_PID_FILE="$ETA_ROOT/supervisor.pid"
HEARTBEAT_FILE="$ETA_ROOT/supervisor.heartbeat"
ARGS_FILE="$ETA_ROOT/args"
LOG_FILE="$ETA_ROOT/eta-net.log"
PREVIOUS_LOG="$ETA_ROOT/eta-net.previous.log"
SUPERVISOR_LOG="$ETA_ROOT/supervisor.log"
DISABLE_FILE="$ETA_ROOT/disabled"
TERMUX_PREFIX=/data/data/com.termux/files/usr
ETA_NET="$TERMUX_PREFIX/bin/eta-net"
SERVICE_SCRIPT=/data/adb/service.d/eta-net.sh

pid_matches() {
    eta_check_pid=$1
    eta_check_text=$2
    case "$eta_check_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    [ -r "/proc/$eta_check_pid/cmdline" ] && \
        grep -aFq "$eta_check_text" "/proc/$eta_check_pid/cmdline"
}

write_heartbeat() {
    eta_heartbeat_tmp="${HEARTBEAT_FILE}.tmp.$$"
    date +%s >"$eta_heartbeat_tmp" && mv -f "$eta_heartbeat_tmp" "$HEARTBEAT_FILE"
}

if [ -r "$SUPERVISOR_PID_FILE" ]; then
    eta_old_pid=$(cat "$SUPERVISOR_PID_FILE" 2>/dev/null)
    if pid_matches "$eta_old_pid" "$SERVICE_SCRIPT"; then
        exit 0
    fi
fi

mkdir -p "$ETA_ROOT" "$STATE_DIR"
touch "$LOG_FILE" "$SUPERVISOR_LOG"
chmod 0600 "$LOG_FILE" "$SUPERVISOR_LOG"
trap '' HUP

supervise_eta_net() {
    while :; do
        write_heartbeat
        if [ -e "$DISABLE_FILE" ]; then
            sleep 2
            continue
        fi

        eta_wait=0
        while [ "$(getprop sys.boot_completed)" != 1 ] && [ "$eta_wait" -lt 180 ]; do
            write_heartbeat
            sleep 1
            eta_wait=$((eta_wait + 1))
        done
        /system/bin/svc wifi disable >/dev/null 2>&1 || true

        eta_wait=0
        while [ "$eta_wait" -lt 180 ]; do
            write_heartbeat
            if ip -4 route show table all 2>/dev/null | grep -Eq 'default.*dev rmnet[0-9]'; then
                break
            fi
            [ -e "$DISABLE_FILE" ] && break
            sleep 1
            eta_wait=$((eta_wait + 1))
        done
        if [ -e "$DISABLE_FILE" ] || [ "$eta_wait" -ge 180 ]; then
            sleep 2
            continue
        fi

        set -- --transport direct --all-apps --no-restart-apps
        if [ -s "$ARGS_FILE" ]; then
            set --
            while IFS= read -r eta_arg || [ -n "$eta_arg" ]; do
                set -- "$@" "$eta_arg"
            done <"$ARGS_FILE"
        fi

        printf '%s eta-net start\n' "$(date '+%Y-%m-%d %H:%M:%S')" >>"$SUPERVISOR_LOG"
        if [ -s "$LOG_FILE" ]; then
            mv -f "$LOG_FILE" "$PREVIOUS_LOG"
        fi
        : >"$LOG_FILE"
        chmod 0600 "$LOG_FILE"
        printf '%s eta-net session start\n' "$(date '+%Y-%m-%d %H:%M:%S')" >>"$LOG_FILE"
        ETA_STATE_DIR="$STATE_DIR" "$TERMUX_PREFIX/bin/bash" "$ETA_NET" "$@" \
            </dev/null >>"$LOG_FILE" 2>&1 &
        eta_child_pid=$!
        eta_stop_sent=0
        while kill -0 "$eta_child_pid" 2>/dev/null; do
            write_heartbeat
            if [ -e "$DISABLE_FILE" ] && [ "$eta_stop_sent" -eq 0 ]; then
                kill -TERM "$eta_child_pid" 2>/dev/null || true
                eta_stop_sent=1
            fi
            sleep 2
        done
        wait "$eta_child_pid"
        eta_status=$?
        printf '%s eta-net session exit=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$eta_status" >>"$LOG_FILE"
        printf '%s eta-net exit=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$eta_status" >>"$SUPERVISOR_LOG"
        sleep 5
    done
}

supervise_eta_net </dev/null >>"$SUPERVISOR_LOG" 2>&1 &
eta_supervisor_pid=$!
printf '%s\n' "$eta_supervisor_pid" >"$SUPERVISOR_PID_FILE"

exit 0
