# aws-elevate

Temporary elevated access (just-in-time) for AWS IAM Identity Center.

A user asks for a permission set on an account for a limited time, with a reason. Access is granted at once or after an approver says yes. When the time is over, the access is removed.

- License: Apache-2.0
- Version: 0.0.3

## What you need

- AWS IAM Identity Center, with the accounts of your organization assigned through it.
- The account that is the Identity Center **delegated administrator**. Elevate is installed there, in the Identity Center home region.
- Three Identity Center groups (create them first, any name):
  - **admins**: install and configure Elevate.
  - **requesters**: everyone who asks for access. Approvers need it too, unless they are admins.
  - **auditors** (optional): can read and export everything.
- [AWS CLI v2](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html).
- An AWS CLI SSO profile (see "Install Elevate").

Elevate runs on Linux and macOS. On Windows, use WSL.

## Install the client

```bash
curl -fsSL https://raw.githubusercontent.com/sdiaco/aws-elevate/main/install.sh | sh
```

The script downloads the executable for your system, checks its SHA-256 against the `checksums.txt` of the release, and installs it in `~/.local/bin/aws-elevate`. If the checksum does not match, nothing is installed. Add `~/.local/bin` to your `PATH` if the script says so.

Options, as environment variables: `VERSION=<x.y.z>` installs that version instead of the latest; `PREFIX=<folder>` installs there.

```bash
aws-elevate --version
```

## Install Elevate

Do this once for the organization, as an administrator. Elevate goes in the delegated administrator account. Two ways: the guided command, or a CloudFormation Launch Stack.

### With the client

1. Create an SSO profile for the delegated administrator account. Choose the account, the administrator role and any name:

   ```bash
   aws configure sso
   ```

2. Run the install:

   ```bash
   aws-elevate install --profile <name>
   ```

   It asks for the region (the Identity Center home region), the three groups, an alert e-mail and, optionally, the organization name. It shows a summary, asks for confirmation, creates the stack and then runs `aws-elevate setup`.

3. Confirm the subscription in the e-mail you receive, so alerts reach you.

For a script, pass everything as flags and add `--yes`:

```bash
aws-elevate install --profile <name> --region eu-west-1 \
  --admin-group <admin group> --requester-group <requester group> \
  --email user@example.com --yes
```

### With Launch Stack

Use it when you prefer the CloudFormation console. Sign in to the delegated administrator account, then open the link of the Identity Center home region:

| Region | |
|---|---|
| `ca-central-1` | [Launch Stack](https://ca-central-1.console.aws.amazon.com/cloudformation/home?region=ca-central-1#/stacks/create/review?templateURL=https://aws-elevate-releases-ca-central-1.s3.ca-central-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-central-1` | [Launch Stack](https://eu-central-1.console.aws.amazon.com/cloudformation/home?region=eu-central-1#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-central-1.s3.eu-central-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-north-1` | [Launch Stack](https://eu-north-1.console.aws.amazon.com/cloudformation/home?region=eu-north-1#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-north-1.s3.eu-north-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-south-1` | [Launch Stack](https://eu-south-1.console.aws.amazon.com/cloudformation/home?region=eu-south-1#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-south-1.s3.eu-south-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-west-1` | [Launch Stack](https://eu-west-1.console.aws.amazon.com/cloudformation/home?region=eu-west-1#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-west-1.s3.eu-west-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-west-2` | [Launch Stack](https://eu-west-2.console.aws.amazon.com/cloudformation/home?region=eu-west-2#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-west-2.s3.eu-west-2.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `eu-west-3` | [Launch Stack](https://eu-west-3.console.aws.amazon.com/cloudformation/home?region=eu-west-3#/stacks/create/review?templateURL=https://aws-elevate-releases-eu-west-3.s3.eu-west-3.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `sa-east-1` | [Launch Stack](https://sa-east-1.console.aws.amazon.com/cloudformation/home?region=sa-east-1#/stacks/create/review?templateURL=https://aws-elevate-releases-sa-east-1.s3.sa-east-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `us-east-1` | [Launch Stack](https://us-east-1.console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https://aws-elevate-releases-us-east-1.s3.us-east-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `us-east-2` | [Launch Stack](https://us-east-2.console.aws.amazon.com/cloudformation/home?region=us-east-2#/stacks/create/review?templateURL=https://aws-elevate-releases-us-east-2.s3.us-east-2.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `us-west-1` | [Launch Stack](https://us-west-1.console.aws.amazon.com/cloudformation/home?region=us-west-1#/stacks/create/review?templateURL=https://aws-elevate-releases-us-west-1.s3.us-west-1.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |
| `us-west-2` | [Launch Stack](https://us-west-2.console.aws.amazon.com/cloudformation/home?region=us-west-2#/stacks/create/review?templateURL=https://aws-elevate-releases-us-west-2.s3.us-west-2.amazonaws.com/v0.0.3/elevate.yaml&stackName=elevate) |

The stack is created with the name `elevate`. CloudFormation needs the group **ids**, not the names. Find the id of a group:

```bash
aws identitystore get-group-id --identity-store-id <d-xxxxxxxxxx> \
  --alternate-identifier '{"UniqueAttribute":{"AttributePath":"displayName","AttributeValue":"<group name>"}}'
```

When the stack is created, set up the client:

```bash
aws-elevate setup --profile <name>
```

### Install flags

Every flag is optional. What you leave out and the command needs, it asks for.

| Flag | Stack parameter | Default | What it is for |
|---|---|---|---|
| `--profile <name>` | none | the default profile | AWS CLI profile of the delegated administrator account |
| `--region <r>` | none | `sso_region` of the profile, after confirmation | Identity Center home region, where the stack is created |
| `--admin-group <name>` | `AdminGroupId` | asked | Group of the administrators |
| `--requester-group <name>` | `RequesterGroupId` | asked | Group that may use Elevate |
| `--auditor-group <name>` | `AuditorGroupId` | none | Group that can read and export everything |
| `--email <address>` | `AlertEmail` | asked | Address subscribed to critical alerts |
| `--organization-name <name>` | `OrganizationName` | empty | Name shown at the top of the interface (up to 64 characters) |
| `--max-open-grants <n>` | `MaxOpenGrantsPerUser` | 5 | Grants not yet closed that one user may have |
| `--event-consumer-accounts <id,id>` | `EventConsumerAccounts` | none | Account ids allowed to receive the Elevate events |
| `--config-versions-listed <n>` | `ConfigVersionsListed` | 50 | Newest rules versions shown in the history |
| `--yes` | none | off | Answer yes to the questions, for scripts |

Flags take group names; the stack takes their ids and `install` looks them up for you.

To change a setting later: in the CloudFormation console, open the `elevate` stack, choose **Update**, **Use existing template**, and edit the parameter.

## First use

1. Open the local interface:

   ```bash
   aws elevate ui
   ```

   It opens in your browser and closes after 30 minutes without use. (`aws elevate` is an alias that `setup` adds; `aws-elevate ui` does the same.)

2. As an administrator, create the first rule: who may ask (a group), which permission set, on which accounts, how long, and whether it is granted at once or needs an approver.
3. After you publish, the rule shows as "Publishing". Rules take effect in about 30 seconds. Then the roles you may request appear.
4. Request access, with a reason (up to 500 characters). Do not include personal data in it.
5. When the grant is active, check that it works:

   ```bash
   aws sts get-caller-identity --profile jit-<permission set>-<account id>
   ```

   For example `jit-ssm-write-111111111111`. The profile is written to your AWS CLI config. `aws elevate shell` opens a shell with it.

The same things work from the terminal: `aws elevate request`, `status`, `approve`, `revoke`. Run `aws-elevate --help` for the list. Administrators also have `aws elevate admin` (rules, export, import, checks).

### Approvers

A rule with manual approval names one approver group. To have several people or teams approve, create a dedicated group in Identity Center with all of them, and use it as the approver group.

### More users

Each user installs the client and sets it up. No admin step is needed if they are in the requester group:

```bash
curl -fsSL https://raw.githubusercontent.com/sdiaco/aws-elevate/main/install.sh | sh
aws configure sso
aws-elevate setup --profile <name>
```

Here the profile is an SSO profile for the same Identity Center. `setup` finds the Elevate account by itself.

### WSL

If `aws elevate ui` cannot open a browser, point `BROWSER` at the Windows one:

```bash
export BROWSER=/mnt/c/Windows/explorer.exe
```

Add it to your shell profile to keep it. If the browser does not open anyway, the command prints a URL to open yourself.

## Uninstall

Remove Elevate from AWS, for the whole organization (administrators only):

```bash
aws-elevate uninstall infra --profile <name>
```

Add `--check` to only list what exists. The command stops while any grant is open: revoke them first. It asks what to do with the history: keep it, export it to a local folder and then delete it, or delete it. You confirm by typing the stack name. The Identity Center groups and the delegated administrator are never removed.

If you installed the optional service control policy stack, delete it from the management account. The command prints the line:

```bash
aws cloudformation delete-stack --stack-name elevate-guardrails --profile <management admin>
```

Remove the client from this computer, on every computer that has it:

```bash
aws-elevate uninstall client
```

It removes the grant profiles and settings of aws-elevate, the `elevate` alias, the cache and the executable. It does not call AWS. Revoke open grants first: it warns you if grant profiles are still there.

## Security and compliance

Elevate helps implement privileged-access controls required by NIS2, DORA and GDPR; it does not by itself make an organization compliant.

**What is stored and for how long**

- The history of grants (who asked, which permission set, which account, the reason, who approved, when it started and ended, how it closed) is in a DynamoDB table in the Elevate account. Each grant is kept for 13 months after it closes, then deleted automatically. The rules versions stay in the Elevate account too.
- The logs of the Lambda functions are kept 90 days (`api`, `engine`) and 30 days (precheck).
- When the service refuses a call, it logs the line `refused call from <userArn>` with the ARN of the caller and the reason. The ARN identifies a person, so it is personal data in those logs.
- The reason of a request is free text. Tell users not to put personal data in it; the form says so.

**Deleting a person's data.** You do not need to delete the grants of a person on request: the access register is kept to meet a legal obligation and to establish and defend legal claims, as GDPR article 17(3) allows. Keep it only for the period above.

**A copy you cannot change.** Elevate publishes an event for every step of a grant on the `elevate-events` bus. Give the account ids of another account, for example an archive account, in `--event-consumer-accounts`. In that account, create a rule that stores the events. That copy is outside the Elevate account and cannot be changed from it.

**Break-glass.** Elevate itself can be unavailable. An administrator can then use the `AdministratorAccess` permission set of Identity Center. If Identity Center is unavailable, nobody can. Prepare an emergency access before you need it:

- the root user, or an IAM user, of the management account;
- a physical MFA key kept in a safe place;
- an alarm that tells you when it is used;
- a test of the procedure at regular intervals.

**Optional guardrails.** `infra/elevate-guardrails.yaml` is a service control policy, deployed from the management account, that keeps other principals of the Elevate account away from its functions and roles.

## Costs

The services are pay per use. There is no instance and no fixed fee: an installation with no requests costs about nothing, and each request adds a few calls to Lambda, DynamoDB, Step Functions and EventBridge. Check the current prices of those services for your region. `aws-elevate install` prints the idle cost before it asks you to confirm.

## Development

Everything runs in a container; only Docker is needed on the host.

```bash
make dev                          # shell with Go, Python, uv and the AWS CLI
make dev-run CMD="go version"     # run one command in the container
make check                        # lint and tests, as run by the pre-push hook
lefthook install                  # once per clone: commit message and pre-push hooks
```

Rules for contributors are in [AGENTS.md](AGENTS.md). `main` holds released versions; `develop` is the base for work branches and reaches `main` through a pull request.
