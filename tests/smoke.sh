#!/bin/bash
set -euo pipefail

# Run an isolated IPP printer and bridge on loopback without changing the host's
# print queues, system daemon, routing table, or network settings.
script_directory=$(cd -- "$(dirname -- "$0")" && pwd)
bridge=${1:?Usage: smoke.sh /path/to/print-bridge}
if [[ "$bridge" != /* ]]; then
  bridge="$PWD/$bridge"
fi
backend_port=${BACKEND_PORT:-18631}
bridge_port=${BRIDGE_PORT:-18632}
resource=${BRIDGE_RESOURCE:-/ipp/print}
backend_uri="ipp://127.0.0.1:$backend_port/ipp/print"
bridge_uri="ipp://127.0.0.1:$bridge_port$resource"
sample_pdf=/usr/share/cups/ipptool/onepage-letter.pdf

temporary_directory=$(mktemp -d "$script_directory/smoke.XXXXXXXX")
backend_pid=
bridge_pid=

cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n "$bridge_pid" ]]; then
    kill "$bridge_pid" 2>/dev/null || true
    wait "$bridge_pid" 2>/dev/null || true
  fi
  if [[ -n "$backend_pid" ]]; then
    kill "$backend_pid" 2>/dev/null || true
    wait "$backend_pid" 2>/dev/null || true
  fi
  if (( result )); then
    for logfile in "$temporary_directory"/*.log; do
      [[ -f "$logfile" ]] || continue
      printf '\n%s\n' "${logfile##*/}"
      /usr/bin/tail -40 "$logfile"
    done
    printf '\nTest files retained at %s\n' "$temporary_directory" >&2
  else
    /bin/rm -r -- "$temporary_directory"
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_listener() {
  local port=$1 pid=$2
  local attempt
  for ((attempt = 0; attempt < 20; attempt++)); do
    kill -0 "$pid" 2>/dev/null || return 1
    if /usr/bin/nc -z -G 1 127.0.0.1 "$port" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

mkdir "$temporary_directory/spool" "$temporary_directory/keys"
pdf="$temporary_directory/input.pdf"
if /usr/bin/gzip -t "$sample_pdf" 2>/dev/null; then
  /usr/bin/gzip -dc "$sample_pdf" >"$pdf"
else
  /bin/cp "$sample_pdf" "$pdf"
fi
/usr/bin/ippeveprinter -r off -n localhost -p "$backend_port" \
  -f application/pdf,image/urf,image/pwg-raster -s 20,15 -k -v \
  -c /usr/bin/true -D /dev/null \
  -K "$temporary_directory/keys" -d "$temporary_directory/spool" 'Loopback printer' \
  >"$temporary_directory/backend.log" 2>&1 &
backend_pid=$!
wait_for_listener "$backend_port" "$backend_pid"

bridge_options=(
  --wifi-interface lo0 --ethernet-interface lo0
  --wifi-ip 127.0.0.1 --ethernet-source 127.0.0.1
  --port "$bridge_port" --name 'Loopback print bridge' --backend "$backend_uri"
)
"$bridge" probe "${bridge_options[@]}" >"$temporary_directory/probe.log" 2>&1
"$bridge" serve "${bridge_options[@]}" >"$temporary_directory/bridge.log" 2>&1 &
bridge_pid=$!
wait_for_listener "$bridge_port" "$bridge_pid"

for mode in -C -L; do
  /usr/bin/ipptool -T 4 "$mode" -tv -d bridge="$bridge_uri" \
    -f "$pdf" "$bridge_uri" "$script_directory/print.test" \
    >"$temporary_directory/ipptool${mode}.log" 2>&1
  /usr/bin/awk '/\[(PASS|FAIL)\]/' "$temporary_directory/ipptool${mode}.log"
  /usr/bin/grep -q 'Summary: 6 tests, 6 passed, 0 failed, 0 skipped' \
    "$temporary_directory/ipptool${mode}.log"
done

first_job_uri=$(/usr/bin/awk '$1 == "job-uri" && $2 == "(uri)" { print $4; exit }' \
  "$temporary_directory/ipptool-L.log")
[[ $first_job_uri == "ipp://127.0.0.1:$bridge_port/"* ]]
/usr/bin/ipptool -T 4 -tv "$first_job_uri" "$script_directory/job-uri.test" \
  >"$temporary_directory/job-uri.log" 2>&1
/usr/bin/awk '/\[(PASS|FAIL)\]/' "$temporary_directory/job-uri.log"
/usr/bin/grep -q '\[PASS\]' "$temporary_directory/job-uri.log"

for path in / /admin "$resource"; do
  status=$(/usr/bin/curl --noproxy '*' -sS --max-time 4 -o /dev/null \
    -w '%{http_code}' "http://127.0.0.1:$bridge_port$path")
  case "$status" in
    400|401|403|404|405|415) printf 'HTTP GET %s denied (%s)\n' "$path" "$status" ;;
    *) printf 'HTTP GET %s unexpectedly returned %s\n' "$path" "$status" >&2; exit 1 ;;
  esac
done

status=$(/usr/bin/curl --noproxy '*' -sS --max-time 4 -o /dev/null \
  -w '%{http_code}' -H 'Content-Type: application/ipp' --data-binary invalid \
  "http://127.0.0.1:$bridge_port/admin")
case "$status" in
  400|401|403|404|405|415) printf 'HTTP POST /admin denied (%s)\n' "$status" ;;
  *) printf 'HTTP POST /admin unexpectedly returned %s\n' "$status" >&2; exit 1 ;;
esac

spooled_documents=("$temporary_directory/spool/"*.pdf)
if (( ${#spooled_documents[@]} != 4 )); then
  printf 'Expected four PDF jobs in the backend spool\n' >&2
  exit 1
fi
for document in "${spooled_documents[@]}"; do
  /usr/bin/cmp -- "$pdf" "$document"
done
printf 'All four backend documents match the input PDF byte-for-byte.\n'

check_raster_print() {
  local format=$1 extension=$2 mode=$3
  local raster="$temporary_directory/input.$extension"
  /usr/sbin/cupsfilter -p "$script_directory/raster.ppd" -m "$format" "$pdf" \
    >"$raster" 2>"$temporary_directory/filter-$extension.log"
  /usr/bin/ipptool -T 4 "$mode" -tv -d format="$format" -f "$raster" \
    "$bridge_uri" "$script_directory/raster.test" >"$temporary_directory/$extension.log" 2>&1
  /usr/bin/awk '/\[(PASS|FAIL)\]/' "$temporary_directory/$extension.log"
  /usr/bin/grep -q 'Summary: 2 tests, 2 passed, 0 failed, 0 skipped' "$temporary_directory/$extension.log"
  local received=("$temporary_directory/spool/"*".$extension")
  [[ ${#received[@]} == 1 ]]
  /usr/bin/cmp -- "$raster" "${received[0]}"
  printf 'Backend %s document matches the raster input byte-for-byte.\n' "$format"
}

check_raster_print image/urf urf -C
check_raster_print image/pwg-raster pwg -L
