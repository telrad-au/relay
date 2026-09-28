#!/bin/sh
# Telrad Relay installer for Linux with systemd. Run as root.
#
#   curl -fsSL https://github.com/telrad-au/relay/releases/latest/download/install.sh | sudo sh
#
# Without a version it installs the release it was downloaded from, so an
# exact-tag installer installs that tag. Install or upgrade to another version
# by passing it as the argument (or TELRAD_RELAY_VERSION). Rerunning the
# installer is the only upgrade path:
#
#   curl -fsSL https://github.com/telrad-au/relay/releases/latest/download/install.sh | sudo sh -s -- 1.2.3
#
# On a test host, main installs the newest main build and main-842 main build
# 842. Main builds are unsigned development prereleases that enrol with the
# development Telrad and are not for clinical use. They are found through the
# anonymous, rate-limited GitHub API, which TELRAD_RELAY_RELEASES_API replaces
# for installer tests (file:// URLs need curl):
#
#   curl -fsSL https://raw.githubusercontent.com/telrad-au/relay/main/packaging/install.sh | sudo sh -s -- main
#   curl -fsSL https://raw.githubusercontent.com/telrad-au/relay/main/packaging/install.sh | sudo sh -s -- main-842
#
# The installer downloads the release binary for this architecture and the
# systemd unit, verifies both against the release's SHA256SUMS, creates the
# telrad-relay system user, installs /usr/local/lib/telrad-relay/telrad (linked
# from /usr/local/bin/telrad), enables telrad-relay.service and starts it on a
# first install or restarts it if it was running. A service that an operator
# stopped stays stopped.
#
# The installer writes /etc/telrad-relay/relay.json only when absent and never
# changes it afterwards; telrad report-receiver does. Its reportHost and
# reportPort are the clinic report receiver. On the first install the installer asks for them on the terminal
# (it reads /dev/tty, so this works through a pipe), or takes them from
# TELRAD_RELAY_REPORT_HOST and TELRAD_RELAY_REPORT_PORT (default 2576):
#
#   curl -fsSL .../install.sh | sudo TELRAD_RELAY_REPORT_HOST=192.0.2.20 sh
#
# With no terminal and no TELRAD_RELAY_REPORT_HOST, or when the question is
# left empty, it writes report-receiver.invalid, a name that can never resolve
# (RFC 6761): pairing and order forwarding work, and every report is answered
# AE so Telrad keeps it and retries until the operator sets the real receiver
# with sudo telrad report-receiver HOST[:PORT]. telrad status shows the
# receiver as NOT CONFIGURED.
#
# TELRAD_RELAY_RELEASE_URL replaces the GitHub release directory, even after a
# main build was found; it exists for installer tests against a locally built
# release (file:// URLs need curl).
#
# Remove Relay with sudo telrad uninstall, which keeps the configuration and
# data directory for a reinstall; sudo telrad uninstall --purge deletes them.
set -eu

repository=https://github.com/telrad-au/relay
# scripts/build-release.sh sets the release's tag here in the shipped copy; the
# empty source copy installs the latest release.
release_tag=
unit=/etc/systemd/system/telrad-relay.service
config=/etc/telrad-relay/relay.json
target=/usr/local/lib/telrad-relay/telrad
placeholder=report-receiver.invalid

fail() {
    echo "telrad-relay install: $*" >&2
    exit 1
}

fetch() {
    if command -v curl >/dev/null 2>&1; then
        curl --fail --silent --show-error --location --proto '=https,file' --output "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget --quiet --output-document "$2" "$1"
    else
        fail "curl or wget is required"
    fi
}

valid_host() {
    case "$1" in
        '' | [.-]* | *[!A-Za-z0-9.:_-]*) return 1 ;;
    esac
    [ "${#1}" -le 253 ]
}

valid_port() {
    case "$1" in
        '' | 0* | *[!0-9]*) return 1 ;;
    esac
    [ "${#1}" -le 5 ] && [ "$1" -le 65535 ]
}

# A terminal is available when /dev/tty opens; it does not under CI or a
# service manager, and stdin is the script itself under curl | sh.
have_terminal() {
    (: </dev/tty >/dev/tty) 2>/dev/null
}

# Asks for the clinic report receiver, re-asking until the answer is valid. An
# empty host (or end of input) leaves report_host empty so the placeholder is
# written.
ask_report_receiver() {
    echo "Relay delivers reports to the clinic RIS report receiver (MLLP)." >/dev/tty
    while :; do
        printf 'Report receiver host or IP address (leave empty to set it later): ' >/dev/tty
        IFS= read -r answer </dev/tty || { echo >/dev/tty; return 0; }
        [ -n "$answer" ] || return 0
        valid_host "$answer" && break
        echo "Enter a hostname or IP address, for example ris.clinic.local or 192.0.2.20." >/dev/tty
    done
    report_host=$answer
    while :; do
        printf 'Report receiver port [%s]: ' "$report_port" >/dev/tty
        IFS= read -r answer </dev/tty || answer=
        [ -n "$answer" ] || break
        if valid_port "$answer"; then
            report_port=$answer
            break
        fi
        echo "Enter a port from 1 to 65535." >/dev/tty
    done
}

# Sets tag to the newest main build, or to main build N for main-N, from the
# anonymous GitHub API (the newest 100 releases). One JSON member per line
# works for compact and pretty-printed responses without jq.
find_main_build() {
    api=${TELRAD_RELAY_RELEASES_API:-https://api.github.com/repos/telrad-au/relay/releases?per_page=100}
    fetch "$api" "$work/releases.json" ||
        fail "could not list releases from $api (anonymous GitHub API requests are rate limited; retry later if so)"
    # shellcheck disable=SC2020 # each of {, } and , becomes a newline
    tags=$(tr '{},' '\n\n\n' <"$work/releases.json" |
        sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\(main-[0-9][0-9]*-g[0-9a-f]\{7,40\}\)"[[:space:]]*$/\1/p' |
        sort -t - -k 2,2n)
    [ -n "$tags" ] || fail "no main builds were found"
    build=${1#main}
    build=${build#-}
    if [ -n "$build" ]; then
        tags=$(printf '%s\n' "$tags" | grep "^main-$build-g") || fail "main build $build was not found"
    fi
    tag=$(printf '%s\n' "$tags" | tail -n 1)
    echo "Telrad Relay main build: $tag"
}

# Everything runs from main so a truncated download through a pipe does nothing.
main() {
    [ "$(id -u)" -eq 0 ] || fail "run this installer as root"
    [ "$(uname -s)" = Linux ] || fail "this installer supports Linux only"
    [ -d /run/systemd/system ] || fail "systemd is required"
    command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"
    case "$(uname -m)" in
        x86_64 | amd64) arch=amd64 ;;
        aarch64 | arm64) arch=arm64 ;;
        *) fail "unsupported architecture $(uname -m)" ;;
    esac

    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    trap 'exit 130' INT TERM

    version=${1:-${TELRAD_RELAY_VERSION:-}}
    if printf '%s\n' "$version" | grep -Eqx 'main(-[1-9][0-9]*)?'; then
        find_main_build "$version"
        release_url="$repository/releases/download/$tag"
    elif [ -n "$version" ]; then
        version=${version#v}
        printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?' ||
            fail "version must look like 1.2.3, 1.2.3-rc.1, main or main-842"
        release_url="$repository/releases/download/v$version"
    elif [ -n "$release_tag" ]; then
        release_url="$repository/releases/download/$release_tag"
    else
        release_url="$repository/releases/latest/download"
    fi
    release_url=${TELRAD_RELAY_RELEASE_URL:-$release_url}

    report_host=${TELRAD_RELAY_REPORT_HOST:-}
    report_port=${TELRAD_RELAY_REPORT_PORT:-2576}
    if [ -n "$report_host" ] && ! valid_host "$report_host"; then
        fail "TELRAD_RELAY_REPORT_HOST must be a hostname or IP address"
    fi
    valid_port "$report_port" || fail "TELRAD_RELAY_REPORT_PORT must be a port from 1 to 65535"

    binary=telrad-relay-linux-$arch
    echo "Downloading Telrad Relay from $release_url"
    for file in SHA256SUMS "$binary" telrad-relay.service; do
        fetch "$release_url/$file" "$work/$file" || fail "could not download $file"
    done
    pattern="^[0-9a-f]{64}  ($binary|telrad-relay\\.service)\$"
    [ "$(grep -cE "$pattern" "$work/SHA256SUMS")" = 2 ] ||
        fail "SHA256SUMS does not list $binary and telrad-relay.service"
    grep -E "$pattern" "$work/SHA256SUMS" >"$work/selected"
    (cd "$work" && sha256sum -c selected >/dev/null) ||
        fail "checksum verification failed; nothing was installed"

    # Ask only when relay.json will be written; it is never changed afterwards.
    if [ -e "$config" ]; then
        if [ -n "${TELRAD_RELAY_REPORT_HOST:-}${TELRAD_RELAY_REPORT_PORT:-}" ]; then
            echo "$config already exists; TELRAD_RELAY_REPORT_HOST and TELRAD_RELAY_REPORT_PORT were ignored."
        fi
    elif [ -z "$report_host" ] && have_terminal; then
        ask_report_receiver
    fi
    report_host=${report_host:-$placeholder}

    fresh=true
    [ -e "$unit" ] && fresh=false
    running=false
    systemctl is-active --quiet telrad-relay.service && running=true

    # Stage the binary beside its target (temporary directories may be noexec)
    # and confirm it runs before replacing anything.
    install -d -m 0755 /usr/local/lib/telrad-relay /usr/local/bin /etc/telrad-relay
    install -m 0755 "$work/$binary" "$target.new"
    installed_version=$("$target.new" version) || {
        rm -f "$target.new"
        fail "the downloaded binary does not run on this host"
    }

    if ! getent passwd telrad-relay >/dev/null; then
        useradd --system --user-group --home-dir /var/lib/telrad-relay --no-create-home \
            --shell "$(command -v nologin || echo /bin/false)" telrad-relay
    fi
    mv -f "$target.new" "$target"
    ln -sfn "$target" /usr/local/bin/telrad

    if [ ! -e "$config" ]; then
        # Only reportHost is required; every other field takes its built-in
        # default. relay.json holds no secrets, so any local user can run
        # telrad status. The service's key and certificate live in its
        # private state directory, /var/lib/telrad-relay.
        printf '{\n  "schemaVersion": 6,\n  "reportHost": "%s",\n  "reportPort": %s\n}\n' \
            "$report_host" "$report_port" >"$config.new"
        chmod 0644 "$config.new"
        mv -f "$config.new" "$config"
    fi

    install -m 0644 "$work/telrad-relay.service" "$unit"
    systemctl daemon-reload
    systemctl enable --quiet telrad-relay.service
    if $running; then
        systemctl restart telrad-relay.service
    elif $fresh; then
        systemctl start telrad-relay.service
    else
        echo "telrad-relay.service was stopped and has been left stopped; start it with: telrad start"
    fi

    echo "Telrad Relay $installed_version installed."
    if grep -Fq "\"$placeholder\"" "$config"; then
        echo "WARNING: no report receiver is configured, so reports cannot be delivered."
        echo "Set the clinic report receiver with: sudo telrad report-receiver HOST[:PORT]"
    fi
    echo "Run telrad to see the pairing link."
}

main "$@"
