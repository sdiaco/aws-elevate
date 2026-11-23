# Security policy

aws-elevate hands out AWS permissions, so a vulnerability in it matters. Thank you for reporting one.

## Report a vulnerability

Report it privately: open the [Security tab](https://github.com/sdiaco/aws-elevate/security) of this repository and choose **Report a vulnerability**.

Do not open a public issue, pull request or discussion for a vulnerability.

Include what you can:

- the version: `aws-elevate --version`, and the infrastructure version if you know it
- what an attacker can do, and with which starting access
- the steps to reproduce it

## What happens next

- You get an answer within 7 days.
- Once the issue is confirmed, the fix is released as a new version and published as a GitHub security advisory. You are credited, unless you prefer not to be.

## Supported versions

Only the latest release receives fixes. Update with `aws-elevate update`.

## Scope

In scope: the client (`aws-elevate`), the service Lambdas, the CloudFormation templates and the release artifacts.

Out of scope: AWS services themselves (report them to [AWS](https://aws.amazon.com/security/vulnerability-reporting/)) and installations that changed the templates.

For how Elevate protects access and data, see [docs/security.md](docs/security.md).
