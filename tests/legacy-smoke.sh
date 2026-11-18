#!/bin/bash
set -euo pipefail

test_dir=$(cd -- "$(dirname -- "$0")" && pwd)
bridge=$(cd -- "$(dirname -- "${1:?Supply the bridge executable}")" && pwd)/$(basename -- "$1")
backend_port=${LEGACY_BACKEND_PORT:-18635}
frontend_port=${LEGACY_FRONTEND_PORT:-18636}
temp_dir=$(mktemp -d)
backend_pid=
bridge_pid=

cleanup() {
    local status=$?
    trap - EXIT
    for pid in "$bridge_pid" "$backend_pid"; do
        if [[ -n $pid ]]; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    if (( status )); then
        /usr/bin/tail -40 "$temp_dir/"*.log || true
        printf 'Test files retained in %s\n' "$temp_dir" >&2
    else
        /bin/rm -r -- "$temp_dir"
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_port() {
    local port=$1 pid=$2
    for _ in {1..24}; do
        kill -0 "$pid" 2>/dev/null || return 1
        if /usr/bin/nc -z -G 1 127.0.0.1 "$port" >/dev/null 2>&1; then return 0; fi
        sleep 0.25
    done
    return 1
}

mkdir "$temp_dir/spool" "$temp_dir/keys"
sample=/usr/share/cups/ipptool/onepage-letter.pdf
if /usr/bin/gzip -t "$sample" 2>/dev/null; then
    /usr/bin/gzip -dc "$sample" > "$temp_dir/input.pdf"
else
    /bin/cp "$sample" "$temp_dir/input.pdf"
fi
/usr/bin/sed 's/150/300/g' "$test_dir/raster.ppd" > "$temp_dir/raster.ppd"
/usr/sbin/cupsfilter -p "$temp_dir/raster.ppd" -m image/urf "$temp_dir/input.pdf" \
    > "$temp_dir/input.urf" 2> "$temp_dir/converter.log"

/usr/bin/ippeveprinter -r off -n localhost -p "$backend_port" -f application/postscript \
    -k -v -c /usr/bin/true -D /dev/null -K "$temp_dir/keys" -d "$temp_dir/spool" 'PostScript only' \
    > "$temp_dir/backend.log" 2>&1 &
backend_pid=$!
wait_for_port "$backend_port" "$backend_pid"

"$bridge" serve --wifi-interface lo0 --ethernet-interface lo0 --port "$frontend_port" \
    --backend "ipp://127.0.0.1:$backend_port/ipp/print" > "$temp_dir/bridge.log" 2>&1 &
bridge_pid=$!
wait_for_port "$frontend_port" "$bridge_pid"
/usr/bin/grep -q 'AirPrint conversion: PostScript over IPP' "$temp_dir/bridge.log"
front="ipp://127.0.0.1:$frontend_port/ipp/print"
cat > "$temp_dir/caps.test" <<'TEST'
{
  NAME "Expose AirPrint formats on the PostScript-only printer"
  OPERATION Get-Printer-Attributes
  GROUP operation-attributes-tag
  ATTR charset attributes-charset utf-8
  ATTR language attributes-natural-language en
  ATTR uri printer-uri $uri
  ATTR keyword requested-attributes document-format-supported,urf-supported
  STATUS successful-ok
  EXPECT document-format-supported WITH-VALUE image/urf
  EXPECT document-format-supported WITH-VALUE application/pdf
  EXPECT urf-supported WITH-VALUE W8
}
TEST
/usr/bin/ipptool -T 8 -tv "$front" "$temp_dir/caps.test" > "$temp_dir/ipp-caps.log" 2>&1
/usr/bin/ipptool -T 12 -L -tv -d format=image/urf -f "$temp_dir/input.urf" \
    "$front" "$test_dir/raster.test" > "$temp_dir/ipp-urf.log" 2>&1
documents=("$temp_dir/spool/"*.ps)
/usr/bin/grep -a -q '^%!PS-Adobe-' "${documents[0]}"
/usr/bin/grep -a -q '^%%Pages: 1' "${documents[0]}"
/usr/bin/awk '/\[PASS\]/ {print}' "$temp_dir/ipp-caps.log" "$temp_dir/ipp-urf.log"
printf 'The PostScript-only IPP printer received a one-page PostScript document.\n'
