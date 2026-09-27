#!/usr/bin/env bash
# Disposable Linux CI hosts only: installs, upgrades and removes the real
# telrad-relay systemd service with packaging/install.sh against locally built
# releases. It changes /etc, /usr/local and system users.
set -euo pipefail
[[ "${TELRAD_NATIVE_INSTALL_TEST:-}" == 1 ]] || {
    echo 'Set TELRAD_NATIVE_INSTALL_TEST=1 on a disposable native test host.' >&2
    exit 1
}
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
work="$(mktemp -d)"
chmod 0755 "$work"

uninstall() {
    sudo systemctl disable --now telrad-relay.service >/dev/null 2>&1 || true
    sudo rm -rf /etc/systemd/system/telrad-relay.service /usr/local/lib/telrad-relay \
        /usr/local/bin/telrad /etc/telrad-relay /var/lib/telrad-relay
    sudo systemctl daemon-reload
    if getent passwd telrad-relay >/dev/null; then sudo userdel telrad-relay; fi
}
trap 'uninstall; rm -rf "$work"' EXIT

# Bash ignores errexit for commands negated with !, so assert failures explicitly.
refute() {
    if "$@"; then
        echo "Unexpectedly succeeded: $*" >&2
        exit 1
    fi
}

# The enrolment endpoint never resolves, so CI never contacts Telrad.
export RELAY_ENROLMENT_URL=https://enrolment.invalid/v1/relay/enrolments
RELAY_RELEASE_DIR="$work/first" scripts/build-release.sh 0.0.0-ci.1 >/dev/null
RELAY_RELEASE_DIR="$work/second" scripts/build-release.sh 0.0.0-ci.2 >/dev/null

# setsid detaches the installer from any terminal, so it never asks for the
# report receiver and a run from an interactive shell behaves like CI.
install_relay() {
    local release="$1"
    shift
    sudo env TELRAD_RELAY_RELEASE_URL="file://$release" "$@" \
        setsid -w sh "$ROOT_DIR/packaging/install.sh" </dev/null
}

wait_for_status() {
    for _ in {1..30}; do
        if sudo -u nobody /usr/local/bin/telrad status > "$work/status" 2>&1 && grep -Fq "$1" "$work/status"; then
            return 0
        fi
        sleep 1
    done
    cat "$work/status" >&2
    echo "telrad status did not report: $1" >&2
    return 1
}

# A checksum mismatch installs nothing.
cp -R "$work/first" "$work/tampered"
printf 'x' >> "$work/tampered/telrad-relay-linux-amd64"
refute install_relay "$work/tampered"
[[ ! -e /usr/local/lib/telrad-relay/telrad && ! -e /etc/systemd/system/telrad-relay.service ]]
refute getent passwd telrad-relay >/dev/null

install_relay "$work/first" TELRAD_RELAY_REPORT_HOST=127.0.0.1 TELRAD_RELAY_REPORT_PORT=12576
wait_for_status 'state: pairing'
wait_for_status 'report receiver: 127.0.0.1:12576'
[[ "$(telrad version)" == 0.0.0-ci.1 ]]
[[ "$(readlink /usr/local/bin/telrad)" == /usr/local/lib/telrad-relay/telrad ]]
[[ "$(systemctl show -p User --value telrad-relay.service)" == telrad-relay ]]
[[ "$(systemctl show -p NoNewPrivileges --value telrad-relay.service)" == yes ]]
systemctl is-enabled --quiet telrad-relay.service
[[ "$(stat -c '%U:%G %a' /usr/local/lib/telrad-relay/telrad)" == 'root:root 755' ]]
[[ "$(stat -c '%U:%G %a' /etc/telrad-relay/relay.json)" == 'root:root 644' ]]
[[ "$(sudo stat -c '%U %a' /var/lib/telrad-relay)" == 'telrad-relay 700' ]]
grep -Fq '"reportHost": "127.0.0.1"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 12576' /etc/telrad-relay/relay.json
refute sudo -u telrad-relay test -w /usr/local/lib/telrad-relay/telrad
refute sudo -u telrad-relay test -w /etc/telrad-relay/relay.json

# Upgrade keeps configuration, even when a receiver is passed again, and
# restarts the running service on the new version.
sudo sed -i 's/"reportPort": 12576/"reportPort": 32576/' /etc/telrad-relay/relay.json
install_relay "$work/second" TELRAD_RELAY_REPORT_HOST=192.0.2.99
wait_for_status 'Telrad Relay 0.0.0-ci.2'
wait_for_status 'report receiver: 127.0.0.1:32576'
grep -Fq '"reportHost": "127.0.0.1"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 32576' /etc/telrad-relay/relay.json

# A deliberately stopped service stays stopped.
sudo systemctl stop telrad-relay.service
install_relay "$work/second"
refute systemctl is-active --quiet telrad-relay.service

# Without a terminal or TELRAD_RELAY_REPORT_HOST the installer writes the
# placeholder, warns, and status shows the receiver as not configured.
uninstall
install_relay "$work/first" >"$work/install.log"
grep -Fq 'WARNING: no report receiver is configured' "$work/install.log"
grep -Fq '"reportHost": "report-receiver.invalid"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 2576' /etc/telrad-relay/relay.json
wait_for_status 'report receiver: NOT CONFIGURED - edit reportHost in /etc/telrad-relay/relay.json and run telrad restart'

# The documented removal leaves nothing behind.
uninstall
[[ ! -e /usr/local/lib/telrad-relay && ! -e /usr/local/bin/telrad && ! -e /etc/telrad-relay && ! -e /var/lib/telrad-relay ]]
refute getent passwd telrad-relay >/dev/null
echo 'Native Linux installation checks passed.'
