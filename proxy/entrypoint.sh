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
# PROXY_AUTH=用户名:密码：代理需要账号认证时（如青果的 AuthKey:AuthPwd）带上；密码里有特殊字符需先 URL 编码
AUTH_PREFIX="${PROXY_AUTH:+${PROXY_AUTH}@}"
# PROXY_POOL_API 带查询参数（?）时按「提取链接」处理：直接 GET 这个地址，从返回文本里取 ip:port，
# 如青果 https://share.proxy.qg.net/get?key=...&num=1&format=txt；否则按 jhao104/proxy_pool 的 /get/ /delete/ /count/。
case "$POOL_API" in *\?*) FETCH_MODE=1 ;; *) FETCH_MODE=0 ;; esac
POOL_SHOWN="${POOL_API%%\?*}" # 日志里不打印查询参数（含 key）
# PROXY_MAX_AGE：代理用满这么多秒就提前换（0 不换）。提取链接多是约 1 分钟的短效代理，默认 50 秒；代理池默认不换。
if [ -z "${PROXY_MAX_AGE:-}" ]; then
    if [ "$FETCH_MODE" = 1 ]; then PROXY_MAX_AGE=50; else PROXY_MAX_AGE=0; fi
fi
# PROXY_COUNT：同时使用几个上游代理，请求在它们之间轮流分发（单个短效代理带宽有限，多个叠加提速）。
# 不设时：提取链接取链接里的 num 参数，代理池为 1。
if [ -z "${PROXY_COUNT:-}" ]; then
    PROXY_COUNT=1
    if [ "$FETCH_MODE" = 1 ]; then
        n=$(echo "$POOL_API" | sed -n 's/.*[?&]num=\([0-9][0-9]*\).*/\1/p')
        [ -n "$n" ] && [ "$n" -gt 0 ] && PROXY_COUNT=$n
    fi
fi

BLACKLIST="/tmp/proxy/blacklist.txt"
SWITCH_TRIGGER="/tmp/proxy/switch"
BASE_PORT=$((PORT + 1)) # 各上游代理槽位的本地端口：8081、8082……
FRONT_PID=""
# 多代理时一个上游一个 gost 进程，让 Go 早点回收内存，单进程占用更小
export GOGC="${GOGC:-50}"

log() { echo "[proxy] $*" >&2; }

# ── Mode 1: fixed upstream ──
if [ -n "${UPSTREAM_PROXY:-}" ]; then
    log "fixed upstream: $(echo "$UPSTREAM_PROXY" | sed 's#//[^@/]*@#//***@#')"
    mkdir -p /tmp/proxy
    touch /tmp/proxy/ready
    exec gost -L "http://:${PORT}" -F "$UPSTREAM_PROXY"
fi

# ── Mode 2: proxy_pool API / 提取链接 ──
#
# 结构：tiler → :8080 前端 gost → 轮流转给 127.0.0.1:8081..808N 的槽位 gost → 各自的上游代理。
# 每个槽位单独换代理（到期、坏掉），前端地址列表固定不变，不用重启；换的那个槽位上被掐断的请求由 tiler 重试。

mkdir -p /tmp/proxy
touch "$BLACKLIST"

mark_ready()   { touch /tmp/proxy/ready; }
mark_unready() { rm -f /tmp/proxy/ready; }

blacklist_add() {
    [ -z "$1" ] && return
    if ! grep -qxF "$1" "$BLACKLIST" 2>/dev/null; then
        echo "$1" >> "$BLACKLIST"
    fi
}

is_blacklisted() {
    grep -qxF "$1" "$BLACKLIST" 2>/dev/null
}

# in_use：是否已在某个槽位上用着（提取链接可能返回重复的 IP）
in_use() {
    local i=1
    while [ $i -le "$PROXY_COUNT" ]; do
        [ "$(slot_get PROXY $i)" = "$1" ] && return 0
        i=$((i + 1))
    done
    return 1
}

delete_from_pool() {
    [ "$FETCH_MODE" = 1 ] && return 0
    curl -sf --max-time 3 "${POOL_API}/delete/?proxy=$1" >/dev/null 2>&1 || true
}

# probe.sh <代理地址>：经该代理请求测试瓦片，状态 200 且 Content-Type 为 image/* 才算可用。
# 只看状态码不够：有的免费代理对任何请求都回 200 加一小段 HTML，tiler 校验全失败却不会切换。
# 写成独立脚本，供下面 xargs 并行测试的子 shell 调用；放容器本地，不放多个 Job 共用的 /tmp/proxy。
PROBE=/tmp/probe.sh
export TEST_URL UA REFERER CONNECT_TIMEOUT MAX_TIME
cat > "$PROBE" <<'EOF'
#!/bin/sh
r=$(curl -s --proxy "$1" --connect-timeout "$CONNECT_TIMEOUT" --max-time "$MAX_TIME" \
      -o /dev/null -w '%{http_code} %{content_type}' \
      -H "User-Agent: $UA" -H "Referer: $REFERER" "$TEST_URL" 2>/dev/null) || exit 1
case "$r" in
    "200 image/"*) exit 0 ;;
esac
exit 1
EOF
chmod +x "$PROBE"

# get_proxies <n>：取最多 n 个测试可用的代理，每行一个；一个都没有时返回 1。
get_proxies() {
    local want="$1"
    local candidates="/tmp/proxy/candidates.txt"
    local winners="/tmp/proxy/winners.txt"
    : > "$winners"

    local batch=0 max_batches=5 found=0
    # 提取链接每调用一次就扣一次额度：每批只调一次，一次取还差的个数，一轮最多 3 批
    [ "$FETCH_MODE" = 1 ] && max_batches=3
    while [ $batch -lt $max_batches ]; do
        : > "$candidates"
        local need=$((want - found))
        if [ "$FETCH_MODE" = 1 ]; then
            local url
            url=$(echo "$POOL_API" | sed "s/\([?&]\)num=[0-9][0-9]*/\1num=${need}/")
            curl -sf --max-time 8 "$url" 2>/dev/null | grep -oE '[0-9]{1,3}(\.[0-9]{1,3}){3}:[0-9]{2,5}' | \
                while IFS= read -r p; do
                    is_blacklisted "$p" || in_use "$p" || grep -qxF "$p" "$candidates" "$winners" 2>/dev/null || echo "$p" >> "$candidates"
                done
        else
            local i=0 limit=$((need > PRE_TEST_BATCH ? need : PRE_TEST_BATCH))
            while [ $i -lt "$limit" ]; do
                local p
                p=$(curl -sf --max-time 3 "${POOL_API}/get/" 2>/dev/null | \
                    sed -n 's/.*"proxy"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
                [ -z "$p" ] && break
                if ! is_blacklisted "$p" && ! in_use "$p" && ! grep -qxF "$p" "$candidates" "$winners" 2>/dev/null; then
                    echo "$p" >> "$candidates"
                fi
                i=$((i + 1))
            done
        fi

        local count
        count=$(wc -l < "$candidates" | tr -d ' ')
        if [ "$count" -eq 0 ]; then
            log "batch $((batch + 1)): no new proxy returned"
            batch=$((batch + 1)); sleep 5; continue
        fi

        # 并行测试，可用的追加到 winners
        xargs -P "$count" -I{} sh -c '
            if "$1" "http://$4$2"; then echo "$2" >> "$3"; fi
        ' _ "$PROBE" {} "$winners" "$AUTH_PREFIX" < "$candidates" 2>/dev/null || true

        # 没通过测试的拉黑，免得再拿到
        while IFS= read -r p; do
            if ! grep -qxF "$p" "$winners" 2>/dev/null; then
                blacklist_add "$p"
                delete_from_pool "$p"
            fi
        done < "$candidates"

        found=$(wc -l < "$winners" | tr -d ' ')
        log "batch $((batch + 1)): $count candidates, $found/$want usable"
        [ "$found" -ge "$want" ] && break
        batch=$((batch + 1))
    done

    [ "$found" -eq 0 ] && return 1
    head -n "$want" "$winners"
}

wait_for_pool() {
    if [ "$FETCH_MODE" = 1 ]; then
        log "proxy fetch url: ${POOL_SHOWN}?...${PROXY_AUTH:+ (with auth)}"
        return 0
    fi
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

# ── 槽位：SLOT_PROXY_i / SLOT_PID_i / SLOT_AT_i ──

slot_get() { eval "echo \"\${SLOT_${1}_${2}:-}\""; }  # slot_get PROXY 3
slot_set() { eval "SLOT_${1}_${2}=\"\$3\""; }         # slot_set PROXY 3 1.2.3.4:5

slot_stop() {
    local pid
    pid=$(slot_get PID "$1")
    if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    fi
    slot_set PID "$1" ""
    slot_set PROXY "$1" ""
}

slot_start() {
    local i="$1" p="$2"
    slot_stop "$i"
    gost -L "http://127.0.0.1:$((BASE_PORT + i - 1))" -F "http://${AUTH_PREFIX}${p}" 2>/dev/null &
    slot_set PID "$i" $!
    slot_set PROXY "$i" "$p"
    slot_set AT "$i" "$(date +%s)"
}

slots_alive() {
    local i=1 n=0
    while [ $i -le "$PROXY_COUNT" ]; do
        [ -n "$(slot_get PROXY $i)" ] && n=$((n + 1))
        i=$((i + 1))
    done
    echo $n
}

update_ready() {
    if [ "$(slots_alive)" -gt 0 ]; then mark_ready; else mark_unready; fi
}

# refill <start|failed|rotate> <槽位...>：给这些槽位换上新代理。
# 一次提取所需个数；拿到的不够时，到期轮换的槽位先用着旧代理（15 秒后再试），坏掉的槽位置空。
refill() {
    local reason="$1"; shift
    [ $# -eq 0 ] && return 0
    local i old
    for i in "$@"; do
        old=$(slot_get PROXY "$i")
        [ -n "$old" ] && { blacklist_add "$old"; delete_from_pool "$old"; }
        [ "$reason" != rotate ] && slot_stop "$i"
    done
    update_ready
    local got
    got=$(get_proxies $#) || got=""
    for i in "$@"; do
        local p
        p=$(echo "$got" | sed -n '1p')
        got=$(echo "$got" | sed '1d')
        if [ -n "$p" ]; then
            slot_start "$i" "$p"
            log "slot $i ($reason): $p"
        elif [ "$reason" = rotate ] && [ -n "$(slot_get PROXY "$i")" ]; then
            slot_set AT "$i" $(( $(slot_get AT "$i") + 15 ))
        else
            slot_stop "$i"
        fi
    done
    sleep 1 # 等新 gost 监听
    update_ready
    log "active proxies: $(slots_alive)/${PROXY_COUNT}"
}

# check_slots：探测各槽位（刚换上 8 秒内的跳过），坏的和空的一起补。
check_slots() {
    local i=1 now bad=""
    now=$(date +%s)
    while [ $i -le "$PROXY_COUNT" ]; do
        if [ -z "$(slot_get PROXY $i)" ]; then
            bad="$bad $i"
        elif [ $((now - $(slot_get AT $i))) -ge 8 ] && ! "$PROBE" "http://127.0.0.1:$((BASE_PORT + i - 1))"; then
            log "slot $i health check FAILED: $(slot_get PROXY $i)"
            bad="$bad $i"
        fi
        i=$((i + 1))
    done
    # shellcheck disable=SC2086
    refill failed $bad
}

rotate_due() {
    [ "$PROXY_MAX_AGE" -gt 0 ] || return 0
    local i=1 now due=""
    now=$(date +%s)
    while [ $i -le "$PROXY_COUNT" ]; do
        if [ -n "$(slot_get PROXY $i)" ] && [ $((now - $(slot_get AT $i))) -ge "$PROXY_MAX_AGE" ]; then
            due="$due $i"
        fi
        i=$((i + 1))
    done
    # shellcheck disable=SC2086
    refill rotate $due
}

start_front() {
    local ips="" i=1
    while [ $i -le "$PROXY_COUNT" ]; do
        ips="${ips:+$ips,}127.0.0.1:$((BASE_PORT + i - 1))"
        i=$((i + 1))
    done
    # 某个槽位连不上（正在换、已置空）时，前端 10 秒内不再选它
    gost -L "http://:${PORT}" \
        -F "http://127.0.0.1:${BASE_PORT}?ip=${ips}&strategy=round&max_fails=1&fail_timeout=10s" &
    FRONT_PID=$!
}

monitor() {
    local tick=0 wait_s=30 next_fill=0
    local interval_ticks=$((HEALTH_INTERVAL / 3))
    [ "$interval_ticks" -lt 1 ] && interval_ticks=1
    while true; do
        sleep 3
        tick=$((tick + 1))

        # 前端 gost 挂了（如被 OOM 杀掉）tiler 就全部连不上，立刻拉起
        if ! kill -0 "$FRONT_PID" 2>/dev/null; then
            log "WARN: front gost exited, restarting"
            start_front
        fi

        # tiler 报告代理出错（每 3 秒看一次）：马上探测各槽位
        if [ -f "$SWITCH_TRIGGER" ]; then
            log "switch trigger from tiler: $(cat "$SWITCH_TRIGGER" 2>/dev/null)"
            rm -f "$SWITCH_TRIGGER"
            check_slots || true
            tick=0
            continue
        fi

        # 短效代理到期前主动换
        rotate_due || true

        if [ $((tick % interval_ticks)) -eq 0 ]; then
            # 全空时按 30 秒起、最长 5 分钟的间隔重试，避免空耗提取额度
            if [ "$(slots_alive)" -eq 0 ]; then
                [ "$(date +%s)" -lt "$next_fill" ] && continue
                log "WARN: no proxy available, retrying"
                check_slots || true
                if [ "$(slots_alive)" -eq 0 ]; then
                    next_fill=$(( $(date +%s) + wait_s ))
                    log "WARN: still no proxy, pausing downloads, retry in ${wait_s}s"
                    wait_s=$((wait_s * 2)); [ "$wait_s" -gt 300 ] && wait_s=300
                else
                    wait_s=30
                fi
                continue
            fi
            check_slots || true
        fi
    done
}

# ── main ──
log "config: proxies=${PROXY_COUNT}, timeout=${CONNECT_TIMEOUT}/${MAX_TIME}s, health=${HEALTH_INTERVAL}s, batch=${PRE_TEST_BATCH}, max_age=${PROXY_MAX_AGE}s"
log "test url: $TEST_URL"
wait_for_pool || { log "proxy_pool not reachable, exiting"; exit 1; }

start_front
wait_s=30
all=$(i=1; while [ $i -le "$PROXY_COUNT" ]; do printf '%s ' $i; i=$((i + 1)); done)
while true; do
    # shellcheck disable=SC2086
    refill start $all
    [ "$(slots_alive)" -gt 0 ] && break
    log "no proxy available at startup, retry in ${wait_s}s..."
    sleep "$wait_s"
    wait_s=$((wait_s * 2)); [ "$wait_s" -gt 300 ] && wait_s=300
done
monitor
