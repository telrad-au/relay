# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities through [GitHub private vulnerability
reporting](https://github.com/telrad-au/relay/security/advisories/new).
This keeps the report and any follow-up discussion private between the reporter
and the Telrad maintainers.

Do not disclose vulnerability details in a public GitHub issue, discussion,
pull request, or other public forum. If the private reporting form is
unavailable, open a public issue containing no sensitive details and ask the
maintainers to establish a private contact channel.

Include as much of the following as is practical:

- the affected relay version, operating system, and installation method;
- a description of the vulnerability and its potential impact;
- reproducible steps, proof-of-concept code, logs, or relevant configuration;
- any mitigations or workarounds you have identified; and
- whether the vulnerability is known to be actively exploited or already
  publicly disclosed.

Use synthetic data when demonstrating an issue. Do not include patient or other
sensitive personal information, private keys, access tokens, or production
credentials in a report.

## Supported versions

The most recent published relay release is supported with security fixes.
This includes the most recent prerelease while no stable release is available.
Security fixes and response commitments target official releases. The Apache
License 2.0 permits development and locally modified builds, but Telrad may be
unable to reproduce or support changes outside an official release unless a
customer agreement states otherwise. Users should reproduce reports against
the latest official release when practical because the issue may already have
been corrected.

## Response timelines

Telrad aims to:

- acknowledge a report within three business days;
- provide an initial assessment within ten business days; and
- provide a status update at least every ten business days until the report is
  resolved or closed.

Remediation time depends on severity, exploitability, and the complexity of
shipping a safe fix. These targets are not a guarantee, but maintainers will
communicate material changes to the expected timeline through the private
report.

## Coordinated disclosure

Please keep the vulnerability and related communications private until a fix
or mitigation is available and Telrad and the reporter have agreed on a
disclosure date. Telrad will work with the reporter to validate the finding,
assess affected versions, prepare a fix, and publish an advisory when
appropriate. The target is coordinated disclosure within 90 days of the
initial report, but the parties may agree to a shorter or longer period based
on user safety, active exploitation, and release complexity.

Telrad will credit reporters in the published advisory when requested, unless
the reporter prefers to remain anonymous.
