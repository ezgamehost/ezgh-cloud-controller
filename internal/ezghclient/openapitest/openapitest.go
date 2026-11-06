// Package openapitest checks a hand-written API client against the service's own OpenAPI
// document (api/openapi/*.json, fetched from each service's GET /openapi.json by
// scripts/openapi.sh). Each client's test calls every method against a recording server, and
// Check verifies that:
//
//   - every call is an operation the document has, with only query parameters it declares;
//   - Idempotency-Key is sent exactly on operations that declare it (creates), and If-Match and
//     Range only on operations that take them;
//   - a body that isn't JSON (a raw upload) has a content type the operation takes;
//   - every field the client sends is in the request schema, every field it reads is in the
//     response schema, and every field the response always has is in the client's type;
//   - every customer operation is called, or listed as left out on purpose, with a reason.
//
// So a client can't invent an endpoint or a field, and refreshing a document after a service
// changes shows exactly what to update.
package openapitest

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Spec is the part of an OpenAPI 3.1 document the checks need.
type Spec struct {
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas    map[string]*Schema   `json:"schemas"`
		Parameters map[string]Parameter `json:"parameters"`
		Responses  map[string]Response  `json:"responses"`
	} `json:"components"`
}

// Operation is one method on one path.
type Operation struct {
	OperationID string      `json:"operationId"`
	Parameters  []Parameter `json:"parameters"`
	RequestBody *struct {
		Content map[string]struct {
			Schema *Schema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]Response `json:"responses"`
}

// Response is an answer's content, or a reference to one in the components.
type Response struct {
	Ref     string `json:"$ref"`
	Content map[string]struct {
		Schema *Schema `json:"schema"`
	} `json:"content"`
}

// Parameter is a path, query or header parameter, or a reference to one in the components.
type Parameter struct {
	Ref  string `json:"$ref"`
	Name string `json:"name"`
	In   string `json:"in"`
}

// parameter follows a parameter's $ref.
func (s *Spec) parameter(p Parameter) Parameter {
	if p.Name == "" && p.Ref != "" {
		return s.Components.Parameters[strings.TrimPrefix(p.Ref, "#/components/parameters/")]
	}
	return p
}

// Schema is a JSON Schema, as much as the checks need.
type Schema struct {
	Ref        string             `json:"$ref"`
	Type       any                `json:"type"`
	Properties map[string]*Schema `json:"properties"`
	Required   []string           `json:"required"`
	Items      *Schema            `json:"items"`
	AnyOf      []*Schema          `json:"anyOf"`
	OneOf      []*Schema          `json:"oneOf"`
	AllOf      []*Schema          `json:"allOf"`
}

// Load reads a document.
func Load(t *testing.T, path string) *Spec {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run scripts/openapi.sh)", err)
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

var methods = []string{"get", "put", "post", "patch", "delete"}

// operation returns the operation for method on the template, if the document has one.
func (s *Spec) operation(template, method string) *Operation {
	raw, ok := s.Paths[template][strings.ToLower(method)]
	if !ok {
		return nil
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil
	}
	return &op
}

// Match finds the path template and operation for a concrete request.
func (s *Spec) Match(method, path string) (string, *Operation) {
	// Literal paths win over templated ones (/queries/schema over /queries/{id}).
	templates := make([]string, 0, len(s.Paths))
	for t := range s.Paths {
		templates = append(templates, t)
	}
	sort.Slice(templates, func(i, j int) bool { return strings.Count(templates[i], "{") < strings.Count(templates[j], "{") })
	for _, template := range templates {
		re := "^" + regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(regexp.QuoteMeta(template), `[^/]+`) + "$"
		if regexp.MustCompile(re).MatchString(path) {
			if op := s.operation(template, method); op != nil {
				return template, op
			}
		}
	}
	return "", nil
}

// resolve follows $ref, merges allOf, and unwraps `anyOf`/`oneOf` of one type and null.
func (s *Spec) resolve(sc *Schema) *Schema {
	for sc != nil {
		switch {
		case sc.Ref != "":
			sc = s.Components.Schemas[strings.TrimPrefix(sc.Ref, "#/components/schemas/")]
		case len(sc.AllOf) > 0:
			merged := &Schema{Type: "object", Properties: map[string]*Schema{}}
			for _, part := range sc.AllOf {
				part = s.resolve(part)
				if part == nil {
					continue
				}
				for k, v := range part.Properties {
					merged.Properties[k] = v
				}
				merged.Required = append(merged.Required, part.Required...)
			}
			for k, v := range sc.Properties {
				merged.Properties[k] = v
			}
			merged.Required = append(merged.Required, sc.Required...)
			return merged
		case len(sc.AnyOf) > 0 || len(sc.OneOf) > 0:
			if constraintsOnly(sc.AnyOf) && constraintsOnly(sc.OneOf) {
				// Alternatives that only add constraints to the schema they sit in (obs-api's
				// SearchRequest: anyOf [{required: [from, to]}, {required: [cursor]}]): the
				// fields are the schema's own.
				copy := *sc
				copy.AnyOf, copy.OneOf = nil, nil
				sc = &copy
				continue
			}
			var alts []*Schema
			for _, alt := range append(append([]*Schema{}, sc.AnyOf...), sc.OneOf...) {
				if alt.Type != "null" {
					alts = append(alts, alt)
				}
			}
			if len(alts) <= 1 {
				sc = nil
				if len(alts) == 1 {
					sc = alts[0]
				}
				continue
			}
			return s.union(alts)
		default:
			if types, ok := sc.Type.([]any); ok {
				// OpenAPI 3.1's ["string", "null"].
				for _, t := range types {
					if t != "null" {
						copy := *sc
						copy.Type = t
						return &copy
					}
				}
			}
			return sc
		}
	}
	return nil
}

// union is a value that may be any of several objects: a client may read any alternative's fields, and only
// the fields every alternative requires are always there.
func (s *Spec) union(alts []*Schema) *Schema {
	merged := &Schema{Type: "object", Properties: map[string]*Schema{}}
	counts := map[string]int{}
	for _, alt := range alts {
		alt = s.resolve(alt)
		if alt == nil {
			continue
		}
		for k, v := range alt.Properties {
			merged.Properties[k] = v
		}
		for _, r := range alt.Required {
			counts[r]++
		}
	}
	for r, n := range counts {
		if n == len(alts) {
			merged.Required = append(merged.Required, r)
		}
	}
	sort.Strings(merged.Required)
	return merged
}

// constraintsOnly reports whether every alternative only constrains the schema it's in (required
// fields, say) instead of describing a value of its own.
func constraintsOnly(alts []*Schema) bool {
	for _, alt := range alts {
		if alt.Ref != "" || alt.Type != nil || alt.Properties != nil || alt.Items != nil ||
			len(alt.AnyOf) > 0 || len(alt.OneOf) > 0 || len(alt.AllOf) > 0 {
			return false
		}
	}
	return true
}

var rawMessage = reflect.TypeOf(json.RawMessage(nil))

// compareResponse: every field Go reads is in the schema, and every field the schema requires is
// in Go.
func (s *Spec) compareResponse(t *testing.T, where string, typ reflect.Type, sc *Schema) {
	t.Helper()
	sc = s.resolve(sc)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if sc == nil || typ == rawMessage || typ.Kind() == reflect.Interface {
		return
	}
	switch typ.Kind() {
	case reflect.Slice:
		if sc.Items == nil {
			t.Errorf("%s: the client reads a list, the schema doesn't have one", where)
			return
		}
		s.compareResponse(t, where+"[]", typ.Elem(), sc.Items)
	case reflect.Struct:
		seen := map[string]bool{}
		for _, f := range fields(typ) {
			seen[f.name] = true
			prop, ok := sc.Properties[f.name]
			if !ok {
				t.Errorf("%s.%s: the client reads it, the service's schema doesn't have it", where, f.name)
				continue
			}
			s.compareResponse(t, where+"."+f.name, f.typ, prop)
		}
		for _, name := range sc.Required {
			if !seen[name] {
				t.Errorf("%s.%s: the service always returns it, the client's type doesn't have it", where, name)
			}
		}
	}
}

// compareRequest: every field the client sends is in the schema.
func (s *Spec) compareRequest(t *testing.T, where string, typ reflect.Type, sc *Schema) {
	t.Helper()
	sc = s.resolve(sc)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if sc == nil {
		t.Errorf("%s: the client sends a body the operation doesn't take", where)
		return
	}
	if typ.Kind() != reflect.Struct {
		return
	}
	for _, f := range fields(typ) {
		prop, ok := sc.Properties[f.name]
		if !ok {
			t.Errorf("%s.%s: the client sends it, the service doesn't take it", where, f.name)
			continue
		}
		ft := f.typ
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			s.compareRequest(t, where+"."+f.name, ft, prop)
		}
	}
}

type field struct {
	name string
	typ  reflect.Type
}

func fields(typ reflect.Type) []field {
	var out []field
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			out = append(out, fields(f.Type)...)
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" && f.IsExported() {
			out = append(out, field{name, f.Type})
		}
	}
	return out
}

// Call is one client method, with the types it sends and reads.
type Call struct {
	Name string
	Run  func() error
	// Response is what the client decodes the answer into (a value of the type), or nil.
	Response any
	// Request is the body type it sends, or nil.
	Request any
}

// Recorder is a server that answers every request with an empty object (204 to DELETE), and
// records what it was sent.
type Recorder struct {
	*httptest.Server
	mu       sync.Mutex
	last     *http.Request
	lastBody []byte
}

// NewRecorder starts one; it's closed when the test ends.
func NewRecorder(t *testing.T) *Recorder {
	r := &Recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.last = req
		r.lastBody = body
		r.mu.Unlock()
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(r.Close)
	return r
}

// templatedQuery reports whether a query parameter matches a declared templated name (compute's
// tag.{key}): the part before the brace, then anything.
func templatedQuery(declared map[string]bool, name string) bool {
	for d := range declared {
		if before, _, ok := strings.Cut(strings.TrimPrefix(d, "query:"), "{"); ok && strings.HasPrefix(d, "query:") && strings.HasPrefix(name, before) && len(name) > len(before) {
			return true
		}
	}
	return false
}

// Check runs every call and checks it against the document. leftOut maps "METHOD /template" of
// customer operations the client doesn't call to why.
func Check(t *testing.T, spec *Spec, rec *Recorder, calls []Call, leftOut map[string]string) {
	t.Helper()
	covered := map[string]bool{}
	for _, c := range calls {
		rec.mu.Lock()
		rec.last = nil
		rec.mu.Unlock()
		if err := c.Run(); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		rec.mu.Lock()
		req, body := rec.last, rec.lastBody
		rec.mu.Unlock()
		if req == nil {
			t.Fatalf("%s sent nothing", c.Name)
		}
		template, op := spec.Match(req.Method, req.URL.Path)
		if op == nil {
			t.Errorf("%s: %s %s isn't in the service's OpenAPI document", c.Name, req.Method, req.URL.Path)
			continue
		}
		covered[req.Method+" "+template] = true
		declared := map[string]bool{}
		for _, p := range op.Parameters {
			p = spec.parameter(p)
			declared[p.In+":"+strings.ToLower(p.Name)] = true
		}
		for name := range req.URL.Query() {
			if !declared["query:"+strings.ToLower(name)] && !templatedQuery(declared, strings.ToLower(name)) {
				t.Errorf("%s: sends query parameter %q, which %s %s doesn't declare", c.Name, name, req.Method, template)
			}
		}
		if key := req.Header.Get("Idempotency-Key"); (key != "") != declared["header:idempotency-key"] {
			t.Errorf("%s: sends Idempotency-Key: %v; %s %s declares it: %v", c.Name, key != "", req.Method, template, declared["header:idempotency-key"])
		}
		for _, header := range []string{"If-Match", "Range"} {
			if req.Header.Get(header) != "" && !declared["header:"+strings.ToLower(header)] {
				t.Errorf("%s: sends %s, which %s %s doesn't take", c.Name, header, req.Method, template)
			}
		}
		if c.Response != nil {
			// Every success answer's schema: a client decoding several (200 and 201) may read
			// any of their fields.
			var answers []*Schema
			statuses := make([]string, 0, len(op.Responses))
			for status := range op.Responses {
				statuses = append(statuses, status)
			}
			sort.Strings(statuses)
			for _, status := range statuses {
				r := op.Responses[status]
				if r.Ref != "" {
					r = spec.Components.Responses[strings.TrimPrefix(r.Ref, "#/components/responses/")]
				}
				if strings.HasPrefix(status, "2") && r.Content["application/json"].Schema != nil {
					answers = append(answers, r.Content["application/json"].Schema)
				}
			}
			var resp *Schema
			switch len(answers) {
			case 0:
			case 1:
				resp = answers[0]
			default:
				resp = &Schema{OneOf: answers}
			}
			spec.compareResponse(t, c.Name, reflect.TypeOf(c.Response), resp)
		}
		if c.Request != nil {
			var body *Schema
			if op.RequestBody != nil {
				body = op.RequestBody.Content["application/json"].Schema
			}
			spec.compareRequest(t, c.Name+" body", reflect.TypeOf(c.Request), body)
		}
		kind, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if len(body) > 0 && kind != "application/json" && (op.RequestBody == nil || op.RequestBody.Content[kind].Schema == nil) {
			t.Errorf("%s: sends a %q body, which %s %s doesn't take", c.Name, kind, req.Method, template)
		}
	}
	var missing []string
	for template, ops := range spec.Paths {
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				continue
			}
			key := strings.ToUpper(m) + " " + template
			if !covered[key] && leftOut[key] == "" {
				missing = append(missing, key)
			}
			if covered[key] && leftOut[key] != "" {
				t.Errorf("%s is called, but listed as left out", key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("operations the client doesn't call (add them, or say why they're left out): %v", missing)
	}
}
