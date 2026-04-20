#!/usr/bin/env bash
set -euo pipefail

PROXY="${SOCKS_PROXY:-socks5h://127.0.0.1:1080}"
BASE_URL="https://speed.cloudflare.com/__down?bytes="

# Parse proxy into curl flags
proxy_flag=()
if [[ "$PROXY" == socks5h://* ]]; then
    proxy_flag=(--socks5-hostname "${PROXY#socks5h://}")
elif [[ "$PROXY" == socks5://* ]]; then
    proxy_flag=(--socks5 "${PROXY#socks5://}")
else
    proxy_flag=(--proxy "$PROXY")
fi

fmt_bytes() {
    awk "BEGIN {
        b = $1
        if (b >= 1048576)     printf \"%.1f MiB\", b/1048576
        else if (b >= 1024)   printf \"%.1f KiB\", b/1024
        else                  printf \"%d B\", b
    }"
}

fmt_speed() {
    awk "BEGIN {
        b = $1
        if (b >= 1048576)     printf \"%.2f MB/s\", b/1048576
        else if (b >= 1024)   printf \"%.1f KB/s\", b/1024
        else                  printf \"%.0f B/s\", b
    }"
}

run_test() {
    local label=$1 bytes=$2
    local result
    result=$(curl -s "${proxy_flag[@]}" -o /dev/null \
        -w "%{size_download} %{time_total} %{speed_download}" \
        --max-time 120 \
        "${BASE_URL}${bytes}" 2>&1) || true

    local size time speed
    size=$(echo "$result" | awk '{print $1}')
    time=$(echo "$result" | awk '{print $2}')
    speed=$(echo "$result" | awk '{print $3}')

    if [[ -z "$speed" || "$speed" == "0" || "$size" == "0" ]]; then
        printf "  %-12s  FAILED\n" "$label"
        return 1
    fi

    printf "  %-12s  %s in %6.2fs  %s\n" \
        "$label" "$(fmt_bytes "${size%%.*}")" "$time" "$(fmt_speed "$speed")"
}

run_concurrent() {
    local n=$1 bytes=$2 label=$3
    local pids=()
    local tmpdir
    tmpdir=$(mktemp -d)

    local start_s
    start_s=$(date +%s)

    for i in $(seq 1 "$n"); do
        curl -s "${proxy_flag[@]}" -o /dev/null \
            -w "%{size_download} %{time_total} %{speed_download}" \
            --max-time 120 \
            "${BASE_URL}${bytes}" > "$tmpdir/$i" 2>&1 &
        pids+=($!)
    done
    for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done

    local end_s
    end_s=$(date +%s)
    local wall=$(( end_s - start_s ))
    [[ $wall -eq 0 ]] && wall=1

    local total_bytes=0
    for i in $(seq 1 "$n"); do
        local size
        size=$(awk '{print $1}' "$tmpdir/$i")
        total_bytes=$((total_bytes + ${size%%.*}))
    done

    rm -rf "$tmpdir"

    local agg_speed=$((total_bytes / wall))

    printf "  %-12s  %s total in %ds wall  %s aggregate\n" \
        "$label" "$(fmt_bytes "$total_bytes")" "$wall" "$(fmt_speed "$agg_speed")"
}

echo "=== Tunnel Speed Test ==="
echo "Proxy: $PROXY"
echo ""

# Quick connectivity check
echo "Checking connectivity..."
if ! curl -s "${proxy_flag[@]}" --max-time 10 -o /dev/null "${BASE_URL}1000"; then
    echo "ERROR: Cannot reach Cloudflare through proxy"
    exit 1
fi
echo ""

echo "--- Single stream ---"
run_test "1 MB"   1000000
run_test "10 MB"  10000000
run_test "50 MB"  50000000
echo ""

echo "--- Concurrent streams ---"
run_concurrent 3 1000000  "3x 1MB"
run_concurrent 3 10000000 "3x 10MB"
echo ""

# Check flood control if running via docker compose
if command -v docker &>/dev/null && docker compose ps -q server 2>/dev/null | grep -q .; then
    floods=$(docker compose logs 2>&1 | grep -ic "flood" || true)
    echo "Flood control hits: $floods"
    echo ""
fi

echo "Done."
