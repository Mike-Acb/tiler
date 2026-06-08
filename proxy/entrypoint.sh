#!/bin/sh
set -e

PORT="${LISTEN_PORT:-8080}"
POOL_API="${PROXY_POOL_API:-http://proxy-pool:5010}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-15}"
TEST_URL="${PROXY_TEST_URL:-http://t0.tianditu.gov.cn/DataServer?T=vec_w&x=0&y=0&l=1&tk=75f0434f240669f4a2df6359275146d2}"
UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
REFERER="https://map.tianditu.gov.cn"
CONNECT_TIMEOUT="${CONNECT_TIMEOUT:-3}"
MAX_TIME="${MAX_TIME:-5}"
PRE_TEST_BATCH="${PRE_TEST_BATCH:-10}"

BLACKLIST="/tmp/proxy/blacklist.txt"
SWITCH_TRIGGER="/tmp/proxy/switch"
GOST_PID=""
CURRENT_PROXY=""

log() { echo "[proxy] $*"; }

# ── Mode 1: fixed upstream ──
if [ -n "${UPSTREAM_PROXY:-}" ]; then
    log "fixed upstream: $UPSTREAM_PROXY"
    mkdir -p /tmp/proxy
    touch /tmp/proxy/ready
    exec gost -L "http://:${PORT}" -F "$UPSTREAM_PROXY"
fi

# ── Mode 2: proxy_pool API ──

mkdir -p /tmp/proxy
touch "$BLACKLIST"

mark_ready()   { touch /tmp/proxy/ready; }
mark_unready() { rm -f /tmp/proxy/ready; }

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

delete_from_pool() {
    curl -sf --max-time 3 "${POOL_API}/delete/?proxy=$1" >/dev/null 2>&1 || true
}

test_proxy_direct() {
    curl -sf --proxy "http://$1" \
         --connect-timeout "$CONNECT_TIMEOUT" --max-time "$MAX_TIME" \
         -o /dev/null -H "User-Agent: $UA" -H "Referer: $REFERER" "$TEST_URL" 2>/dev/null
}

get_proxy() {
    local candidates="/tmp/proxy/candidates.txt"
    local winner="/tmp/proxy/winner.txt"
    : > "$candidates"
    : > "$winner"

    local batch=0
    while [ $batch -lt 5 ]; do
        # Grab a batch of proxies from pool
        local i=0
        : > "$candidates"
        while [ $i -lt "$PRE_TEST_BATCH" ]; do
            local p
            p=$(curl -sf --max-time 3 "${POOL_API}/get/" 2>/dev/null | \
                sed -n 's/.*"proxy"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
            [ -z "$p" ] && break
            if ! is_blacklisted "$p" && ! grep -qxF "$p" "$candidates" 2>/dev/null; then
                echo "$p" >> "$candidates"
            fi
            i=$((i + 1))
        done

        local count
        count=$(wc -l < "$candidates" | tr -d ' ')
        [ "$count" -eq 0 ] && { batch=$((batch + 1)); sleep 5; continue; }

        log "testing $count proxies (parallel)..."

        # Parallel test: first one to succeed wins
        cat "$candidates" | xargs -P "$count" -I{} sh -c '
            if curl -sf --proxy "http://{}" \
                   --connect-timeout '"$CONNECT_TIMEOUT"' --max-time '"$MAX_TIME"' \
                   -o /dev/null -H "User-Agent: '"$UA"'" -H "Referer: '"$REFERER"'" \
                   "'"$TEST_URL"'" 2>/dev/null; then
                echo "{}" >> "'"$winner"'"
            fi
        ' 2>/dev/null || true

        if [ -s "$winner" ]; then
            local result
            result=$(head -1 "$winner")
            log "found working proxy: $result"
            # Blacklist the rest that didn't work
            while IFS= read -r p; do
                if ! grep -qxF "$p" "$winner" 2>/dev/null; then
                    blacklist_add "$p"
                    delete_from_pool "$p"
                fi
            done < "$candidates"
            echo "$result"
            return 0
        fi

        # All failed, blacklist them
        while IFS= read -r p; do
            blacklist_add "$p"
            delete_from_pool "$p"
        done < "$candidates"
        log "batch $((batch + 1)) all failed, retrying..."
        batch=$((batch + 1))
    done

    log "no working proxy found after $batch batches"
    return 1
}

wait_for_pool() {
    log "waiting for proxy_pool at ${POOL_API} ..."
    local i=0
    while [ $i -lt 60 ]; do
        if curl -sf --max-time 3 "${POOL_API}/count/" >/dev/null 2>&1; then
            log "proxy_pool is ready"
            return 0
        fi
        sleep 3
        i=$((i + 1))
    done
    log "proxy_pool not reachable after 3 min"
    return 1
}

start_gost() {
    mark_unready
    if [ -n "$GOST_PID" ]; then
        kill "$GOST_PID" 2>/dev/null || true
        wait "$GOST_PID" 2>/dev/null || true
    fi
    CURRENT_PROXY="$1"
    log "using proxy: $CURRENT_PROXY"
    gost -L "http://:${PORT}" -F "http://${CURRENT_PROXY}" &
    GOST_PID=$!
    sleep 1
    mark_ready
}

health_check() {
    curl -sf --proxy "http://localhost:${PORT}" \
         --connect-timeout "$CONNECT_TIMEOUT" --max-time "$MAX_TIME" \
         -o /dev/null -H "User-Agent: $UA" -H "Referer: $REFERER" "$TEST_URL" 2>/dev/null
}

switch_proxy() {
    log "switching from: $CURRENT_PROXY"
    blacklist_add "$CURRENT_PROXY"
    delete_from_pool "$CURRENT_PROXY"
    local new_proxy
    new_proxy=$(get_proxy) || { log "WARN: no proxy available"; return 1; }
    start_gost "$new_proxy"
}

monitor() {
    local fail_count=0
    local tick=0
    while true; do
        sleep 3
        tick=$((tick + 1))

        # 418 trigger from tiler (check every 3s)
        if [ -f "$SWITCH_TRIGGER" ]; then
            rm -f "$SWITCH_TRIGGER"
            log "418 trigger received from tiler"
            switch_proxy || true
            fail_count=0
            tick=0
            continue
        fi

        # periodic health check
        local interval_ticks=$((HEALTH_INTERVAL / 3))
        [ "$interval_ticks" -lt 1 ] && interval_ticks=1
        if [ $((tick % interval_ticks)) -eq 0 ]; then
            if health_check; then
                fail_count=0
            else
                fail_count=$((fail_count + 1))
                log "health check FAILED ($fail_count)"
                if [ "$fail_count" -ge 1 ]; then
                    switch_proxy || true
                    fail_count=0
                fi
            fi
        fi
    done
}

# ── main ──
log "config: timeout=${CONNECT_TIMEOUT}/${MAX_TIME}s, health=${HEALTH_INTERVAL}s, batch=${PRE_TEST_BATCH}"
log "test url: $TEST_URL"
wait_for_pool || { log "fallback: direct (no proxy)"; touch /tmp/proxy/ready; exec gost -L "http://:${PORT}"; }

first=$(get_proxy) || { log "fallback: direct (no proxy)"; touch /tmp/proxy/ready; exec gost -L "http://:${PORT}"; }
start_gost "$first"
monitor
