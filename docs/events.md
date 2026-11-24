# Events

Elevate publishes an event for every step of a grant, and one when it ignores a rules
version, on the event bus `<stack>-events` (`elevate-events` with the default stack name).
Use them to archive the history outside the Elevate account, to feed a SIEM, or to post to
chat or a ticketing system.

The format is defined by the examples in [`contract/events/`](../contract/events/): one file
per event type, checked by the service tests against what the service actually publishes.

## Event types

Every event has `source = elevate`. The `detail-type` says what happened:

| `detail-type` | When | Published by |
|---|---|---|
| `elevate.request.created` | A request is recorded, before any approval | state machine |
| `elevate.request.approved` | An approver approved a manual request | state machine |
| `elevate.request.rejected` | An approver rejected it | state machine |
| `elevate.request.timed_out` | Nobody decided within 30 minutes | state machine |
| `elevate.request.cancelled` | The requester cancelled it before approval | state machine |
| `elevate.request.invalidated` | The rule changed or disappeared before approval | state machine |
| `elevate.grant.activated` | The access is ready to use | state machine |
| `elevate.grant.closed` | The access is over and cleaned up (`closeReason` says why) | state machine |
| `elevate.grant.error` | The cleanup keeps failing after retries; an operator must act | state machine |
| `elevate.config.rejected` | A published rules version is not valid and was ignored | `api` |

A grant with automatic approval goes `created` → `activated` → `closed`. A manual one adds
`approved`, or ends with one of `rejected`, `timed_out`, `cancelled`, `invalidated`.

## Grant events

All events except `elevate.config.rejected` carry the same snapshot of the grant
([example](../contract/events/elevate.grant.closed.json)):

| Field | Meaning |
|---|---|
| `schemaVersion` | Format version, currently `1` |
| `eventId` | `<grantId>:<step>`, for example `abcde23456:activated`. Stable: use it to drop duplicates |
| `grantId` | Grant id, as shown by the client |
| `requester`, `approver`, `revokedBy` | `{userId, userName}` of the Identity Center user, or `null` |
| `approverGroup` | `{id, name}` of the group that approves the rule, `null` for automatic rules |
| `rule` | Rule id |
| `sourcePs` | `{arn, name}` of the permission set the access is copied from |
| `account` | Target account id |
| `approval` | `auto` or `manual` |
| `privileged` | Whether the rule is marked privileged |
| `startsAt`, `expiresAt` | Start and end of the access (UTC, ISO 8601), `null` before activation |
| `configVersion` | Rules version the grant was created with |
| `reason` | The requester's reason |
| `closeReason` | `expired`, `revoked`, `rejected`, `timed_out`, `cancelled`, `invalidated` or `failed`; `null` while open |
| `revokeReason`, `rejectReason` | Reasons given by whoever revoked or rejected, or `null` |
| `error` | Last error message, or `null` |

`elevate.grant.error` adds `errorCode` (`revoke_failed`). The same event sends an e-mail to
the alerts topic; the grant appears under **Problems** in `aws elevate ui`.

The values come from [`contract/vocabulary.json`](../contract/vocabulary.json). The reasons
are free text typed by users: treat them as untrusted input.

## Rules version events

`elevate.config.rejected` ([example](../contract/events/elevate.config.rejected.json)):

| Field | Meaning |
|---|---|
| `schemaVersion` | `1` |
| `eventId` | `config:<version>:rejected` |
| `configVersion` | The rejected rules version |
| `author` | ARN of the session that published it |
| `errors` | What is wrong, one message per problem |

The previous valid version stays in force.

## Compatibility

- `schemaVersion` changes only when a field is removed or changes meaning.
- New fields can appear in any release: ignore the fields you do not know.
- EventBridge can deliver an event more than once: deduplicate on `eventId`.
- Events are sent with retries, but a grant never waits for them: if EventBridge keeps
  failing, the grant goes on and that event is lost. The grant history in Elevate stays
  complete.

## Receiving the events in another account

1. Install or update Elevate with the consumer accounts:
   `--event-consumer-accounts 333333333333` (stack parameter `EventConsumerAccounts`).
   The bus then lets those accounts create and manage their own rules on it, and no other
   rules.
2. From the consumer account, create a rule on the Elevate bus, by its ARN
   `arn:aws:events:<region>:<elevate-account>:event-bus/elevate-events`, with a pattern
   such as:

   ```json
   {"source": ["elevate"], "detail-type": [{"prefix": "elevate.grant."}]}
   ```

   A new rule takes effect after about a minute.
3. Give the rule a target. For targets in another account, follow
   [Sending and receiving events between AWS accounts](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-cross-account.html).

A consumer account can create rules for every event on the bus: the bus policy limits who
owns a rule, not which events it matches. List only accounts that may see all the events.

For an archive that Elevate's own account cannot change, see
[Security and compliance: A copy you cannot change](security.md#a-copy-you-cannot-change).
