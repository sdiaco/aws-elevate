# API examples

One file per route of the `api` Lambda (prefix `/v1`, SigV4 signed, header
`X-Elevate-Client-Version`). Each file holds `method`, `path`, `query`, `request`, the
expected `status` and an example `response`.

Both test suites read them: the service checks that every route answers with the same
keys and JSON types, and the client decodes every response with unknown fields refused.
A `null` value marks a field that can be empty; the other examples show it filled.
`error.json` is the format of every error; the codes are in `../vocabulary.json`.

`GET /v1/grants` filters: `scope` (`mine`, `approvable`, `all`), `user`, `from`, `to`,
`cursor`, and at most one of `open=true` (grants not yet closed) or `closed=true` (closed
grants only, the History view).

`GET /v1/config/versions` lists the newest versions, newest first, as many as the stack
parameter `ConfigVersionsListed` says (default 50); an older one is
still readable at `GET /v1/config/versions/{number}`.

`GET /v1/rules?scope=requestable` also lists in `openGrants` the rule and account pairs
where the caller already has an open grant on the same permission set: a new request
there would be refused as `duplicate`. Without the scope the list is empty.
