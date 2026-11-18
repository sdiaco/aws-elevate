# Rules document examples

Shared by the service tests and the client tests. The file name says the expected outcome:

- `valid-*`: accepted by `infra/rules.schema.json` and by the service parser.
- `invalid-*`: refused by both.
- `parse-only-*`: accepted by the schema, refused by the parser. A JSON Schema draft-04
  (the version AWS AppConfig supports) cannot express these constraints, so the service
  checks them on every version it reads.
