# Contract: user dashboard

## Agreed
The dashboard service returns the widgets a user sees.

```endpoint
POST /api/v1/dashboard
summary: build the dashboard for a user
request: {"user_id": "string", "locale?": "string"}
response: {"user_id": "string", "widgets": ["Widget"], "generated": "integer"}
errors: 400 invalid_user
```

```schema
Widget
{"id": "integer", "label": "string", "tags?": ["string"]}
```

## Proposed changes

## Changelog
