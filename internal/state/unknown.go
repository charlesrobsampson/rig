package state

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Fields this build does not know are kept and written back, so rewriting a file
// from a newer rig never drops data. Readers ignore them otherwise (formats.md §5).

func unknownFields(data []byte, known any) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if json.Unmarshal(data, &all) != nil {
		return nil
	}
	for key := range jsonKeys(reflect.TypeOf(known)) {
		delete(all, key)
	}
	if len(all) == 0 {
		return nil
	}
	return all
}

func marshalWithUnknown(known any, unknown map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}
	if len(unknown) == 0 {
		return raw, nil
	}
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &merged); err != nil {
		return nil, err
	}
	for key, value := range unknown {
		if _, taken := merged[key]; !taken {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		keys[name] = true
	}
	return keys
}
