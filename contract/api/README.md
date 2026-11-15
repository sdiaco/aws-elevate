# API examples

One file per route of the `api` Lambda (prefix `/v1`, SigV4 signed, header
`X-Elevate-Client-Version`). Each file holds `method`, `path`, `query`, `request`, the
expected `status` and an example `response`.

Both test suites read them: the service checks that every route answers with the same
keys and JSON types, and the client decodes every response with unknown fields refused.
A `null` value marks a field that can be empty; the other examples show it filled.
`error.json` is the format of every error; the codes are in `../vocabulary.json`.
