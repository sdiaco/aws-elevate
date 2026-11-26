# Installation details

The [README](../README.md) covers the usual path. This page has everything else: every option of the install, how to change it later, updates and the uninstall.

- [The client](#the-client)
- [Install options](#install-options)
- [Change a setting later](#change-a-setting-later)
- [Notifications](#notifications)
- [Updates](#updates)
- [Uninstall](#uninstall)
- [Troubleshooting](#troubleshooting)

---

## The client

```bash
curl -fsSL https://raw.githubusercontent.com/sdiaco/aws-elevate/main/install.sh | sh
```

The script downloads the executable for your system, checks its SHA-256 against the `checksums.txt` of the release and installs it in `~/.local/bin/aws-elevate`. If the checksum does not match, nothing is installed. Add `~/.local/bin` to your `PATH` if the script says so.

| Variable | Effect |
|---|---|
| `VERSION=<x.y.z>` | installs that version instead of the latest |
| `PREFIX=<folder>` | installs in that folder |

`aws-elevate setup` adds the alias `elevate` to the AWS CLI, so `aws elevate <command>` and `aws-elevate <command>` are the same.

---

## Install options

`aws-elevate install` asks for what it needs. Each answer can be given as a flag instead, and `--yes` answers yes to the confirmations, for a script:

```bash
aws-elevate install --profile elevate-admin --region eu-west-1 \
  --admin-group Admins --requester-group Developers \
  --email alerts@example.com --yes
```

Flags take group **names**; the stack takes their ids and `install` looks them up for you.

### Basics

| Flag | Stack parameter | Default | What it is for |
|---|---|---|---|
| `--profile <name>` | | the default profile | AWS CLI profile of the delegated administrator account |
| `--region <region>` | | `sso_region` of the profile, after confirmation | Identity Center home region, where the stack is created |
| `--admin-group <name>` | `AdminGroupId` | asked | Group of the administrators |
| `--requester-group <name>` | `RequesterGroupId` | asked | Group that may use Elevate |
| `--auditor-group <name>` | `AuditorGroupId` | none | Group that can read and export everything |
| `--email <address>` | `AlertEmail` | asked | Address subscribed to critical alerts |
| `--organization-name <name>` | `OrganizationName` | empty | Shown at the top of the interface, up to 64 characters |
| `--yes` | | off | Answer yes to the confirmations |

### Limits and integrations

| Flag | Stack parameter | Default | What it is for |
|---|---|---|---|
| `--max-open-grants <n>` | `MaxOpenGrantsPerUser` | 5 | Grants not yet closed that one user may have |
| `--config-versions-listed <n>` | `ConfigVersionsListed` | 50 | Newest rules versions shown in the history |
| `--event-consumer-accounts <id,id>` | `EventConsumerAccounts` | none | Accounts allowed to receive the Elevate events: see [Events](events.md) |

### Audit and data protection

See [Security and compliance](security.md) for what each one protects.

| Flag | Stack parameter | Default | What it is for |
|---|---|---|---|
| `--retention-months <n>` | `HistoryRetentionMonths` | 13 | Months a grant is kept after it closes, 1 to 120 |
| `--pitr` | `PointInTimeRecovery` | `Disabled` | Continuous backups of the grant history, for 35 days |
| `--kms-key-arn <arn>` | `KmsKeyArn` | keys owned by AWS | Your KMS key for the history and the logs ([key policy](security.md#customer-kms-key)) |

In the Launch Stack page the same parameters are grouped the same way: **Who uses Elevate**, **Alerts and interface**, **Limits**, **Integrations**, **Audit and data protection**. Leave **For testing only** as it is.

---

## Change a setting later

In the CloudFormation console of the delegated administrator account: open the `elevate` stack → **Update** → **Use existing template** → change the parameter → confirm. The next release keeps the values you set.

---

## Notifications

Elevate sends two kinds of e-mail through two SNS topics in the delegated administrator account. The `<stack>-notify` function composes and publishes them; its role may only publish to these two topics and write its own logs.

The subject of each e-mail:

- approvals: `aws-elevate: approval needed for <rule> on <account>`
- alerts: `aws-elevate: cleanup of grant <id> is failing`

| Topic | What it sends | Who receives it after the install |
|---|---|---|
| `<stack>-alerts` | A grant whose cleanup keeps failing after retries: the access may still work | The `--email` address, once its owner confirms the e-mail from AWS |
| `<stack>-approvals` | Every request that needs manual approval, with the rule, the account, the reason, the approver group and the commands to answer | Nobody until you subscribe someone |

E-mails reach whoever is subscribed to the topic, not the members of the Identity Center group: SNS does not know the groups. One approvals topic receives the requests of every rule, so everyone subscribed sees every request, including who asked and why. Only members of the approver group can approve, whoever reads the e-mail. Without subscribers, requests waiting for approval show in `aws elevate status` and in the interface.

To subscribe an address, with an administrator profile of the delegated administrator account (Elevate's own roles cannot) and the default stack name `elevate`:

```bash
aws sns subscribe --profile elevate-admin --region <region> \
  --topic-arn arn:aws:sns:<region>:<account>:elevate-approvals \
  --protocol email --notification-endpoint approvers@example.com
```

Or in the SNS console of that account and region: **Topics** → `elevate-approvals` → **Create subscription** → protocol **Email**. Each address must confirm the e-mail AWS sends before anything reaches it. A mailing list of the approvers is easier to keep up to date than one subscription per person.

`aws elevate admin doctor` checks that the alerts topic has a confirmed subscriber and shows how many the approvals topic has.

The e-mails are plain text. SNS adds the unsubscribe footer. For per-group routing, chat or ticketing, use the [events](events.md) instead: a rule can match `elevate.request.created` on `approverGroup.name` and send it anywhere EventBridge can.

Costs: the first 1,000 e-mails a month are free, then about $2 per 100,000 (see [SNS pricing](https://aws.amazon.com/sns/pricing/)).

---

## Updates

The **Updates** page of `aws elevate ui` shows when a new version is out. Every user updates the client from there; administrators also update the stack, after a preview of the changes.

From the terminal, to update the client:

```bash
aws-elevate update
```

---

## Uninstall

```bash
aws-elevate uninstall           # asks: infra or client
aws-elevate uninstall infra     # Elevate from AWS
aws-elevate uninstall client    # aws-elevate from this computer
```

Add `--check` to only list what would be removed.

### infra

For administrators: it removes Elevate for the whole organization.

1. It stops while any access is open: revoke them first.
2. It asks what to do with the history: keep it, export it to a local folder and then delete it, or delete it.
3. You confirm by typing the stack name.

It removes the stack, the grant history (unless you keep it), the rules versions, and the grant profiles and settings on this computer. The Identity Center groups and the delegated administrator stay.

If you installed the [guardrails](security.md#guardrails), delete their stack from the management account:

```bash
aws cloudformation delete-stack --stack-name elevate-guardrails --profile <management admin>
```

### client

For every user, on their own computer. It removes the grant profiles and settings of aws-elevate, the `elevate` alias, the cache and the executable. It does not call AWS. Revoke your open access first: it warns you if grant profiles are still there.

---

## Troubleshooting

| What you see | What to do |
|---|---|
| `install` says the account is not the delegated administrator | Use a profile of the delegated administrator account, or register it from the management account with the command it prints |
| `install` asks for the home region | Use the region where Identity Center is enabled |
| `aws elevate ui` prints *Could not open a browser* | Open the URL it prints. To fix it, set `BROWSER` to your browser |
| No **New request** after the first rule | Rules take about 30 seconds to apply; the interface updates by itself |
| `ElevateRequester`, `ElevateAdmin` or `ElevateAuditor` in the AWS access portal | Expected: they are the roles the client uses to call Elevate. Opening the console with them shows almost nothing |
| No e-mail for requests waiting for approval | Nobody is subscribed to the approvals topic: see [Notifications](#notifications) |
| `uninstall infra` says you need the administrator role | Use an administrator profile of the delegated administrator account |
