#!/usr/bin/env bash
# Disposable Linux CI hosts only: installs the managed service at its real paths.
set -euo pipefail
[[ "${TELRAD_NATIVE_INSTALL_TEST:-}" == 1 ]] || { echo 'Set TELRAD_NATIVE_INSTALL_TEST=1 on a disposable native test host.' >&2; exit 1; }
fixture="$(mktemp -d)"
trap 'sudo systemctl stop telrad-relay.service >/dev/null 2>&1 || true; sudo rm -f /etc/systemd/system/telrad-relay.service.d/coverage.conf; sudo systemctl daemon-reload; rm -rf "$fixture"' EXIT
scripts/build-release.sh 0.0.0-ci.1
cp packaging/install.sh packaging/relay.example.json "$fixture/"
cp dist/0.0.0-ci.1/installation-manifest.json "$fixture/"
cp dist/0.0.0-ci.1/telrad-relay-linux-amd64 "$fixture/telrad-relay"
coverage_directory="${RELAY_NATIVE_COVERAGE_DIR:-}"
if [[ -n "$coverage_directory" ]]; then
    mkdir -p "$coverage_directory"
    chmod 0777 "$coverage_directory"
    go build -cover -covermode=atomic -ldflags '-X main.version=0.0.0-ci.1' -o "$fixture/telrad-relay" ./cmd/telrad-relay
    sudo mkdir -p /etc/systemd/system/telrad-relay.service.d
    printf '[Service]\nEnvironment="GOCOVERDIR=%s"\nReadWritePaths=%s\n' "$coverage_directory" "$coverage_directory" | sudo tee /etc/systemd/system/telrad-relay.service.d/coverage.conf >/dev/null
fi
printf '%s\n' '{"schemaVersion":1,"channel":"stable","manifestUrl":"https://example.invalid/stable.json","publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}' > "$fixture/update-trust.json"
(cd "$fixture" && sudo env GOCOVERDIR="$coverage_directory" ./install.sh)
for attempt in {1..30}; do
    if sudo -u nobody env GOCOVERDIR="$coverage_directory" /usr/local/bin/telrad status > "$fixture/status"; then break; fi
    sleep 1
done
grep -Fq 'ingest ready: false' "$fixture/status"
[[ "$(systemctl show -p User --value telrad-relay.service)" == telrad-relay ]]
[[ "$(systemctl show -p NoNewPrivileges --value telrad-relay.service)" == yes ]]
! sudo -u nobody test -r /etc/telrad-relay/relay.json
! sudo -u telrad-relay test -w /usr/local/lib/telrad-relay/telrad
! sudo -u telrad-relay test -w /usr/local/lib/telrad-relay/update-trust.json
! sudo -u nobody env GOCOVERDIR="$coverage_directory" /usr/local/bin/telrad native-action enroll
sudo -u nobody python3 - <<'PY'
import json, socket
with socket.socket(socket.AF_UNIX) as connection:
    connection.settimeout(5)
    connection.connect('/run/telrad-relay/management.sock')
    connection.sendall(b'{"version":1,"action":"enroll"}\n')
    response = json.loads(connection.makefile().readline())
    assert response['done'] and 'administrator' in response['error'], response
PY
! sudo env GOCOVERDIR="$coverage_directory" /usr/local/bin/telrad run
sudo env GOCOVERDIR="$coverage_directory" /usr/local/bin/telrad native-action restart
sudo sed -i 's/"reportPort":2576/"reportPort":32576/' /etc/telrad-relay/relay.json
(cd "$fixture" && sudo env GOCOVERDIR="$coverage_directory" ./install.sh)
sudo grep -Fq '"reportPort":32576' /etc/telrad-relay/relay.json
sudo python3 - "$fixture/installation-manifest.json" /usr/local/lib/telrad-relay/installation.json <<'PY'
import json, sys
with open(sys.argv[1]) as source, open(sys.argv[2]) as installed:
    assert json.load(source) == json.load(installed), 'Installed component manifest changed.'
PY
sudo systemctl stop telrad-relay.service
(cd "$fixture" && sudo env GOCOVERDIR="$coverage_directory" ./install.sh)
! systemctl is-active --quiet telrad-relay.service
# A compromised service can place a link in its state directory. Neither an
# installation repair nor later elevated diagnostics may follow that link.
printf '%s' 'outside sentinel' > "$fixture/sentinel"
sudo mv /etc/telrad-relay/relay.json /etc/telrad-relay/relay.saved
sudo -u telrad-relay ln -s "$fixture/sentinel" /etc/telrad-relay/relay.json
if (cd "$fixture" && sudo env GOCOVERDIR="$coverage_directory" ./install.sh); then echo 'Installer accepted a state symlink.' >&2; exit 1; fi
[[ "$(cat "$fixture/sentinel")" == 'outside sentinel' ]]
sudo rm /etc/telrad-relay/relay.json
sudo mv /etc/telrad-relay/relay.saved /etc/telrad-relay/relay.json
sudo chown telrad-relay:telrad-relay /usr/local/lib/telrad-relay/update-trust.json
if (cd "$fixture" && sudo env GOCOVERDIR="$coverage_directory" ./install.sh); then echo 'Installer trusted a service-owned rollback input.' >&2; exit 1; fi
sudo chown root:root /usr/local/lib/telrad-relay/update-trust.json
next_binary="$fixture/telrad-next"
if [[ -n "$coverage_directory" ]]; then
    go build -cover -covermode=atomic -ldflags '-X main.version=0.0.0-ci.2' -o "$next_binary" ./cmd/telrad-relay
else
    go build -ldflags '-X main.version=0.0.0-ci.2' -o "$next_binary" ./cmd/telrad-relay
fi
go test -c -o "$fixture/native-lifecycle.test" ./cmd/telrad-relay
sudo env TELRAD_NATIVE_LIFECYCLE_TEST=1 TELRAD_NATIVE_NEXT_BINARY="$next_binary" GOCOVERDIR="$coverage_directory" \
    "$fixture/native-lifecycle.test" -test.run '^TestNative(InstalledLifecycle|SystemRollback)$' -test.v -test.timeout=5m
echo 'Native Linux installation and privilege boundary checks passed.'
