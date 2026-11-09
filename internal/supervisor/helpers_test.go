package supervisor_test

import "encoding/json"

func jsonUnmarshal(raw json.RawMessage, v any) error { return json.Unmarshal(raw, v) }
