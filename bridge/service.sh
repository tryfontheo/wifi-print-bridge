#!/bin/bash

# This file also supplies the networking functions used by the installer.
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH LC_ALL=C

LABEL=local.wifiprintbridge
STATE_DIR='/Library/Application Support/WiFiPrintBridge'
PLIST="/Library/LaunchDaemons/${LABEL}.plist"
CONFIG="$STATE_DIR/config.sh"
BRIDGE_BIN="$STATE_DIR/print-bridge"
LOG="$STATE_DIR/bridge.log"
OWN_ALIAS="$STATE_DIR/alias-owned"
OWN_ROUTE="$STATE_DIR/route-owned"

say() { printf '%s\n' "$*"; }
error() { printf '%s\n' "$*" >&2; }

interface_ipv4() {
    /sbin/ifconfig "$1" | /usr/bin/awk '$1 == "inet" && !found++ {print $2}'
}

interfaces_for_ip() {
    /sbin/ifconfig -a | /usr/bin/awk -v ip="$1" '
        /^[^[:space:]]/ { iface = $1; sub(/:$/, "", iface) }
        $1 == "inet" && $2 == ip { print iface }
    '
}

available_interface() {
    local info
    info=$(/sbin/ifconfig "$1" 2>/dev/null) || return 1
    [[ $info != *'status: inactive'* && $info == *$'\tinet '* ]]
}

bridge_args() {
    BRIDGE_ARGS=(--wifi-interface "$WIFI_IFACE" --ethernet-interface "$ETH_IFACE"
                 --backend "$BACKEND" --name "$PRINT_NAME" --port "$LISTEN_PORT")
    if [[ -n $ETH_ALIAS ]]; then
        BRIDGE_ARGS+=(--ethernet-source "$ETH_ALIAS")
    fi
    if [[ ${WINDOWS_LAN:-off} == on ]]; then
        BRIDGE_ARGS+=(--windows-lan)
    elif [[ -n ${WINDOWS_CLIENT:-} ]]; then
        BRIDGE_ARGS+=(--windows-client "$WINDOWS_CLIENT")
    fi
}

as_service_user() {
    /usr/bin/sudo -H -n -u nobody -- "$@"
}

scoped_route() {
    /sbin/route -n get -ifscope "$ETH_IFACE" "$1" 2>/dev/null
}

route_field() {
    /usr/bin/awk -v key="$1:" '$1 == key { print $2; exit }' <<< "$2"
}

matches_route() {
    local route flags source
    route=$(/sbin/route -nv get -ifscope "$ETH_IFACE" "$PRINTER_IP" 2>/dev/null) || return 1
    flags=$(route_field flags "$route")
    [[ $(route_field destination "$route") == "$PRINTER_IP" &&
       $(route_field interface "$route") == "$ETH_IFACE" &&
       $flags == *HOST* && $flags == *IFSCOPE* && $flags == *STATIC* ]] || return 1
    if [[ -n $GATEWAY ]]; then
        [[ $flags == *GATEWAY* && $(route_field gateway "$route") == "$GATEWAY" ]]
    else
        source=$(/usr/bin/awk '/^sockaddrs: .*IFA/ { getline; print $NF; exit }' <<< "$route")
        [[ $flags != *GATEWAY* && $source == "$ETH_ALIAS" ]]
    fi
}

alias_netmask() {
    /sbin/ifconfig "$ETH_IFACE" 2>/dev/null | /usr/bin/awk -v alias="$ETH_ALIAS" '
        $1 == "inet" && $2 == alias {
            for (i=3; i<NF; i++) if ($i == "netmask") { print $(i+1); exit }
        }
    '
}

ensure_alias() {
    local netmask
    netmask=$(alias_netmask)
    if [[ $netmask == 0xffffffff || $netmask == 255.255.255.255 ]]; then
        return 0
    fi
    if [[ -n $(interfaces_for_ip "$ETH_ALIAS") ]]; then
        error "The alias $ETH_ALIAS is already assigned locally. Choose an unused address."
        return 1
    fi
    /sbin/ifconfig "$ETH_IFACE" inet "$ETH_ALIAS" netmask 255.255.255.255 alias || return 1
    if ! /usr/bin/touch "$OWN_ALIAS"; then
        /sbin/ifconfig "$ETH_IFACE" inet "$ETH_ALIAS" -alias
        return 1
    fi
}

ensure_routing() {
    local gateway_route gateway_flags
    local -a route_args
    [[ -n $ETH_ALIAS || -n $GATEWAY ]] || return 0
    route_args=(-host -ifscope "$ETH_IFACE" "$PRINTER_IP")
    if [[ -n $ETH_ALIAS ]]; then
        ensure_alias || return 1
        route_args+=(-interface "$ETH_ALIAS" -ifp "$ETH_IFACE" -ifa "$ETH_ALIAS")
    else
        gateway_route=$(scoped_route "$GATEWAY") || gateway_route=
        gateway_flags=$(route_field flags "$gateway_route")
        if [[ $(route_field interface "$gateway_route") != "$ETH_IFACE" ||
              $gateway_flags == *GATEWAY* || $gateway_flags == *LOCAL* ]]; then
            error "Gateway $GATEWAY must be directly reachable on Ethernet interface $ETH_IFACE."
            return 1
        fi
        route_args+=("$GATEWAY" -ifp "$ETH_IFACE")
    fi
    matches_route && return 0
    /sbin/route -n add "${route_args[@]}" || return 1
    if ! matches_route; then
        error "Could not verify the scoped Ethernet route to $PRINTER_IP."
        /sbin/route -n delete -host -ifscope "$ETH_IFACE" "$PRINTER_IP"
        return 1
    fi
    if ! /usr/bin/touch "$OWN_ROUTE"; then
        /sbin/route -n delete -host -ifscope "$ETH_IFACE" "$PRINTER_IP"
        return 1
    fi
}

remove_owned_routing() {
    local netmask
    if [[ -f $OWN_ROUTE ]] && matches_route; then
        /sbin/route -n delete -host -ifscope "$ETH_IFACE" "$PRINTER_IP" || return 1
    fi
    if [[ -f $OWN_ALIAS ]]; then
        netmask=$(alias_netmask)
        if [[ $netmask == 0xffffffff || $netmask == 255.255.255.255 ]]; then
            /sbin/ifconfig "$ETH_IFACE" inet "$ETH_ALIAS" -alias || return 1
        fi
    fi
}

console_available() {
    local user
    user=$(/usr/bin/stat -f '%Su' /dev/console 2>/dev/null) || return 1
    case $user in
        ''|root|loginwindow|_mbsetupuser) return 1 ;;
    esac
}

stop_child() {
    if [[ -n $child ]]; then
        kill "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
        child=
    fi
}

serve_at_login() {
    local child= active_wifi_ip= current_wifi_ip= waiting=
    [[ $EUID == 0 && -f $CONFIG ]] || { error 'The service needs an installed configuration.'; return 1; }
    # The installer creates this file as root under a root-owned directory.
    # shellcheck source=/dev/null
    source "$CONFIG"
    bridge_args
    trap 'stop_child; exit 0' TERM INT

    while true; do
        if console_available && available_interface "$WIFI_IFACE" && available_interface "$ETH_IFACE"; then
            current_wifi_ip=$(interface_ipv4 "$WIFI_IFACE")
            if ensure_routing; then
                if [[ $active_wifi_ip != "$current_wifi_ip" ]]; then
                    stop_child
                    active_wifi_ip=$current_wifi_ip
                fi
                if [[ -z $child ]] || ! kill -0 "$child" 2>/dev/null; then
                    say "Starting the print bridge on $WIFI_IFACE ($current_wifi_ip) via $ETH_IFACE."
                    (exec /usr/bin/sudo -H -n -u nobody -- "$BRIDGE_BIN" serve "${BRIDGE_ARGS[@]}") &
                    child=$!
                fi
                waiting=
            else
                stop_child
            fi
        else
            stop_child
            if [[ -z $waiting ]]; then
                say 'Waiting for a graphical login and both network interfaces.'
                waiting=yes
            fi
        fi
        /bin/sleep 10 &
        wait "$!"
    done
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    set -u
    serve_at_login
fi
