package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The machine-readable parts of an agreed contract are fenced blocks (docs/formats.md
// §11). An `endpoint` block describes one HTTP endpoint:
//
//	POST /api/v1/dashboard/{id}
//	status: 201
//	request: {"user_id": "string", "locale?": "string"}
//	response: {"widgets": ["string"]}
//	errors: 400 invalid_user, 404 not_found
//
// and a `schema` block names a shape other blocks may refer to:
//
//	Widget
//	{"id": "integer", "label": "string"}
//
// Shapes are JSON examples whose strings name types: string, integer, number, boolean,
// object, array or null, or the name of a schema block. A key ending in ? is optional.
// Any other string is a string with that example value.

// Endpoint is one parsed endpoint block.
type Endpoint struct {
	Method      string      `json:"method"`
	Path        string      `json:"path"`
	Status      int         `json:"status"`
	Summary     string      `json:"summary,omitempty"`
	Request     any         `json:"request,omitempty"`
	Response    any         `json:"response,omitempty"`
	Errors      []ErrorCase `json:"errors,omitempty"`
	Line        int         `json:"line"`
	hasRequest  bool
	hasResponse bool
}

// ErrorCase is one documented failure: an HTTP status and a short code.
type ErrorCase struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
}

// Schema is one parsed schema block.
type Schema struct {
	Name  string `json:"name"`
	Shape any    `json:"shape"`
	Line  int    `json:"line"`
}

// Spec is everything machine-readable in a contract, with the problems found. A block
// with a problem is left out of the endpoints and schemas, never half used; every other
// block is still read.
type Spec struct {
	Title     string     `json:"title,omitempty"`
	Endpoints []Endpoint `json:"endpoints"`
	Schemas   []Schema   `json:"schemas"`
	Problems  []Problem  `json:"problems"`
}

var (
	httpMethods  = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true}
	endpointKeys = []string{"status", "summary", "request", "response", "errors"}
	keyLineRe    = regexp.MustCompile(`^([A-Za-z_]+):\s*(.*)$`)
	schemaNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	pathParamRe  = regexp.MustCompile(`\{([^{}/]+)\}`)
	scalarTypes  = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "object": true, "array": true, "null": true}
)

// Spec reads the endpoint and schema blocks of the agreed section. A contract with no
// blocks gives an empty Spec with no problems.
func (d Doc) Spec() Spec {
	spec := Spec{Title: d.Title, Endpoints: []Endpoint{}, Schemas: []Schema{}, Problems: []Problem{}}
	seenEndpoint, seenSchema := map[string]int{}, map[string]int{}
	problem := func(line int, format string, args ...any) {
		spec.Problems = append(spec.Problems, Problem{Line: line, Message: fmt.Sprintf(format, args...)})
	}

	// schemas first, so endpoints can tell a schema name from an example string
	for _, b := range d.Blocks {
		if b.Kind != "schema" {
			continue
		}
		s, ok := parseSchema(b, problem)
		if !ok {
			continue
		}
		if first, dup := seenSchema[s.Name]; dup {
			problem(b.Line, "schema %q is defined twice (first at line %d)", s.Name, first)
			continue
		}
		seenSchema[s.Name] = b.Line
		spec.Schemas = append(spec.Schemas, s)
	}
	for _, b := range d.Blocks {
		if b.Kind != "endpoint" {
			continue
		}
		e, ok := parseEndpoint(b, problem)
		if !ok {
			continue
		}
		key := e.Method + " " + e.Path
		if first, dup := seenEndpoint[key]; dup {
			problem(b.Line, "endpoint %s is described twice (first at line %d)", key, first)
			continue
		}
		seenEndpoint[key] = b.Line
		spec.Endpoints = append(spec.Endpoints, e)
	}
	sort.SliceStable(spec.Problems, func(i, j int) bool { return spec.Problems[i].Line < spec.Problems[j].Line })
	return spec
}

// bodyLine is the 1-based file line of line k (0-based) of a block's body.
func bodyLine(b Block, k int) int { return b.Line + 1 + k }

func parseSchema(b Block, problem func(int, string, ...any)) (Schema, bool) {
	lines := strings.Split(b.Body, "\n")
	first := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		problem(b.Line, "the schema block is empty: put the schema's name on its first line, then its shape as JSON")
		return Schema{}, false
	}
	name := strings.TrimSpace(lines[first])
	if !schemaNameRe.MatchString(name) {
		problem(bodyLine(b, first), "%q is not a schema name (letters, digits and underscores, starting with a letter); the first line of a schema block is its name", name)
		return Schema{}, false
	}
	shape, err := parseShape(strings.Join(lines[first+1:], "\n"))
	if err != nil {
		problem(bodyLine(b, first+1), "schema %s: %v", name, err)
		return Schema{}, false
	}
	if shape == nil {
		problem(bodyLine(b, first), "schema %s has no shape: put it as JSON after the name", name)
		return Schema{}, false
	}
	return Schema{Name: name, Shape: shape, Line: b.Line}, true
}

type field struct {
	key  string
	line int // 1-based file line where the field starts
	text string
}

func parseEndpoint(b Block, problem func(int, string, ...any)) (Endpoint, bool) {
	lines := strings.Split(b.Body, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			start = i
			break
		}
	}
	if start < 0 {
		problem(b.Line, "the endpoint block is empty: its first line is the method and path, like POST /api/v1/things")
		return Endpoint{}, false
	}
	ok := true
	e := Endpoint{Status: 200, Line: b.Line}

	head := strings.Fields(lines[start])
	switch {
	case len(head) != 2:
		problem(bodyLine(b, start), "the first line of an endpoint block is the method and path, like POST /api/v1/things; got %q", strings.TrimSpace(lines[start]))
		return Endpoint{}, false
	case !httpMethods[head[0]]:
		problem(bodyLine(b, start), "%q is not an HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD or OPTIONS, in capitals)", head[0])
		ok = false
	case !strings.HasPrefix(head[1], "/") || strings.ContainsAny(head[1], "?# "):
		problem(bodyLine(b, start), "the path %q must start with / and have no query or fragment", head[1])
		ok = false
	}
	e.Method, e.Path = head[0], head[1]

	// fields: `key: value`, a value continuing over following lines until the next key
	var fields []field
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			if len(fields) > 0 {
				fields[len(fields)-1].text += "\n"
			}
			continue
		}
		if m := keyLineRe.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			if !isKey(m[1]) {
				problem(bodyLine(b, i), "%q is not a field of the endpoint (known: %s)", m[1]+":", strings.Join(endpointKeys, ", "))
				ok = false
				fields = append(fields, field{key: "", line: bodyLine(b, i)}) // its continuation lines belong to it, not to the field before
				continue
			}
			fields = append(fields, field{key: m[1], line: bodyLine(b, i), text: m[2]})
			continue
		}
		if len(fields) == 0 {
			problem(bodyLine(b, i), "%q is not a field of the endpoint (known: %s)", strings.TrimSpace(l), strings.Join(endpointKeys, ", "))
			ok = false
			continue
		}
		fields[len(fields)-1].text += "\n" + l
	}

	seen := map[string]bool{}
	for _, f := range fields {
		if f.key == "" {
			continue // an unknown field, already reported
		}
		text := strings.TrimSpace(f.text)
		if seen[f.key] && f.key != "errors" {
			problem(f.line, "%q is given twice", f.key)
			ok = false
			continue
		}
		seen[f.key] = true
		switch f.key {
		case "status":
			n, err := strconv.Atoi(text)
			if err != nil || n < 100 || n > 599 {
				problem(f.line, "status must be an HTTP status code like 201, got %q", text)
				ok = false
			} else {
				e.Status = n
			}
		case "summary":
			e.Summary = text
		case "request", "response":
			shape, err := parseShape(text)
			if err != nil {
				problem(f.line, "%s: %v", f.key, err)
				ok = false
				continue
			}
			if f.key == "request" {
				e.Request, e.hasRequest = shape, true
			} else {
				e.Response, e.hasResponse = shape, true
			}
		case "errors":
			cases, err := parseErrors(text)
			if err != nil {
				problem(f.line, "errors: %v", err)
				ok = false
				continue
			}
			e.Errors = append(e.Errors, cases...)
		}
	}
	return e, ok
}

func isKey(s string) bool {
	for _, k := range endpointKeys {
		if s == k {
			return true
		}
	}
	return false
}

// parseErrors reads "400 invalid_user, 404 not_found": a status and an optional code.
func parseErrors(text string) ([]ErrorCase, error) {
	var out []ErrorCase
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		f := strings.Fields(part)
		if len(f) == 0 {
			continue
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n < 100 || n > 599 || len(f) > 2 {
			return nil, fmt.Errorf("%q is not an error like \"400 invalid_user\" (an HTTP status, then an optional code)", strings.TrimSpace(part))
		}
		c := ErrorCase{Status: n}
		if len(f) == 2 {
			c.Code = f[1]
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no errors listed; remove the line or give statuses like \"400 invalid_user\"")
	}
	return out, nil
}

// parseShape reads a JSON example. Empty text is no shape (nil, nil).
func parseShape(text string) (any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not valid JSON (%v)", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("not valid JSON (extra text after the value)")
	}
	return v, nil
}

// OpenAPI renders the spec as an OpenAPI 3.0 document, as a map ready to marshal to JSON
// or YAML. Shapes become JSON schemas: type names are types, other strings are string
// examples, schema block names are $refs, keys ending in ? are optional.
func (s Spec) OpenAPI() map[string]any {
	names := map[string]bool{}
	for _, sc := range s.Schemas {
		names[sc.Name] = true
	}
	title := s.Title
	if title == "" {
		title = "contract"
	}
	doc := map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": title, "version": "1"},
	}
	paths := map[string]any{}
	for _, e := range s.Endpoints {
		item, _ := paths[e.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[e.Path] = item
		}
		op := map[string]any{"responses": responsesFor(e, names)}
		if e.Summary != "" {
			op["summary"] = e.Summary
		}
		if params := pathParams(e.Path); len(params) > 0 {
			op["parameters"] = params
		}
		if e.hasRequest {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": toSchema(e.Request, names)}}}
		}
		item[strings.ToLower(e.Method)] = op
	}
	doc["paths"] = paths
	if len(s.Schemas) > 0 {
		schemas := map[string]any{}
		for _, sc := range s.Schemas {
			schemas[sc.Name] = toSchema(sc.Shape, names)
		}
		doc["components"] = map[string]any{"schemas": schemas}
	}
	return doc
}

func responsesFor(e Endpoint, names map[string]bool) map[string]any {
	ok := map[string]any{"description": "success"}
	if e.hasResponse {
		ok["content"] = map[string]any{"application/json": map[string]any{"schema": toSchema(e.Response, names)}}
	}
	out := map[string]any{strconv.Itoa(e.Status): ok}
	for _, c := range e.Errors {
		desc := "error"
		if c.Code != "" {
			desc = c.Code
		}
		out[strconv.Itoa(c.Status)] = map[string]any{"description": desc}
	}
	return out
}

func pathParams(path string) []any {
	var out []any
	for _, m := range pathParamRe.FindAllStringSubmatch(path, -1) {
		out = append(out, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
	}
	return out
}

func toSchema(v any, names map[string]bool) map[string]any {
	switch x := v.(type) {
	case nil:
		return map[string]any{"nullable": true}
	case bool:
		return map[string]any{"type": "boolean", "example": x}
	case json.Number:
		if n, err := x.Int64(); err == nil && !strings.ContainsAny(x.String(), ".eE") {
			return map[string]any{"type": "integer", "example": n}
		}
		f, _ := x.Float64()
		return map[string]any{"type": "number", "example": f}
	case string:
		switch {
		case names[x]:
			return map[string]any{"$ref": "#/components/schemas/" + x}
		case scalarTypes[x]:
			if x == "null" {
				return map[string]any{"nullable": true}
			}
			return map[string]any{"type": x}
		}
		return map[string]any{"type": "string", "example": x}
	case []any:
		items := map[string]any{}
		if len(x) > 0 {
			items = toSchema(x[0], names)
		}
		return map[string]any{"type": "array", "items": items}
	case map[string]any:
		props, required := map[string]any{}, []string{}
		for k, val := range x {
			name, optional := strings.CutSuffix(k, "?")
			props[name] = toSchema(val, names)
			if !optional {
				required = append(required, name)
			}
		}
		sort.Strings(required)
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return map[string]any{}
}

// MarshalOpenAPIJSON renders the OpenAPI document as indented JSON with stable key order.
func (s Spec) MarshalOpenAPIJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s.OpenAPI()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalOpenAPIYAML renders the OpenAPI document as YAML with stable key order.
func (s Spec) MarshalOpenAPIYAML() ([]byte, error) {
	return yaml.Marshal(s.OpenAPI())
}
