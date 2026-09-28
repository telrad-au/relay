#!/usr/bin/env bash
# Disposable Linux CI hosts only: installs, upgrades and removes the real
# telrad-relay systemd service with packaging/install.sh against locally built
# releases, and exercises the telrad operator commands against it. It changes
# /etc, /usr/local and system users.
set -euo pipefail
[[ "${TELRAD_NATIVE_INSTALL_TEST:-}" == 1 ]] || {
    echo 'Set TELRAD_NATIVE_INSTALL_TEST=1 on a disposable native test host.' >&2
    exit 1
}
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
work="$(mktemp -d)"
chmod 0755 "$work"

# Removes everything by hand, so a failed run cleans up even without telrad.
cleanup() {
    sudo systemctl disable --now telrad-relay.service >/dev/null 2>&1 || true
    sudo rm -rf /etc/systemd/system/telrad-relay.service /usr/local/lib/telrad-relay \
        /usr/local/bin/telrad /etc/telrad-relay /var/lib/telrad-relay
    sudo systemctl daemon-reload
    if getent passwd telrad-relay >/dev/null; then sudo userdel telrad-relay; fi
    sudo rm -rf /srv/telrad-relay-custom
}
trap 'cleanup; rm -rf "$work"' EXIT

# Bash ignores errexit for commands negated with !, so assert failures explicitly.
refute() {
    if "$@"; then
        echo "Unexpectedly succeeded: $*" >&2
        exit 1
    fi
}

# The enrolment endpoint never resolves, so CI never contacts Telrad.
export RELAY_ENROLMENT_URL=https://enrolment.invalid/api/relay/enrolments
RELAY_RELEASE_DIR="$work/first" scripts/build-release.sh 0.0.0-ci.1 >/dev/null
RELAY_RELEASE_DIR="$work/second" scripts/build-release.sh 0.0.0-ci.2 >/dev/null
RELAY_RELEASE_TAG=main-7-gabcdef0 RELAY_RELEASE_DIR="$work/main" \
    scripts/build-release.sh 0.0.0-main.7.gabcdef0 >/dev/null

# Shipped installers default to their own release tag, the source installers
# keep the empty placeholder, and SHA256SUMS covers the rewritten copies.
has_line_once() {
    [[ "$(grep -cFx -- "$1" "$2")" == 1 ]]
}
has_line_once 'release_tag=' packaging/install.sh
has_line_once "\$releaseTag = ''" packaging/install.ps1
has_line_once 'release_tag=v0.0.0-ci.1' "$work/first/install.sh"
has_line_once "\$releaseTag = 'v0.0.0-ci.1'" "$work/first/install.ps1"
has_line_once 'release_tag=main-7-gabcdef0' "$work/main/install.sh"
has_line_once "\$releaseTag = 'main-7-gabcdef0'" "$work/main/install.ps1"
sh -n "$work/main/install.sh"
(cd "$work/main" && sha256sum --check --quiet SHA256SUMS)
refute env RELAY_RELEASE_TAG=main-7 RELAY_RELEASE_DIR="$work/invalid" \
    scripts/build-release.sh 0.0.0-ci.1 2>/dev/null

# Synthetic GitHub release listings for main build selection, compact and
# pretty-printed, with stable and rc tags; 100 must win numerically over 9 and 10.
cat >"$work/compact.json" <<'EOF'
[{"id":1,"tag_name":"main-9-g1111111","name":"Telrad Relay main build 9 (1111111)","prerelease":true,"body":"a, b"},{"id":2,"tag_name":"v2.1.0","prerelease":false},{"id":3,"tag_name":"main-100-gaaaaaaa","prerelease":true},{"id":4,"tag_name":"v2.1.0-rc.1","prerelease":true},{"id":5,"tag_name":"main-10-gbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","prerelease":true}]
EOF
cat >"$work/pretty.json" <<'EOF'
[
  {
    "id": 3,
    "tag_name": "main-10-gbbbbbbb",
    "prerelease": true
  },
  {
    "id": 2,
    "tag_name": "v9.0.0",
    "prerelease": false
  },
  {
    "id": 1,
    "tag_name": "main-100-gccccccc",
    "prerelease": true
  }
]
EOF
cat >"$work/none.json" <<'EOF'
[{"tag_name":"v2.1.0"},{"tag_name":"v2.1.0-rc.1"},{"tag_name":"main"},{"tag_name":"main-x-g1234567"}]
EOF

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

# Installs with a version that must be refused with the given message.
install_refused() {
    local message="$1"
    shift
    if install_relay "$@" >"$work/refused.log" 2>&1; then
        echo "Installer accepted: $*" >&2
        exit 1
    fi
    grep -Fq "$message" "$work/refused.log" || {
        cat "$work/refused.log" >&2
        echo "Installer refusal lacks: $message" >&2
        exit 1
    }
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
# The upgrade selects the newest main build from the listing; the release URL
# still supplies the files.
sudo sed -i 's/"reportPort": 12576/"reportPort": 32576/' /etc/telrad-relay/relay.json
install_relay "$work/second" TELRAD_RELAY_REPORT_HOST=192.0.2.99 TELRAD_RELAY_VERSION=main \
    TELRAD_RELAY_RELEASES_API="file://$work/compact.json" >"$work/install.log"
grep -Fqx 'Telrad Relay main build: main-100-gaaaaaaa' "$work/install.log"
wait_for_status 'Telrad Relay 0.0.0-ci.2'
wait_for_status 'report receiver: 127.0.0.1:32576'
grep -Fq '"reportHost": "127.0.0.1"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 32576' /etc/telrad-relay/relay.json

# A deliberately stopped service stays stopped. A requested main build is
# found in a pretty-printed listing.
sudo systemctl stop telrad-relay.service
install_relay "$work/second" TELRAD_RELAY_VERSION=main-10 \
    TELRAD_RELAY_RELEASES_API="file://$work/pretty.json" >"$work/install.log"
grep -Fqx 'Telrad Relay main build: main-10-gbbbbbbb' "$work/install.log"
refute systemctl is-active --quiet telrad-relay.service

# Missing or malformed main builds are refused before anything changes.
install_refused 'main build 11 was not found' "$work/first" TELRAD_RELAY_VERSION=main-11 \
    TELRAD_RELAY_RELEASES_API="file://$work/compact.json"
install_refused 'no main builds were found' "$work/first" TELRAD_RELAY_VERSION=main \
    TELRAD_RELAY_RELEASES_API="file://$work/none.json"
install_refused 'could not list releases' "$work/first" TELRAD_RELAY_VERSION=main \
    TELRAD_RELAY_RELEASES_API="file://$work/missing.json"
for version in main-010 mainx main-0 vmain; do
    install_refused 'version must look like 1.2.3, 1.2.3-rc.1, main or main-842' "$work/first" \
        TELRAD_RELAY_VERSION="$version" TELRAD_RELAY_RELEASES_API="file://$work/compact.json"
done
[[ "$(telrad version)" == 0.0.0-ci.2 ]]
refute systemctl is-active --quiet telrad-relay.service

# Operator commands that change the installation refuse without root, and
# ask for confirmation unless --yes; setsid leaves them without a terminal.
refute telrad uninstall --yes 2>"$work/refused.log"
grep -Fq 'telrad uninstall changes the installation; run it as root' "$work/refused.log"
refute sudo setsid -w telrad uninstall </dev/null >/dev/null 2>"$work/refused.log"
grep -Fq 'rerun with --yes' "$work/refused.log"
[[ -e /usr/local/lib/telrad-relay/telrad ]]

# uninstall --purge removes the stopped installation and everything it kept.
sudo telrad uninstall --purge --yes >"$work/uninstall.log"
grep -Fqx 'Telrad Relay was removed.' "$work/uninstall.log"
[[ ! -e /etc/systemd/system/telrad-relay.service && ! -e /usr/local/lib/telrad-relay && ! -e /usr/local/bin/telrad ]]
[[ ! -e /etc/telrad-relay && ! -e /var/lib/telrad-relay ]]
refute getent passwd telrad-relay >/dev/null

# Without a terminal or TELRAD_RELAY_REPORT_HOST the installer writes the
# placeholder, warns, and status shows the receiver as not configured.
install_relay "$work/first" >"$work/install.log"
grep -Fq 'WARNING: no report receiver is configured' "$work/install.log"
grep -Fqx 'Set the clinic report receiver with: sudo telrad report-receiver HOST[:PORT]' "$work/install.log"
grep -Fq '"reportHost": "report-receiver.invalid"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 2576' /etc/telrad-relay/relay.json
wait_for_status 'report receiver: NOT CONFIGURED - set it with: sudo telrad report-receiver HOST[:PORT]'

# report-receiver shows the receiver to anyone, and as root sets it, changing
# only reportHost and reportPort, keeping the file's owner and mode, and
# restarting the running service.
[[ "$(telrad report-receiver)" == $'report receiver: NOT CONFIGURED\nSet it with: sudo telrad report-receiver HOST[:PORT]' ]]
refute telrad report-receiver 127.0.0.1 2>/dev/null
for receiver in 'bad host' 127.0.0.1:0 127.0.0.1:65536 '[127.0.0.1]:2576' 2001:db8::1:2576x; do
    refute sudo telrad report-receiver "$receiver" 2>/dev/null
done
grep -Fq '"reportHost": "report-receiver.invalid"' /etc/telrad-relay/relay.json
sudo telrad report-receiver 127.0.0.1:12577 >"$work/receiver.log"
grep -Fqx 'Restarted telrad-relay.service; it now delivers reports to 127.0.0.1:12577.' "$work/receiver.log"
[[ "$(cat /etc/telrad-relay/relay.json)" == $'{\n  "schemaVersion": 6,\n  "reportHost": "127.0.0.1",\n  "reportPort": 12577\n}' ]]
[[ "$(stat -c '%U:%G %a' /etc/telrad-relay/relay.json)" == 'root:root 644' ]]
[[ "$(telrad report-receiver)" == 'report receiver: 127.0.0.1:12577' ]]
wait_for_status 'report receiver: 127.0.0.1:12577'
# Without a port the current one is kept; IPv6 takes brackets with a port.
sudo telrad report-receiver ::1 >/dev/null
wait_for_status 'report receiver: [::1]:12577'
sudo telrad report-receiver '[::1]:12578' >/dev/null
wait_for_status 'report receiver: [::1]:12578'
grep -Fq '"reportHost": "::1"' /etc/telrad-relay/relay.json
grep -Fq '"reportPort": 12578' /etc/telrad-relay/relay.json

# pair removes only identity.json. The enrolment URL never resolves, so the
# restarted service publishes no link: pair reports the pairing problem and
# fails after its bounded wait. A synthetic ledger entry, an open backlog
# window and an unreadable identity stand in for a paired Relay's data.
sudo systemctl stop telrad-relay.service
printf 'ACC-SYNTHETIC-1\n' | sudo tee -a /var/lib/telrad-relay/accessions.ledger >/dev/null
printf 'not an identity\n' | sudo install -o telrad-relay -g telrad-relay -m 0600 /dev/stdin /var/lib/telrad-relay/identity.json
refute telrad accept-backlog --hours 1 2>/dev/null
refute sudo test -e /var/lib/telrad-relay/accept-backlog.json
sudo telrad accept-backlog --hours 1 >/dev/null
refute sudo setsid -w telrad pair </dev/null >"$work/pair.log" 2>&1
grep -Fq 'rerun with --yes' "$work/pair.log"
sudo test -e /var/lib/telrad-relay/identity.json
if sudo telrad pair --yes >"$work/pair.log" 2>&1; then
    cat "$work/pair.log" >&2
    echo 'pair --yes succeeded without a reachable enrolment endpoint' >&2
    exit 1
fi
grep -Fq 'Removed the previous pairing (identity.json); the accession ledger is kept.' "$work/pair.log"
grep -Fq 'pairing problem: enrolment request failed' "$work/pair.log"
grep -Fq 'run telrad later to see the link' "$work/pair.log"
refute sudo test -e /var/lib/telrad-relay/identity.json
sudo grep -Fqx ACC-SYNTHETIC-1 /var/lib/telrad-relay/accessions.ledger
sudo test -e /var/lib/telrad-relay/accept-backlog.json
systemctl is-active --quiet telrad-relay.service

# uninstall keeps the configuration, data and service account, and a
# reinstall resumes with them.
sudo telrad uninstall --yes >"$work/uninstall.log"
grep -Fq 'Reinstall to resume with the kept files.' "$work/uninstall.log"
[[ ! -e /etc/systemd/system/telrad-relay.service && ! -e /usr/local/lib/telrad-relay && ! -e /usr/local/bin/telrad ]]
refute systemctl is-active --quiet telrad-relay.service
grep -Fq '"reportHost": "::1"' /etc/telrad-relay/relay.json
sudo grep -Fqx ACC-SYNTHETIC-1 /var/lib/telrad-relay/accessions.ledger
getent passwd telrad-relay >/dev/null
install_relay "$work/second" >"$work/install.log"
wait_for_status 'report receiver: [::1]:12578'
refute grep -Fq 'WARNING' "$work/install.log"
sudo grep -Fqx ACC-SYNTHETIC-1 /var/lib/telrad-relay/accessions.ledger

# uninstall --purge leaves nothing behind, except a dataDir moved away from
# the default, which it names and never deletes.
custom_data=/srv/telrad-relay-custom
sudo install -d -m 0700 "$custom_data"
sudo sed -i "s|\"schemaVersion\": 6,|\"schemaVersion\": 6,\n  \"dataDir\": \"$custom_data\",|" /etc/telrad-relay/relay.json
[[ "$(telrad report-receiver)" == 'report receiver: [::1]:12578' ]]
sudo telrad uninstall --purge --yes >"$work/uninstall.log"
grep -Fq "$custom_data, the dataDir set in relay.json, holds the pairing and ledger and is left in place." "$work/uninstall.log"
grep -Fq "sudo rm -rf '$custom_data'" "$work/uninstall.log"
[[ -d "$custom_data" ]]
sudo rmdir "$custom_data"
[[ ! -e /etc/systemd/system/telrad-relay.service && ! -e /usr/local/lib/telrad-relay && ! -e /usr/local/bin/telrad ]]
[[ ! -e /etc/telrad-relay && ! -e /var/lib/telrad-relay ]]
refute getent passwd telrad-relay >/dev/null
echo 'Native Linux installation checks passed.'
