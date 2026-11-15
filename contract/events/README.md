# Event examples

One file per `detail-type` published on the `elevate-events` bus, `source = elevate`.
Grant events are built by the state machine from the engine snapshot;
`elevate.config.rejected` is emitted by `api` for a rules version it refuses.

`detail.schemaVersion` changes only when a field is removed or changes meaning; new
fields can appear without notice, so consumers should ignore what they do not know.
