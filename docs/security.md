# Security and compliance

Elevate helps implement privileged-access controls required by **NIS2**, **DORA** and **GDPR**. It does not by itself make an organization compliant: governance, risk analysis, incident handling and contracts stay with you.

- [How it helps](#how-it-helps)
- [How Elevate is built](#how-elevate-is-built)
- [What is stored, and for how long](#what-is-stored-and-for-how-long)
- [What protects the history](#what-protects-the-history)
- [Point-in-time recovery](#point-in-time-recovery)
- [Customer KMS key](#customer-kms-key)
- [Guardrails](#guardrails)
- [A copy you cannot change](#a-copy-you-cannot-change)
- [Personal data](#personal-data)
- [Who keeps standing access](#who-keeps-standing-access)
- [Break-glass](#break-glass)

---

## How it helps

| Requirement | What Elevate gives you |
|---|---|
| **NIS2** art. 21(2)(i): access control | No standing administrator access: it is asked for, limited in time and removed by itself |
| **DORA** art. 9(4)(c): least privilege, controlled privileged access | A reason for every access, approval by someone else, a history to review |
| **DORA** art. 12: logging | Who asked, what, when, why, and who approved |
| **GDPR** art. 32: access to personal data only when needed | Access to accounts with personal data only for the time asked, with a reason |
| **GDPR** art. 5(1)(c, e): minimization, limited retention | Little data, deleted by itself after the retention you choose |

---

## How Elevate is built

- **The part that reads requests cannot grant access.** The `api` function handles every call from users and has no permission to change Identity Center.
- **The part that grants access never reads requests.** The `engine` function changes Identity Center and receives only an action and a grant id from the workflow.
- **Access is denied at the expiry.** Each grant has its own temporary permission set with a statement that denies everything after the end time, so access stops even before the assignment is removed.
- **It runs in your account.** The service, the rules and the history are all in the delegated administrator account. Nothing is sent outside your AWS organization.

---

## What is stored, and for how long

| What | Where | How long |
|---|---|---|
| Grant history: who asked, permission set, account, reason, who approved, start, end, how it closed | DynamoDB table in the Elevate account | 13 months after the grant closes by default, 1 to 120 (`--retention-months`) |
| Rules versions | AppConfig in the Elevate account | until you uninstall |
| Logs of `api` and `engine` | CloudWatch Logs | 90 days |
| Logs of the precheck | CloudWatch Logs | 30 days |
| Workflow history (includes user name and reason) | Step Functions | 90 days |

A change of the retention applies to grants closed after it; grants already closed keep the retention they had.

---

## What protects the history

- **The table's resource policy** lets only the Elevate service roles read or write the data. It denies backups, point-in-time restores and exports to everyone else, administrators of the account included.
- **The [guardrails](#guardrails)**, if you install them, also keep the table, its resource policy, its time-to-live, its point-in-time recovery and its backups to the people who deploy Elevate. They are the only block on AWS Backup jobs that use the advanced DynamoDB backup features, which a resource policy cannot stop.

A backup plan of the organization that includes the table will therefore fail on it, by design: exclude the table from such plans.

---

## Point-in-time recovery

Optional, off by default: `--pitr` (`PointInTimeRecovery=Enabled`). It keeps continuous backups of the history for 35 days, so the table can be restored to any second in that window. Turn it on where the history must be recoverable, for example under DORA. It costs a few cents for this table.

To restore (an administrator of the Elevate account):

1. Widen the table's resource policy so your role may restore and copy data. This is a deliberate change, recorded in CloudTrail.
2. Restore the table to a new table with `RestoreTableToPointInTime`.
3. Copy the items you need back into the Elevate table, then delete the new table. Elevate always uses its own table.
4. Put the resource policy back exactly as the template defines it.

---

## Customer KMS key

By default the history and the logs use keys owned by AWS. To use your own: `--kms-key-arn` (`KmsKeyArn`), with the ARN of a key in the same region as the stack.

The key policy must keep the statement that enables IAM policies in the account, and must allow CloudWatch Logs of that region. Example for account `111111111111`, region `eu-west-1`, stack `elevate`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "EnableIAMPolicies",
      "Effect": "Allow",
      "Principal": {"AWS": "arn:aws:iam::111111111111:root"},
      "Action": "kms:*",
      "Resource": "*"
    },
    {
      "Sid": "AllowCloudWatchLogs",
      "Effect": "Allow",
      "Principal": {"Service": "logs.eu-west-1.amazonaws.com"},
      "Action": [
        "kms:Encrypt*",
        "kms:Decrypt*",
        "kms:ReEncrypt*",
        "kms:GenerateDataKey*",
        "kms:Describe*"
      ],
      "Resource": "*",
      "Condition": {
        "ArnLike": {
          "kms:EncryptionContext:aws:logs:arn": "arn:aws:logs:eu-west-1:111111111111:log-group:/aws/lambda/elevate-*"
        }
      }
    }
  ]
}
```

The Elevate service roles get only `kms:Decrypt` on the key, through DynamoDB, from the template itself.

**What the key covers:** the grant history and the three log groups (`/aws/lambda/<stack>-precheck`, `-engine`, `-api`). **What it does not cover:** the rules (AppConfig), the workflow history, the events and the alert e-mails; these stay on keys owned by AWS.

---

## Guardrails

`elevate-guardrails.yaml` is an optional service control policy. It is deployed from the **management account** and attached to the Elevate account. It keeps everyone except the deployers away from:

- the Elevate functions, roles and workflow;
- the task tokens and executions of the workflow;
- the grant history table, its resource policy, its backups and its point-in-time recovery.

Deploy it from the management account with the template of the release, in the same region as the stack:

```
https://aws-elevate-releases-<region>.s3.<region>.amazonaws.com/v<version>/elevate-guardrails.yaml
```

| Parameter | Value |
|---|---|
| `ServiceAccountId` | the delegated administrator account |
| `StackName` | the name of the Elevate stack, `elevate` by default |
| `DeployerRoleArns` | the roles that install and update Elevate, for example `arn:aws:iam::<account>:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_AdministratorAccess_*` |

If a deployer ARN is wrong, the management account is never affected by service control policies: fix or detach the policy from there.

---

## A copy you cannot change

Elevate publishes an event for every step of a grant on the `elevate-events` bus. Give another account, for example an archive account, in `--event-consumer-accounts`, and create a rule there that stores the events. That copy is outside the Elevate account and cannot be changed from it.

---

## Personal data

- **The reason is free text.** The form tells users not to put personal data in it.
- **Refused calls are logged** with the ARN of the caller, which identifies a person. They are kept with the logs, 90 days at most.
- **Deletion requests.** You do not need to delete the grants of a person on request: the access register is kept to meet a legal obligation and to establish and defend legal claims, as GDPR article 17(3) allows. It is deleted by itself at the end of the retention.

---

## Who keeps standing access

Elevate removes standing access from everyday work, but not from the account it runs in. Installing, updating and uninstalling the stack create and delete IAM roles, Lambda functions and permission sets: that needs an administrator role in the Identity Center delegated administrator account, and Elevate cannot grant it to itself.

| Access | Who | Used for |
|---|---|---|
| Administrator role in the delegated administrator account | 1–2 people who operate the stack | `aws-elevate install`, stack updates, `aws-elevate uninstall infra`, the first level of break-glass |
| `ElevateAdmin` (created by the stack) | Elevate administrators | Rules, revocations, updates page: Elevate only, no access to the account |
| Grants | Everyone else | Everything else, for the time asked |

Keep the first row small and rare:

- give it through a dedicated group (for example `elevate-operators`) assigned to that account only;
- or, stricter, through a separate user kept for stack operations and emergencies, so that the everyday user has no standing access;
- alert on its use, for example with an EventBridge rule on its sign-in in CloudTrail.

---

## Break-glass

If Elevate is unavailable, an administrator can use the `AdministratorAccess` permission set of Identity Center. If Identity Center is unavailable, nobody can. Prepare an emergency access before you need it:

- the root user, or an IAM user, of the management account;
- a physical MFA key kept in a safe place;
- an alarm that tells you when it is used;
- a test of the procedure at regular intervals.
