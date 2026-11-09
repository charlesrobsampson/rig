# Contract tests

A multi-service ticket's `contract.md` says what its services have agreed on. When that
agreement is more than prose, you can make it checkable: write the endpoints and schemas
as fenced blocks, and each service's test suite can check itself against them, so drift
fails a build instead of surprising the service on the other side.

This is optional. A contract without blocks works exactly as before.

## Writing the blocks

In the `## Agreed` section, before the plan is approved (afterwards through proposals;
see docs/formats.md §11):

````markdown
```endpoint
POST /api/v1/dashboard/{id}
summary: build the dashboard for a user
status: 201
request: {"user_id": "string", "locale?": "string"}
response: {"widgets": ["Widget"], "generated": "integer"}
errors: 400 invalid_user, 404 not_found
```

```schema
Widget
{"id": "integer", "label": "string", "tags?": ["string"]}
```
````

**Endpoint blocks.** The first line is the method (capitals) and path (`{id}` marks a path
parameter). Then any of these fields, each starting at the beginning of a line:

| Field | Meaning |
|---|---|
| `summary:` | one line of description |
| `status:` | the success status (default `200`) |
| `request:` | the JSON body the endpoint takes |
| `response:` | the JSON body it returns |
| `errors:` | documented failures, `STATUS code` separated by commas (the line may repeat) |

A `request:` or `response:` value may continue over several lines.

**Schema blocks.** The first line is the schema's name; the rest is its shape as JSON.
Any shape may refer to a schema by its name.

**Shapes** are JSON examples whose strings name types: `string`, `integer`, `number`,
`boolean`, `object`, `array`, `null`, or the name of a schema. A key ending in `?` is
optional; every other key is required. A string that is none of those is an example value
of a string. `[X]` is an array of `X`. Extra fields are not forbidden by the contract
itself: it says what must be there.

Only the agreed section counts. Blocks inside a proposal are not part of the contract until
a human accepts it.

## Checking and exporting

```sh
rig contract check <ticket-id>                  # or: --file path/to/contract.md
rig contract export <ticket-id> --format json   # the parsed endpoints and schemas
rig contract export <ticket-id> --format openapi-json -o contract.openapi.json
rig contract export --file ../contract.md --format openapi   # OpenAPI 3.0 as YAML (the default)
```

`--file` reads any contract.md without a rig daemon, so a service's own CI can run these
with just the `rig` binary and a copy of the file. A malformed block is reported with its
line number (`contract.md:23: "FETCH" is not an HTTP method ...`), nothing is written, and
the exit code is 2: a build never runs against a half-read contract. One bad block never
hides the others: every problem is listed. A contract with no blocks exports an empty
document and a note.

`openapi` and `openapi-json` give an OpenAPI 3.0 document you can feed to any validator or
fuzzer (path parameters, request and response schemas, the documented errors, shared
schemas as `$ref`s). `json` is the same information in rig's own simpler form.

## A service test

`examples/contract-test/` is a complete, runnable example: a contract, the `contract.json`
exported from it, a toy handler, and a test (`dashboard/contract_test.go`) that reads the
documented response shape and checks what the handler actually returns, reporting every
missing field and wrong type. Rename a field in the handler and the test fails:

```
--- FAIL: TestDashboardMatchesTheContract
    the response diverges from the contract:
      response.generated: missing
```

The pattern for your own service:

1. Keep a copy of the contract's export in the service's repository (`contract.json`, or
   fetch it in CI with `rig contract export --file`).
2. In a test, call your handler the way the contract says and compare the response to the
   `response` shape; call it badly and compare the failure to the documented `errors`.
3. When the contract changes (a human accepted a proposal), regenerate the export; the test
   then tells you what your service must change.
