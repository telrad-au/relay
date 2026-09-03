#!/bin/sh
set -eu

had_installation=false
had_enrollment=false
service_was_active=false
[ -x /usr/local/lib/telrad-relay/telrad ] && had_installation=true
[ -x /var/lib/telrad-relay/bin/telrad ] && had_installation=true
[ -f /etc/telrad-relay/relay-credential.json ] && had_enrollment=true
if systemctl is-active --quiet telrad-relay.service; then
    service_was_active=true
fi

install -d -m 0700 /etc/telrad-relay
if ! id telrad-relay >/dev/null 2>&1; then
    useradd --system --home-dir /var/lib/telrad-relay --shell /usr/sbin/nologin telrad-relay
fi
install -d -m 0755 -o root -g root /usr/local/lib/telrad-relay
install -m 0755 -o root -g root telrad-relay /usr/local/lib/telrad-relay/telrad
install -m 0644 -o root -g root update-trust.json /usr/local/lib/telrad-relay/update-trust.json
ln -sfn /usr/local/lib/telrad-relay/telrad /usr/local/bin/telrad
if [ ! -f /etc/telrad-relay/relay.json ]; then
    install -m 0600 relay.example.json /etc/telrad-relay/relay.json
fi
if grep -Eq '"schemaVersion"[[:space:]]*:[[:space:]]*2' /etc/telrad-relay/relay.json; then
    /usr/local/lib/telrad-relay/telrad --config /etc/telrad-relay/relay.json migrate-config
fi
install -m 0600 installation-manifest.json /etc/telrad-relay/installation.json
chown -R telrad-relay:telrad-relay /etc/telrad-relay
install -m 0644 telrad-relay.service /etc/systemd/system/telrad-relay.service
systemctl daemon-reload
if [ "$service_was_active" = true ]; then
    systemctl restart telrad-relay.service
fi

installed_version="$(/usr/local/lib/telrad-relay/telrad version)"
echo "Telrad Relay $installed_version installed."
if [ "$had_enrollment" = true ]; then
    echo "Existing authentication preserved."
elif [ "$had_installation" = true ]; then
    echo "Existing configuration preserved."
fi
if [ "$service_was_active" = true ]; then
    echo "Service restarted successfully."
    echo "Relay is running."
elif [ "$had_enrollment" = true ]; then
    echo "The service remains stopped. Run 'telrad' to start it."
else
    echo "Run 'telrad' to authenticate this host and start the service."
fi
rm -f /var/lib/telrad-relay/bin/telrad >/dev/null 2>&1 || true
