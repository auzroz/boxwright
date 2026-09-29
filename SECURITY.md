# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for a security problem.

Use GitHub's private vulnerability reporting: open the **Security** tab of this
repository and choose **Report a vulnerability**. I aim to acknowledge a report
within a few days.

## Scope

Boxwright is self-hosted software that talks to your own Homebox and, optionally,
the Anthropic API using **your** key. Useful reports include anything that could
leak that key or your Homebox credentials, or let one device act on another's
server settings. Secrets are never stored in this repository; if you find one,
please report it the same way.

## Supported versions

Only the latest release receives fixes.
