#!/bin/bash
set -euo pipefail

PACKAGE_DIR=$(cd -P -- "$(/usr/bin/dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=service.sh
source "$PACKAGE_DIR/service.sh"

usage() {
    cat <<'EOF'
Usage
  bash install.sh discover [--ethernet-interface enN]
  bash install.sh diagnose --backend ipp://IP[:PORT]/PATH [options]
  sudo bash install.sh install --backend ipp://IP[:PORT]/PATH [options]
  sudo bash install.sh install [update options]
  sudo bash install.sh status
  sudo bash install.sh uninstall

Options for diagnose and install
  --wifi-ip ADDRESS              Identify Wi-Fi by its current IPv4 address
  --ethernet-ip ADDRESS          Identify Ethernet by its current IPv4 address
  --wifi-interface enN           Identify Wi-Fi directly by its macOS device name
  --ethernet-interface enN       Identify Ethernet directly by its macOS device name
  --backend ipp://IP[:PORT]/PATH  Printer's IPP endpoint on Ethernet
  --gateway ADDRESS              Router on the Ethernet subnet that can reach the printer
  --alias ADDRESS                Reserved Ethernet IPv4 address on the printer's own subnet
  --name NAME                    Printer name shown to phones (default Ethernet Printer)
  --port PORT                    Wi-Fi listening port (default 8631)
  --windows-lan on|off           Allow Windows RAW printing from the Wi-Fi subnet, or disable it
  --windows-client ADDRESS|off   Allow one Windows IPv4 address to print over RAW TCP 9100, or disable it

Discover lists Ethernet printers as JSON. Diagnose performs no network configuration changes.
Without interface or address options, the installer detects Wi-Fi and Ethernet.
Choose at most one Windows option and at most one of --gateway and --alias.
An in-place update accepts --backend, --ethernet-interface, and Windows options.
Omitting any of those options preserves its existing setting.
EOF
}

die() { error "$*"; exit 1; }

needs_root() {
    [[ $EUID == 0 ]] || die 'Run this command with sudo on the target Mac.'
}

is_ipv4() {
    local address=$1 first second third fourth
    [[ $address =~ ^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ ]] || return 1
    first=${BASH_REMATCH[1]} second=${BASH_REMATCH[2]} third=${BASH_REMATCH[3]} fourth=${BASH_REMATCH[4]}
    (( first > 0 && first < 224 && first != 127 && second < 256 && third < 256 && fourth < 256 ))
}

wifi_hardware_interface() {
    /usr/sbin/networksetup -listallhardwareports | /usr/bin/awk '
        /^Hardware Port: / { wifi = ($0 == "Hardware Port: Wi-Fi" || $0 == "Hardware Port: AirPort") }
        wifi && /^Device: en[0-9]+$/ { print $2; exit }
    '
}

ethernet_candidates() {
    local wifi=$1 iface
    for iface in $(/sbin/ifconfig -l); do
        if [[ $iface =~ ^en[0-9]+$ && $iface != "$wifi" ]] && available_interface "$iface"; then
            say "$iface"
        fi
    done
    return 0
}

direct_printer_route() {
    local iface=$1 route flags
    route=$(/sbin/route -n get -ifscope "$iface" "$PRINTER_IP" 2>/dev/null) || return 1
    flags=$(route_field flags "$route")
    [[ $(route_field interface "$route") == "$iface" && -n $flags &&
       $flags != *GATEWAY* && $flags != *LOCAL* && $flags != *REJECT* && $flags != *BLACKHOLE* ]]
}

detect_ethernet() {
    local wifi=$1 iface direct=
    local -a candidates=()
    while IFS= read -r iface; do
        candidates+=("$iface")
    done < <(ethernet_candidates "$wifi")
    if (( ${#candidates[@]} == 1 )); then
        say "${candidates[0]}"
        return 0
    fi
    if (( ${#candidates[@]} == 0 )); then
        error 'Could not find an active Ethernet interface with IPv4. Connect Ethernet and check its network settings.'
        return 1
    fi
    for iface in "${candidates[@]}"; do
        if direct_printer_route "$iface"; then
            if [[ -n $direct ]]; then
                direct=
                break
            fi
            direct=$iface
        fi
    done
    if [[ -n $direct ]]; then
        say "$direct"
        return 0
    fi
    error 'Multiple active Ethernet interfaces were found. Enter the current IP of the Ethernet adapter connected to the printer network, or specify --ethernet-interface enN.'
    for iface in "${candidates[@]}"; do
        error "  $iface ($(interface_ipv4 "$iface"))"
    done
    return 1
}

validate_interface() {
    local role=$1 iface=$2 address=$3
    if [[ ! $iface =~ ^en[0-9]+$ ]] || ! available_interface "$iface"; then
        if [[ -n $address ]]; then
            error "Could not find an active $role interface with IPv4 address $address. Enter its current address or specify --${role}-interface enN."
        else
            error "Could not find an active $role interface with IPv4. Connect $role or specify --${role}-interface enN."
        fi
        return 1
    fi
    if [[ -n $address ]] && ! interfaces_for_ip "$address" | /usr/bin/awk -v iface="$iface" '$0 == iface { found = 1 } END { exit !found }'; then
        error "$role interface $iface does not have IPv4 address $address. Enter its current address."
        return 1
    fi
}

select_interfaces() {
    local wifi_ip=$1 eth_ip=$2 hardware_wifi
    hardware_wifi=$(wifi_hardware_interface) || hardware_wifi=
    if [[ -z $WIFI_IFACE ]]; then
        if [[ -n $wifi_ip ]]; then
            WIFI_IFACE=$(interfaces_for_ip "$wifi_ip")
        else
            WIFI_IFACE=$hardware_wifi
        fi
    fi
    validate_interface wifi "$WIFI_IFACE" "$wifi_ip" || return 1
    if [[ -n $hardware_wifi && $WIFI_IFACE != "$hardware_wifi" ]]; then
        error "$WIFI_IFACE is not the Wi-Fi adapter. The Wi-Fi adapter is $hardware_wifi ($(interface_ipv4 "$hardware_wifi"))."
        return 1
    fi

    if [[ -z $ETH_IFACE ]]; then
        if [[ -n $eth_ip ]]; then
            ETH_IFACE=$(interfaces_for_ip "$eth_ip")
        else
            ETH_IFACE=$(detect_ethernet "$WIFI_IFACE") || return 1
        fi
    fi
    validate_interface ethernet "$ETH_IFACE" "$eth_ip" || return 1
    if [[ $ETH_IFACE == "$WIFI_IFACE" || ( -n $hardware_wifi && $ETH_IFACE == "$hardware_wifi" ) ]]; then
        error "$ETH_IFACE is the Wi-Fi adapter. Enter the address of the separate Ethernet adapter."
        return 1
    fi
}

validate_ip() {
    is_ipv4 "$2" || die "$1 needs a unicast IPv4 address. Received $2."
}

set_windows_mode() {
    local option=$1 value=$2
    WINDOWS_CLIENT=
    WINDOWS_LAN=off
    case $option in
        --windows-client)
            if [[ $value != off ]]; then
                validate_ip "$option" "$value"
                WINDOWS_CLIENT=$value
            fi
            ;;
        --windows-lan)
            [[ $value == on || $value == off ]] || die 'Use on or off after --windows-lan.'
            WINDOWS_LAN=$value
            ;;
    esac
}

validate_backend() {
    local backend_port
    [[ -n $BACKEND ]] || die 'Choose an Ethernet printer and provide its IPP address with --backend.'
    [[ $BACKEND =~ ^ipp://([0-9.]+)(:([1-9][0-9]{0,4}))?(/[^[:space:]?#]*)$ ]] ||
        die 'Use a printer IPv4 and IPP path, for example ipp://192.0.2.25:631/ipp/print.'
    PRINTER_IP=${BASH_REMATCH[1]}
    backend_port=${BASH_REMATCH[3]:-631}
    validate_ip --backend "$PRINTER_IP"
    (( backend_port <= 65535 )) || die 'The backend port must be between 1 and 65535.'
}

check_windows_port() {
    if [[ $LISTEN_PORT == 9100 && ( $WINDOWS_LAN == on || -n $WINDOWS_CLIENT ) ]]; then
        die 'Choose an IPP listening port other than 9100 when Windows RAW printing is enabled.'
    fi
}

read_options() {
    local wifi_ip= eth_ip= option value windows_option=
    WIFI_IFACE= ETH_IFACE= ETH_ALIAS= GATEWAY= BACKEND= WINDOWS_CLIENT=
    WINDOWS_LAN=off
    LISTEN_PORT=8631
    PRINT_NAME='Ethernet Printer'
    while (( $# )); do
        option=$1
        (( $# >= 2 )) || die "Missing value for $option."
        value=$2
        [[ -n $value ]] || die "Missing value for $option."
        shift 2
        case $option in
            --wifi-ip) wifi_ip=$value ;;
            --ethernet-ip) eth_ip=$value ;;
            --wifi-interface) WIFI_IFACE=$value ;;
            --ethernet-interface) ETH_IFACE=$value ;;
            --gateway) GATEWAY=$value ;;
            --alias) ETH_ALIAS=$value ;;
            --backend) BACKEND=$value ;;
            --name) PRINT_NAME=$value ;;
            --port) LISTEN_PORT=$value ;;
            --windows-client|--windows-lan)
                [[ -z $windows_option ]] || die 'Choose only one Windows printing option.'
                windows_option=$option
                set_windows_mode "$option" "$value"
                ;;
            *) die "Unknown option $option. Run bash install.sh --help." ;;
        esac
    done

    if [[ -n $wifi_ip ]]; then validate_ip --wifi-ip "$wifi_ip"; fi
    if [[ -n $eth_ip ]]; then validate_ip --ethernet-ip "$eth_ip"; fi
    validate_backend
    [[ $LISTEN_PORT =~ ^[1-9][0-9]{3,4}$ ]] && (( LISTEN_PORT >= 1024 && LISTEN_PORT <= 65535 )) ||
        die 'The Wi-Fi listening port must be between 1024 and 65535.'
    check_windows_port
    [[ ${#PRINT_NAME} -le 63 && $PRINT_NAME != *[[:cntrl:]]* ]] ||
        die 'The printer name must be at most 63 bytes and contain no control characters.'
    [[ -z $ETH_ALIAS || -z $GATEWAY ]] || die 'Choose either --alias or --gateway.'
    if [[ -n $ETH_ALIAS ]]; then
        validate_ip --alias "$ETH_ALIAS"
        [[ $ETH_ALIAS != "$PRINTER_IP" ]] || die 'The Ethernet alias and printer need separate addresses.'
    fi
    if [[ -n $GATEWAY ]]; then
        validate_ip --gateway "$GATEWAY"
        [[ $GATEWAY != "$PRINTER_IP" ]] || die 'The router and printer need separate addresses.'
    fi

    select_interfaces "$wifi_ip" "$eth_ip" || exit 1
    [[ -z $(interfaces_for_ip "$PRINTER_IP") ]] || die 'The printer IP is already assigned to this Mac.'
    bridge_args
}

describe_windows_mode() {
    if [[ ${WINDOWS_LAN:-off} == on ]]; then
        say 'Windows RAW printing is configured for the Wi-Fi subnet.'
    elif [[ -n ${WINDOWS_CLIENT:-} ]]; then
        say "Windows RAW printing on TCP port 9100 is limited to $WINDOWS_CLIENT."
    else
        say 'Windows RAW printing is disabled.'
    fi
}

describe_setup() {
    say "Wi-Fi $WIFI_IFACE ($(interface_ipv4 "$WIFI_IFACE"))"
    say "Ethernet $ETH_IFACE ($(interface_ipv4 "$ETH_IFACE"))"
    say "Printer $BACKEND"
    if [[ -n $GATEWAY ]]; then
        say "Requested Ethernet route through $GATEWAY."
    elif [[ -n $ETH_ALIAS ]]; then
        say "Requested direct Ethernet route with local source $ETH_ALIAS."
    fi
    describe_windows_mode
}

discover() {
    local wifi
    wifi=$(wifi_hardware_interface) || wifi=
    [[ -n $wifi ]] || die 'Could not identify this Mac’s Wi-Fi hardware to exclude it from Ethernet discovery.'
    if (( $# != 0 )); then
        [[ $# == 2 && $1 == --ethernet-interface && $2 =~ ^en[0-9]+$ && $2 != "$wifi" ]] ||
            die 'Discover accepts only --ethernet-interface with an Ethernet adapter name such as en1.'
    fi
    [[ -x $PACKAGE_DIR/print-bridge ]] || die 'Keep the print-bridge executable alongside install.sh.'
    "$PACKAGE_DIR/print-bridge" discover --wifi-interface "$wifi" "$@"
}

diagnose() {
    local requested_alias
    read_options "$@"
    describe_setup
    [[ -x $PACKAGE_DIR/print-bridge ]] || die 'Keep the print-bridge executable alongside install.sh.'
    requested_alias=$ETH_ALIAS
    if [[ -n $ETH_ALIAS && -z $(alias_netmask) ]]; then
        say 'The requested alias is currently absent. Probing Ethernet with its current source address.'
        # The configured source cannot be bound until the alias exists.
        ETH_ALIAS=
        bridge_args
        ETH_ALIAS=$requested_alias
    fi
    if "$PACKAGE_DIR/print-bridge" probe "${BRIDGE_ARGS[@]}"; then
        return 0
    fi
    error 'The IPP probe could not reach a usable printer over Ethernet.'
    if [[ -n $GATEWAY || -n $ETH_ALIAS ]]; then
        error 'Diagnose uses existing network settings. Install applies the requested Ethernet route before probing.'
    elif direct_printer_route "$ETH_IFACE"; then
        error "The routing table puts $PRINTER_IP directly on Ethernet. Check that the printer is awake and that its IPP service is available at $BACKEND."
    else
        error 'If the printer is behind an Ethernet router, provide --gateway. For a printer on the Ethernet segment, provide a reserved address using --alias.'
    fi
    return 1
}

write_settings() {
    local setting
    for setting in "$@"; do
        printf '%s=%q\n' "$setting" "${!setting}"
    done
}

write_config() {
    write_settings WIFI_IFACE ETH_IFACE BACKEND PRINTER_IP PRINT_NAME LISTEN_PORT GATEWAY ETH_ALIAS WINDOWS_CLIENT WINDOWS_LAN
}

write_plist() {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$STATE_DIR/service.sh</string>
    </array>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>$LOG</string>
    <key>StandardErrorPath</key>
    <string>$LOG</string>
    <key>Umask</key>
    <integer>63</integer>
</dict>
</plist>
EOF
}

# Return 0 for loaded, 1 for confirmed absent, or 2 for an unknown state.
query_service() {
    local output status=0
    output=$(/bin/launchctl print "system/$LABEL" 2>&1) || status=$?
    if (( status == 0 )); then
        return 0
    fi
    if (( status == 113 )) || [[ $output == *"Could not find service \"$LABEL\" in domain for system"* ]]; then
        return 1
    fi
    error "Could not query launchd service $LABEL (exit $status)."
    if [[ -n $output ]]; then error "$output"; fi
    return 2
}

wait_for_service_state() {
    local expected=$1 state attempt
    # bootout can return before launchd has finished stopping the process.
    for (( attempt = 0; attempt <= 80; attempt++ )); do
        state=0
        query_service || state=$?
        if (( state == expected )); then return 0; fi
        if (( state == 2 )); then return 2; fi
        if (( attempt < 80 )); then /bin/sleep 0.25 || return 2; fi
    done
    return 1
}

stop_service() {
    local state=0 bootout_status=0 bootout_output= wait_status=0
    query_service || state=$?
    if (( state == 1 )); then return 0; fi
    if (( state == 2 )); then return 1; fi

    bootout_output=$(/bin/launchctl bootout "system/$LABEL" 2>&1) || bootout_status=$?
    # A job can disappear between print and bootout. Confirm the resulting state.
    wait_for_service_state 1 || wait_status=$?
    if (( wait_status == 0 )); then return 0; fi
    if (( bootout_status != 0 )); then
        error "launchctl bootout failed for $LABEL (exit $bootout_status)."
        if [[ -n $bootout_output ]]; then error "$bootout_output"; fi
    fi
    if (( wait_status == 1 )); then
        error "Launchd still reports $LABEL loaded after waiting 20 seconds."
    fi
    return 1
}

start_service() {
    local wait_status=0
    /bin/launchctl bootstrap system "$PLIST" || return 1
    wait_for_service_state 0 || wait_status=$?
    if (( wait_status == 0 )); then return 0; fi
    if (( wait_status == 1 )); then
        error "Launchd did not report $LABEL loaded after waiting 20 seconds."
    fi
    return 1
}

install_payload() {
    local destination=$1 file
    for file in print-bridge install.sh service.sh; do
        /usr/bin/install -o root -g wheel -m 0755 "$PACKAGE_DIR/$file" "$destination/$file" || return 1
    done
}

finish_upgrade() {
    local result=$1 rollback_failed=no file state=0
    trap - EXIT INT TERM
    if (( result != 0 )) && [[ $files_changed == yes ]]; then
        if stop_service; then
            for file in "${upgrade_files[@]}"; do
                /bin/cp -p "$backup/$file" "$STATE_DIR/$file" || rollback_failed=yes
            done
        else
            rollback_failed=yes
        fi
    fi
    if (( result != 0 )) && [[ $rollback_failed == no && $was_loaded == yes ]]; then
        query_service || state=$?
        case $state in
            1) start_service || rollback_failed=yes ;;
            2) rollback_failed=yes ;;
        esac
    fi
    if [[ $rollback_failed == yes ]]; then
        error "The update could not restore the previous installation. Its backup is in $backup."
    else
        for file in "${upgrade_files[@]}"; do
            /bin/rm -f "$staging/$file" "$backup/$file" || true
        done
        /bin/rmdir "$backup" "$staging" 2>/dev/null || true
        if (( result != 0 )) && [[ $files_changed == yes ]]; then
            error 'The previous installation was restored.'
        fi
    fi
    exit "$result"
}

upgrade_bridge() (
    local windows_option= windows_value= requested_backend= requested_ethernet= option value
    local previous_client previous_lan previous_backend previous_ethernet previous_ip saved_windows_mode=yes
    local staging backup was_loaded=no files_changed=no pending_change=no printer_changed=no file state=0
    local upgrade_files=(print-bridge install.sh service.sh config.sh)
    while (( $# )); do
        option=$1
        (( $# >= 2 )) || die "Missing value for $option."
        value=$2
        [[ -n $value ]] || die "Missing value for $option."
        shift 2
        case $option in
            --backend) requested_backend=$value ;;
            --ethernet-interface) requested_ethernet=$value ;;
            --windows-client|--windows-lan)
                [[ -z $windows_option ]] || die 'Choose only one Windows printing option.'
                windows_option=$option
                windows_value=$value
                ;;
            *) die 'An in-place update accepts --backend, --ethernet-interface, and a Windows printing option. To change other settings, uninstall and install again.' ;;
        esac
    done
    [[ -x $PACKAGE_DIR/print-bridge ]] || die 'Keep the print-bridge executable alongside install.sh.'
    unset WINDOWS_CLIENT WINDOWS_LAN
    # Existing versions may omit WINDOWS_CLIENT or WINDOWS_LAN.
    # shellcheck source=/dev/null
    source "$CONFIG"
    [[ ${WINDOWS_CLIENT+x} == x && ${WINDOWS_LAN+x} == x ]] || saved_windows_mode=no
    WINDOWS_CLIENT=${WINDOWS_CLIENT:-}
    WINDOWS_LAN=${WINDOWS_LAN:-off}
    previous_client=$WINDOWS_CLIENT
    previous_lan=$WINDOWS_LAN
    previous_backend=$BACKEND
    previous_ethernet=$ETH_IFACE
    previous_ip=$PRINTER_IP
    if [[ -n $windows_option ]]; then
        set_windows_mode "$windows_option" "$windows_value"
        check_windows_port
    fi
    if [[ -n $requested_backend ]]; then
        BACKEND=$requested_backend
        validate_backend
    fi
    if [[ -n $requested_ethernet ]]; then
        ETH_IFACE=$requested_ethernet
    fi
    if [[ $BACKEND != "$previous_backend" || $ETH_IFACE != "$previous_ethernet" ]]; then
        if [[ ( -n $GATEWAY || -n $ETH_ALIAS || -e $OWN_ROUTE || -e $OWN_ALIAS ) &&
              ( $PRINTER_IP != "$previous_ip" || $ETH_IFACE != "$previous_ethernet" ) ]]; then
            die 'The installed bridge has a custom Ethernet route. Uninstall it before changing printer IP or Ethernet adapter.'
        fi
        select_interfaces '' '' || exit 1
        [[ -z $(interfaces_for_ip "$PRINTER_IP") ]] || die 'The printer IP is already assigned to this Mac.'
        printer_changed=yes
    fi

    staging=$(/usr/bin/mktemp -d "$STATE_DIR/.upgrade.XXXXXXXX") || die 'Could not stage the bridge update.'
    backup="$staging/previous"
    trap 'finish_upgrade "$?"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    /bin/mkdir -m 0700 "$backup" || die 'Could not prepare an update backup.'
    for file in "${upgrade_files[@]}"; do
        /bin/cp -p "$STATE_DIR/$file" "$backup/$file" || die "Could not back up $file."
    done
    install_payload "$staging" || die 'Could not stage the updated bridge files.'
    if [[ $printer_changed == no && $WINDOWS_CLIENT == "$previous_client" && $WINDOWS_LAN == "$previous_lan" &&
          ( -z $windows_option || $saved_windows_mode == yes ) ]]; then
        /bin/cp -p "$backup/config.sh" "$staging/config.sh" || die 'Could not stage the bridge configuration.'
    else
        (umask 077
         /usr/bin/awk '!/^(BACKEND|PRINTER_IP|ETH_IFACE|WINDOWS_CLIENT|WINDOWS_LAN)=/' "$backup/config.sh" > "$staging/config.sh" &&
         write_settings BACKEND PRINTER_IP ETH_IFACE WINDOWS_CLIENT WINDOWS_LAN >> "$staging/config.sh") ||
            die 'Could not stage the bridge configuration.'
    fi

    query_service || state=$?
    case $state in
        0) was_loaded=yes ;;
        2) die 'Launchd service state is unknown. The installed bridge was kept.' ;;
    esac
    for file in "${upgrade_files[@]}"; do
        if ! /usr/bin/cmp -s "$staging/$file" "$backup/$file"; then
            pending_change=yes
            break
        fi
    done
    if [[ $pending_change == no && $was_loaded == yes ]]; then
        say "$PRINT_NAME is already up to date."
        say "Verified launchd service $LABEL."
        return 0
    fi
    if [[ $printer_changed == yes || ( -n $windows_option && ( $WINDOWS_LAN == on || -n $WINDOWS_CLIENT ) ) ]]; then
        /bin/chmod 0755 "$staging" || die 'Could not prepare the printer probe.'
        bridge_args
        as_service_user "$staging/print-bridge" probe "${BRIDGE_ARGS[@]}" ||
            die 'The updated settings could not reach a compatible Ethernet printer. The previous bridge installation was kept.'
    fi

    if [[ $pending_change == no ]]; then
        start_service || die 'Could not start the bridge.'
        say "Started $PRINT_NAME."
        say "Verified launchd service $LABEL."
        return 0
    fi
    stop_service || die 'Could not stop the existing bridge. Its files were kept.'
    files_changed=yes
    for file in "${upgrade_files[@]}"; do
        /bin/mv -f "$staging/$file" "$STATE_DIR/$file" || die "Could not update $file."
    done
    start_service || die 'Could not start the updated bridge.'
    say "Updated $PRINT_NAME. The bridge starts automatically when someone logs in to this Mac."
    say "Verified launchd service $LABEL."
    describe_windows_mode
)

remove_files() {
    /bin/rm -f "$PLIST" "$STATE_DIR/launchd.plist" "$STATE_DIR/install.sh" "$STATE_DIR/service.sh" \
        "$CONFIG" "$BRIDGE_BIN" "$LOG" "$OWN_ALIAS" "$OWN_ROUTE" || return 1
    /bin/rmdir "$STATE_DIR" 2>/dev/null || true
}

rollback_install() {
    local result=$?
    trap - EXIT
    if (( result != 0 )); then
        if stop_service && remove_owned_routing && remove_files; then
            error 'Installation stopped and rolled back.'
        else
            error "Rollback needs attention. Run sudo bash install.sh uninstall to retry."
        fi
    fi
    exit "$result"
}

install_bridge() {
    needs_root
    if [[ -f $CONFIG && -x $BRIDGE_BIN && -f $PLIST ]]; then
        upgrade_bridge "$@"
        return $?
    fi
    [[ ! -e $STATE_DIR && ! -L $STATE_DIR && ! -e $PLIST && ! -L $PLIST ]] ||
        die 'A bridge installation already has files on this Mac. Run uninstall before retrying.'
    [[ -x $PACKAGE_DIR/print-bridge ]] || die 'Keep the print-bridge executable alongside install.sh.'
    read_options "$@"
    describe_setup

    /bin/mkdir -m 0755 "$STATE_DIR"
    trap rollback_install EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    (umask 077; write_config > "$CONFIG"; : > "$LOG")
    install_payload "$STATE_DIR"
    ensure_routing || die 'Could not prepare the Ethernet route.'
    if ! as_service_user "$BRIDGE_BIN" probe "${BRIDGE_ARGS[@]}"; then
        if [[ -z $GATEWAY && -z $ETH_ALIAS ]] && direct_printer_route "$ETH_IFACE"; then
            die "The routing table puts $PRINTER_IP directly on Ethernet. Check that the printer is awake and that its IPP service is available at $BACKEND."
        fi
        die 'The Ethernet IPP probe failed. Verify the router or alias and the printer’s IPP endpoint.'
    fi

    write_plist > "$STATE_DIR/launchd.plist"
    /usr/bin/plutil -lint "$STATE_DIR/launchd.plist" >/dev/null
    /usr/bin/install -o root -g wheel -m 0644 "$STATE_DIR/launchd.plist" "$PLIST"
    /bin/rm "$STATE_DIR/launchd.plist"
    /bin/launchctl enable "system/$LABEL"
    start_service
    trap - EXIT INT TERM
    say "Installed $PRINT_NAME. The bridge starts automatically when someone logs in to this Mac."
    say "Verified launchd service $LABEL."
}

bridge_status() {
    needs_root
    [[ -f $CONFIG ]] || die 'No bridge is installed.'
    # shellcheck source=/dev/null
    source "$CONFIG"
    describe_setup
    if [[ -n $GATEWAY || -n $ETH_ALIAS ]]; then
        if matches_route; then
            say 'The scoped Ethernet route is active.'
        else
            say 'The scoped Ethernet route is currently unavailable.'
        fi
    fi
    if ! console_available; then
        say 'The bridge is waiting for a graphical login.'
    fi
    local job
    if job=$(/bin/launchctl print "system/$LABEL" 2>/dev/null); then
        say 'The launchd supervisor is loaded.'
        /usr/bin/awk '/^[[:space:]]+(state|pid) = / { print }' <<< "$job"
    else
        say 'The launchd supervisor is currently unloaded.'
    fi
    if [[ -s $LOG ]]; then
        say ''
        say 'Recent bridge log'
        /usr/bin/tail -n 20 "$LOG"
    fi
}

uninstall_bridge() {
    needs_root
    if [[ ! -e $STATE_DIR && ! -e $PLIST ]]; then
        say 'No bridge is installed.'
        return 0
    fi
    stop_service || die 'Could not stop the launchd supervisor. Installation files have been kept.'
    if [[ -f $CONFIG ]]; then
        # shellcheck source=/dev/null
        source "$CONFIG"
        remove_owned_routing || die 'Could not remove the bridge’s Ethernet changes. Installation files have been kept.'
    fi
    remove_files
    say 'Removed the Wi-Fi Print Bridge.'
}

run_state_change() {
    local action=$1 result=0
    shift
    needs_root
    if [[ ${WIFI_PRINT_BRIDGE_LOCKED:-} == 1 ]]; then
        case $action in
            install) install_bridge "$@" ;;
            uninstall) uninstall_bridge ;;
        esac
    else
        WIFI_PRINT_BRIDGE_LOCKED=1 /usr/bin/lockf -k -t 0 "/private/var/run/$LABEL.lock" \
            /bin/bash "$PACKAGE_DIR/install.sh" "$action" "$@" || result=$?
        (( result != 75 )) || die 'Another bridge install, update, or removal is already in progress.'
        return "$result"
    fi
}

command=${1:---help}
if (( $# )); then shift; fi
case $command in
    -h|--help|help) usage ;;
    discover|diagnose|install|status|uninstall)
        [[ $(/usr/bin/uname -s) == Darwin ]] || die 'Run this installer on the target Mac.'
        if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
        case $command in
            discover) discover "$@" ;;
            diagnose) diagnose "$@" ;;
            install) run_state_change install "$@" ;;
            status) (( $# == 0 )) || die 'Status does not take options.'; bridge_status ;;
            uninstall) (( $# == 0 )) || die 'Uninstall does not take options.'; run_state_change uninstall ;;
        esac
        ;;
    *) usage >&2; exit 1 ;;
esac
