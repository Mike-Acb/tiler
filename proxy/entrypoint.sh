#!/bin/sh
set -e

PORT="${LISTEN_PORT:-8080}"
POOL_API="${PROXY_POOL_API:-http://proxy-pool:5010}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-30}"
TEST_URL="${PROXY_TEST_URL:-https://t0.tianditu.gov.cn/}"

BLACKLIST="/tmp/proxy/blacklist.txt"
SWITCH_TRIGGER="/tmp/proxy/switch"
GOST_PID=""
CURRENT_PROXY=""

log() { echo "[proxy] $*"; }

# ── Mode 1: fixed upstream ──
if [ -n "${UPSTREAM_PROXY:-}" ]; then
    log "fixed upstream: $UPSTREAM_PROXY"
    exec gost -L "http://:${PORT}" -F "$UPSTREAM_PROXY"
fi

# ── Mode 2: proxy_pool API ──

mkdir -p /tmp/proxy
touch "$BLACKLIST"

blacklist_add() {
    [ -z "$1" ] && return
    if ! grep -qxF "$1" "$BLACKLIST" 2>/dev/null; then
        echo "$1" >> "$BLACKLIST"
        log "blacklisted: $1 (total: $(wc -l < "$BLACKLIST" | tr -d ' '))"
    fi
}

is_blacklisted() {
    grep -qxF "$1" "$BLACKLIST" 2>/dev/null
}

get_proxy() {
    local attempts=0
    while [ $attempts -lt 20 ]; do
        local p
        p=$(curl -sf --max-time 5 "${POOL_API}/get/" 2>/dev/null | \
            sed -n 's/.*"proxy"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
        [ -z "$p" ] && return 1
        if is_blacklisted "$p"; then
            log "skip blacklisted: $p"
            delete_from_pool "$p"
            attempts=$((attempts + 1))
            continue
        fi
        echo "$p"
        return 0
    done
    return 1
}

delete_from_pool() {
    curl -sf --max-time 5 "${POOL_API}/delete/?proxy=$1" >/dev/null 2>&1 || true
}

pool_count() {
    curl -sf --max-time 5 "${POOL_API}/count/" 2>/dev/null || echo "{}"
}

wait_for_pool() {
    log "waiting for proxy_pool at ${POOL_API} ..."
    local i=0
    while [ $i -lt 60 ]; do
        if curl -sf --max-time 3 "${POOL_API}/count/" >/dev/null 2>&1; then
            log "proxy_pool is ready"
            return 0
        fi
        sleep 5
        i=$((i + 1))
    done
    log "proxy_pool not reachable after 5 min"
    return 1
}

wait_for_proxies() {
    log "waiting for available proxies..."
    local i=0
    while [ $i -lt 30 ]; do
        local p
        p=$(get_proxy) && [ -n "$p" ] && return 0
        sleep 10
        i=$((i + 1))
    done
    log "no proxies available after 5 min"
    return 1
}

start_gost() {
    if [ -n "$GOST_PID" ]; then
        kill "$GOST_PID" 2>/dev/null || true
        wait "$GOST_PID" 2>/dev/null || true
    fi
    CURRENT_PROXY="$1"
    log "using proxy: $CURRENT_PROXY"
    gost -L "http://:${PORT}" -F "http://${CURRENT_PROXY}" &
    GOST_PID=$!
    sleep 1
}

health_check() {
    curl -sf --proxy "http://localhost:${PORT}" \
         --connect-timeout 5 --max-time 10 \
         -o /dev/null "$TEST_URL" 2>/dev/null
}

switch_proxy() {
    log "switching from: $CURRENT_PROXY"
    blacklist_add "$CURRENT_PROXY"
    delete_from_pool "$CURRENT_PROXY"
    local new_proxy
    new_proxy=$(get_proxy)
    if [ -z "$new_proxy" ]; then
        log "pool empty, waiting..."
        wait_for_proxies || { log "WARN: no proxy available"; return 1; }
        new_proxy=$(get_proxy)
    fi
    start_gost "$new_proxy"
}

monitor() {
    local fail_count=0
    local tick=0
    while true; do
        sleep 5
        tick=$((tick + 1))

        # 418 trigger from tiler (check every 5s)
        if [ -f "$SWITCH_TRIGGER" ]; then
            rm -f "$SWITCH_TRIGGER"
            log "418 trigger received from tiler"
            switch_proxy || true
            fail_count=0
            tick=0
            continue
        fi

        # periodic health check (every HEALTH_INTERVAL)
        local interval_ticks=$((HEALTH_INTERVAL / 5))
        [ "$interval_ticks" -lt 1 ] && interval_ticks=1
        if [ $((tick % interval_ticks)) -eq 0 ]; then
            if health_check; then
                fail_count=0
            else
                fail_count=$((fail_count + 1))
                log "health check FAILED ($fail_count)"
                if [ "$fail_count" -ge 2 ]; then
                    switch_proxy || true
                    fail_count=0
                fi
            fi
        fi
    done
}

# ── main ──
log "blacklist file: $BLACKLIST"
wait_for_pool || { log "fallback: direct (no proxy)"; exec gost -L "http://:${PORT}"; }
wait_for_proxies || { log "fallback: direct (no proxy)"; exec gost -L "http://:${PORT}"; }

first=$(get_proxy)
start_gost "$first"
monitor
