# Support and compatibility

Production support is provided under the applicable Telrad customer agreement.
Public GitHub issues are suitable for reproducible defects that contain no
patient data, credentials, private infrastructure details, or vulnerability
information. Report security issues through the private process in
[`SECURITY.md`](SECURITY.md).

The most recent stable relay release is the supported production version.
Until a stable release exists, only the most recent prerelease is supported for
integration testing. Release notes identify any administrator action or
compatibility change required during an upgrade.

## Distributed platforms

The release workflow produces:

- Linux amd64 and arm64 static binaries for systemd-based installations. Linux
  prereleases and their GitHub Release installer use a development-only Ed25519
  trust root and an exact-tag update manifest;
- Windows amd64 binaries and a PowerShell service installer; and
- Linux amd64 and arm64 container images.

These artifact targets are not, by themselves, a promise that every operating
system version has completed production qualification. Telrad records the
qualified operating-system versions in customer release documentation before a
stable release. The Apache License 2.0 permits local modifications and
redistributed modified builds. Telrad support covers official releases unless
a customer agreement states otherwise; this support boundary does not restrict
the licence rights for modified builds.

The cloud control protocol is designed to remain backward compatible within
the supported release window. Customers must not assume indefinite
compatibility for an unmaintained relay. Native installations can use the
signed update channel; container operators remain responsible for replacing
the pinned image digest using their normal change process.

Development-signed Linux prereleases are for integration testing only. Their
trust root must never authorize production updates, and the checksum-only
Windows prerelease artifact is not a signed installer.
