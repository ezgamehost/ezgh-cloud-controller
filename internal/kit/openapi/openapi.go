// Package openapi builds a product's OpenAPI 3.1 document, which the service serves at
// GET /openapi.json (contracts/api-conventions.md, "OpenAPI").
//
// The document is generated from two sources that can't drift from the routes:
//
//   - the manifest, which says what every product's API has in common: the customer endpoints'
//     paths and methods, their path parameters, the credentials they take, their operation IDs
//     (the Trails eventName), Idempotency-Key, and the platform's error responses;
//   - the product's overlay (openapi.overlay.json at the repository root), which says what only
//     the product knows: info, and for each customer endpoint its query parameters, request body
//     and success responses, with the schemas they use.
//
// The service checks its routes against the manifest (the Trails catalog), and Build checks the
// overlay against the manifest: an overlay operation that isn't a customer endpoint, and a
// customer endpoint without one, are both errors, so the service refuses to start (and its tests
// fail). /health, /ready, /openapi.json, /internal/* and edge-host endpoints are never in the document;
// live-host endpoints are, with a servers entry naming the live host.
package openapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// Path is where the document is served.
const Path = "/openapi.json"

// Customer reports whether an endpoint is part of the customer API, and so of the document:
// everything a customer calls (auth caller or operation) on the API host, never /health, /ready,
// /openapi.json or /internal/*, never an edge-host endpoint (on https://<alias>.<edge domain>,
// outside the document's server), never a registry-host endpoint (the OCI distribution spec
// describes those), never a data-host endpoint (a product data plane, auth productKey; OFREP
// describes Feature Flags'), and never a PKI-host endpoint (RFC 5280, RFC 6960 and RFC 8555
// describe Private CA's).
func Customer(e manifest.Endpoint) bool {
	return (e.Auth == "caller" || e.Auth == "operation") && !e.OnEdgeHost() && !e.OnRegistryHost() && !e.OnGitHost() && !e.OnDataHost() && !e.OnPKIHost()
}

var (
	manifestParam = regexp.MustCompile(`:([A-Za-z0-9]+)`)
	openAPIParam  = regexp.MustCompile(`\{([A-Za-z0-9]+)\}`)
	methods       = []string{"get", "put", "post", "delete", "patch"}
	// What an overlay operation may say. Security, servers and the platform's errors are the
	// kit's; everything else a product doesn't need.
	operationKeys = []string{"summary", "description", "operationId", "tags", "parameters", "requestBody", "responses", "deprecated", "externalDocs"}
	overlayKeys   = []string{"info", "paths", "components", "tags", "externalDocs"}
)

// ToOpenAPIPath turns a manifest path (/v1/organizations/:orgId) into an OpenAPI one
// (/v1/organizations/{orgId}).
func ToOpenAPIPath(path string) string { return manifestParam.ReplaceAllString(path, "{$1}") }

// ToManifestPath is the inverse of ToOpenAPIPath.
func ToManifestPath(path string) string { return openAPIParam.ReplaceAllString(path, ":$1") }

// Build makes the document from the manifest and the overlay. The error lists every problem.
// orgId and projectId take a UUID or a prefixed ID; every other …Id parameter a UUID.
func Build(m *manifest.Manifest, overlay []byte) ([]byte, error) { return BuildWith(m, overlay, nil) }

// BuildWith is Build where the …Id path parameters in prefixes (parameter → type prefix: the
// service's PathIDs) also take a prefixed ID.
func BuildWith(m *manifest.Manifest, overlay []byte, prefixes map[string]string) ([]byte, error) {
	doc, problems := build(m, overlay, prefixes)
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("openapi.overlay.json and the manifest's customer endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return json.MarshalIndent(doc, "", "  ")
}

// Operations lists a document's operations as "METHOD /manifest/:path", sorted: what a test
// compares with the routes.
func Operations(doc []byte) ([]string, error) {
	var d struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	var out []string
	for path, item := range d.Paths {
		for method := range item {
			if slices.Contains(methods, method) {
				out = append(out, strings.ToUpper(method)+" "+ToManifestPath(path))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func build(m *manifest.Manifest, raw []byte, prefixes map[string]string) (map[string]any, []string) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	var overlay map[string]any
	if err := json.Unmarshal(raw, &overlay); err != nil || overlay == nil {
		return nil, []string{"the overlay isn't a JSON object"}
	}
	for key := range overlay {
		if !slices.Contains(overlayKeys, key) && !strings.HasPrefix(key, "x-") {
			add("%s: an overlay has only %s", key, strings.Join(overlayKeys, ", "))
		}
	}
	info, _ := overlay["info"].(map[string]any)
	if title, _ := info["title"].(string); title == "" {
		add("info.title: required")
	}
	if version, _ := info["version"].(string); version == "" {
		add("info.version: required")
	}
	components, _ := overlay["components"].(map[string]any)
	if _, ok := overlay["components"]; ok && components == nil {
		add("components: must be an object")
	}
	if components == nil {
		components = map[string]any{}
	}
	kit := platformComponents()
	for model, extra := range map[string]func() map[string]map[string]any{
		manifest.ErrorModelPrometheus: prometheusComponents,
		manifest.ErrorModelOpenAI:     openAIComponents,
	} {
		if !slices.ContainsFunc(m.Endpoints, func(e manifest.Endpoint) bool { return Customer(e) && e.ErrorModel == model }) {
			continue
		}
		for kind, components := range extra() {
			for name, value := range components {
				kit[kind][name] = value
			}
		}
	}
	for kind, platform := range kit {
		mine, _ := components[kind].(map[string]any)
		if mine == nil {
			mine = map[string]any{}
		}
		for name, value := range platform {
			if _, taken := mine[name]; taken {
				add("components.%s.%s: the kit defines it", kind, name)
			}
			mine[name] = value
		}
		components[kind] = mine
	}

	// The overlay's operations, by "METHOD /manifest/:path".
	given := map[string]map[string]any{}
	paths, _ := overlay["paths"].(map[string]any)
	if paths == nil {
		add("paths: required, an object")
	}
	for path, item := range paths {
		ops, ok := item.(map[string]any)
		if !ok {
			add("paths.%s: must be an object", path)
			continue
		}
		for method, op := range ops {
			if !slices.Contains(methods, method) {
				add("paths.%s.%s: a path has only operations (%s)", path, method, strings.Join(methods, ", "))
				continue
			}
			o, ok := op.(map[string]any)
			if !ok {
				add("paths.%s.%s: must be an object", path, method)
				continue
			}
			given[strings.ToUpper(method)+" "+ToManifestPath(path)] = o
		}
	}

	actions := map[string]string{}
	for _, a := range m.Actions {
		actions[a.ID] = a.Description
	}
	docPaths := map[string]any{}
	// Paths with operations on the live host, and those with operations on the API host.
	livePaths, apiPaths := map[string][]map[string]any{}, map[string]bool{}
	operationIDs := map[string]string{}
	for _, e := range m.Endpoints {
		key := e.Method + " " + e.Path
		o, ok := given[key]
		if !Customer(e) {
			if ok {
				add("%s: isn't a customer endpoint, so it isn't in the document", key)
				delete(given, key)
			}
			continue
		}
		if !ok {
			add("%s: a customer endpoint with no operation in the overlay", key)
			continue
		}
		delete(given, key)
		alias, ownID := false, ""
		if e.Trail != nil && !e.Trail.RPC() && e.Action != "" {
			for _, other := range m.Endpoints {
				if other.Action != e.Action || other.Trail == nil || other.Trail.RPC() || other.Trail.EventName != e.Trail.EventName {
					continue
				}
				if e.Method == "GET" && other.Path != e.Path && other.Method == "GET" {
					alias = true
					break
				}
				// A computation POST sharing its GET's event: its operation is <eventName>Post
				// unless the overlay names it.
				if e.Method == "POST" && manifest.ReadPair(e, other) {
					alias, ownID = true, e.Trail.EventName+"Post"
					break
				}
			}
		}
		op, opProblems := operation(e, o, actions[e.Action], alias, ownID, prefixes)
		for _, p := range opProblems {
			add("%s: %s", key, p)
		}
		if id, _ := op["operationId"].(string); id != "" {
			if other, dup := operationIDs[id]; dup {
				add("%s: operationId %s is also %s's", key, id, other)
			}
			operationIDs[id] = key
		}
		path := ToOpenAPIPath(e.Path)
		item, _ := docPaths[path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			docPaths[path] = item
		}
		item[strings.ToLower(e.Method)] = op
		if e.OnLiveHost() {
			livePaths[path] = append(livePaths[path], op)
		} else {
			apiPaths[path] = true
		}
	}
	// A live-host endpoint is served on https://<live hostname>: a path-level servers entry names
	// it (contracts/api-conventions.md, "Live hosts"), or, on a path that also has API-host
	// operations, each live operation's own.
	if m.Live != nil {
		for path, ops := range livePaths {
			servers := []any{map[string]any{"url": "https://" + m.Live.Hostname}}
			if !apiPaths[path] {
				docPaths[path].(map[string]any)["servers"] = servers
				continue
			}
			for _, op := range ops {
				op["servers"] = servers
			}
		}
	}
	for key := range given {
		add("%s: in the overlay, but not a route in the manifest", key)
	}

	doc := map[string]any{
		"openapi":    "3.1.0",
		"info":       info,
		"servers":    []any{map[string]any{"url": "https://" + m.API.Hostname}},
		"paths":      docPaths,
		"components": components,
	}
	for _, key := range []string{"tags", "externalDocs"} {
		if v, ok := overlay[key]; ok {
			doc[key] = v
		}
	}
	for key, v := range overlay {
		if strings.HasPrefix(key, "x-") {
			doc[key] = v
		}
	}
	return doc, problems
}

// operation is one endpoint's operation: the overlay's, with the manifest's parts added.
func operation(e manifest.Endpoint, o map[string]any, actionDescription string, readAlias bool, ownID string, prefixes map[string]string) (map[string]any, []string) {
	var problems []string
	op := map[string]any{}
	for key, v := range o {
		if !slices.Contains(operationKeys, key) && !strings.HasPrefix(key, "x-") {
			problems = append(problems, fmt.Sprintf("%s: the kit writes it, or it isn't used", key))
			continue
		}
		op[key] = v
	}

	// The operation ID is the Trails eventName. Read aliases may name their format, and a
	// computation POST sharing its GET's event is ownID unless it names its own; endpoints that
	// are not recorded, or record per JSON-RPC method, name their own operation.
	if e.Trail != nil && !e.Trail.RPC() {
		id, supplied := o["operationId"].(string)
		if readAlias && supplied && id != "" {
			op["operationId"] = id
		} else if ownID != "" {
			op["operationId"] = ownID
		} else {
			if supplied && id != e.Trail.EventName {
				problems = append(problems, fmt.Sprintf("operationId is the Trails eventName, %s", e.Trail.EventName))
			}
			op["operationId"] = e.Trail.EventName
		}
	} else if id, _ := o["operationId"].(string); id == "" {
		problems = append(problems, "operationId: required when the endpoint has no Trails eventName")
	}
	if _, ok := op["summary"]; !ok && actionDescription != "" {
		op["summary"] = actionDescription
	}

	// Path parameters from the manifest, then Idempotency-Key, then the overlay's (which may
	// describe a path parameter further, but never add one).
	var params []any
	var pathParams []string
	index := map[string]int{}
	for _, m := range manifestParam.FindAllStringSubmatch(e.Path, -1) {
		name := m[1]
		pathParams = append(pathParams, name)
		schema := map[string]any{"type": "string"}
		if strings.HasSuffix(name, "Id") {
			schema = map[string]any{"type": "string", "format": "uuid"}
			prefix := map[string]string{"orgId": "org", "projectId": "prj"}[name]
			if prefix == "" {
				prefix = prefixes[name]
			}
			if prefix != "" {
				schema = map[string]any{"type": "string", "description": "UUID or prefixed ID (" + prefix + "_…)"}
			}
		}
		index["path "+name] = len(params)
		params = append(params, map[string]any{"name": name, "in": "path", "required": true, "schema": schema})
	}
	if e.IdempotencyKey {
		index["header Idempotency-Key"] = len(params)
		params = append(params, map[string]any{"$ref": "#/components/parameters/IdempotencyKey"})
	}
	if list, ok := o["parameters"]; ok {
		items, ok := list.([]any)
		if !ok {
			problems = append(problems, "parameters: must be an array")
		}
		for i, item := range items {
			p, _ := item.(map[string]any)
			name, _ := p["name"].(string)
			in, _ := p["in"].(string)
			if _, ref := p["$ref"]; ref {
				params = append(params, p)
				continue
			}
			if name == "" || !slices.Contains([]string{"path", "query", "header", "cookie"}, in) {
				problems = append(problems, fmt.Sprintf("parameters[%d]: needs a name and in (path, query, header or cookie)", i))
				continue
			}
			if in == "path" && !slices.Contains(pathParams, name) {
				problems = append(problems, fmt.Sprintf("parameters[%d]: %s isn't a parameter of the path", i, name))
				continue
			}
			if at, ok := index[in+" "+name]; ok {
				merged := map[string]any{}
				if base, ok := params[at].(map[string]any); ok {
					for k, v := range base {
						merged[k] = v
					}
				}
				for k, v := range p {
					merged[k] = v
				}
				params[at] = merged
				continue
			}
			index[in+" "+name] = len(params)
			params = append(params, p)
		}
	}
	if len(params) > 0 {
		op["parameters"] = params
	} else {
		delete(op, "parameters")
	}

	// The overlay's responses, which must say what success looks like, then the platform's
	// errors wherever the overlay doesn't describe that status itself.
	responses := map[string]any{}
	given, _ := o["responses"].(map[string]any)
	success := false
	for status, r := range given {
		responses[status] = r
		if strings.HasPrefix(status, "2") {
			success = true
		}
	}
	if !success {
		problems = append(problems, "responses: needs the success response (2xx)")
	}
	statusErrors := platformErrors(e)
	switch e.ErrorModel {
	case manifest.ErrorModelPrometheus:
		statusErrors = prometheusErrors(e)
	case manifest.ErrorModelOpenAI:
		statusErrors = openAIErrors(e)
	}
	for status, name := range statusErrors {
		if _, ok := responses[status]; !ok {
			responses[status] = map[string]any{"$ref": "#/components/responses/" + name}
		}
	}
	op["responses"] = responses

	// Any of the three customer credentials (contracts/identity-and-authz.md): the bearer ones
	// first, then the browser's cookie, which a bearer-only endpoint never reads.
	security := []any{
		map[string]any{"apiKey": []any{}},
		map[string]any{"oauth": []any{}},
	}
	if e.Credentials != manifest.CredentialsBearer {
		security = append(security, map[string]any{"sessionCookie": []any{}})
	}
	op["security"] = security
	if e.Action != "" {
		op["x-ezgh-action"] = e.Action
	}
	if len(e.Quotas) > 0 {
		var ids []any
		for _, q := range e.Quotas {
			ids = append(ids, q.ID)
		}
		op["x-ezgh-quotas"] = ids
	}
	return op, problems
}

// platformErrors are the errors every customer endpoint can answer (contracts/api-conventions.md,
// "Errors"), by status.
func platformErrors(e manifest.Endpoint) map[string]string {
	out := map[string]string{
		"400": "InvalidRequest",
		"401": "Unauthenticated",
		"403": "AccessDenied",
		"404": "NotFound",
		"429": "TooManyRequests",
		"500": "InternalError",
		"503": "Unavailable",
	}
	if e.Method != "GET" || len(e.Quotas) > 0 {
		out["409"] = "Conflict"
	}
	if e.Method == "POST" || e.Method == "PUT" || e.Method == "PATCH" {
		out["413"] = "PayloadTooLarge"
	}
	return out
}

// prometheusErrors are the errors an endpoint with errorModel prometheus answers, in the Prometheus
// HTTP API's body (contracts/api-conventions.md, "Prometheus query surface"), by status.
func prometheusErrors(e manifest.Endpoint) map[string]string {
	out := map[string]string{
		"400": "PrometheusBadData",
		"401": "PrometheusUnauthorized",
		"403": "PrometheusForbidden",
		"404": "PrometheusNotFound",
		"422": "PrometheusExecution",
		"429": "PrometheusTooManyRequests",
		"500": "PrometheusInternal",
		"503": "PrometheusUnavailable",
	}
	if e.Method == "POST" {
		out["413"] = "PrometheusPayloadTooLarge"
	}
	return out
}

// prometheusComponents are the kit's for endpoints with errorModel prometheus, in a document only
// when it has one: the PrometheusError body and its responses.
func prometheusComponents() map[string]map[string]any {
	body := map[string]any{"$ref": "#/components/schemas/PrometheusError"}
	response := func(description string, retry bool) map[string]any {
		headers := map[string]any{"X-Request-Id": map[string]any{"$ref": "#/components/headers/X-Request-Id"}}
		if retry {
			headers["Retry-After"] = map[string]any{"$ref": "#/components/headers/Retry-After"}
		}
		return map[string]any{
			"description": description,
			"headers":     headers,
			"content":     map[string]any{"application/json": map[string]any{"schema": body}},
		}
	}
	return map[string]map[string]any{
		"schemas": {
			"PrometheusError": map[string]any{
				"description": "Every non-2xx answer of a Prometheus HTTP API endpoint: Prometheus's error body, with the platform's code. Prometheus clients read errorType and error; branch on code, never on error.",
				"type":        "object",
				"required":    []any{"status", "errorType", "error", "code"},
				"properties": map[string]any{
					"status":    map[string]any{"const": "error"},
					"errorType": map[string]any{"type": "string", "description": "bad_data, unauthorized, forbidden, not_found, execution, timeout, unavailable or internal. Treat it as an open string."},
					"error":     map[string]any{"type": "string", "description": "What went wrong, for people."},
					"code":      map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64, "description": "The platform's error code, as in every other EZGH Cloud API."},
					"budget":    map[string]any{"type": "string", "description": "query_too_expensive: the budget the query ran over."},
				},
				"additionalProperties": true,
			},
		},
		"responses": {
			"PrometheusBadData":         response("bad_data (invalid_request): a parameter, the query's syntax or the request body isn't valid.", false),
			"PrometheusUnauthorized":    response("unauthorized (unauthenticated): no credential, or one that doesn't check out.", false),
			"PrometheusForbidden":       response("forbidden (access_denied, forbidden_origin): the caller's policies don't allow the action, or a cookie-authenticated POST came from an origin that isn't allowed.", false),
			"PrometheusNotFound":        response("not_found (not_found): unknown, malformed, or in an organization the caller isn't in.", false),
			"PrometheusPayloadTooLarge": response("bad_data (payload_too_large): the form body is over the endpoint's limit.", false),
			"PrometheusExecution":       response("execution or timeout (query_too_expensive, with budget; query_failed): the query couldn't be evaluated within its limits.", false),
			"PrometheusTooManyRequests": response("unavailable (too_many_requests), with Retry-After.", true),
			"PrometheusInternal":        response("internal (internal_error): a bug; the details are in the logs, under the request ID.", false),
			"PrometheusUnavailable":     response("unavailable (unavailable): a dependency the request needs is down. Retry after Retry-After.", true),
		},
	}
}

// openAIErrors are the errors an endpoint with errorModel openai answers, in the OpenAI API's body
// (contracts/api-conventions.md, "OpenAI-compatible surface"), by status. A product documents its
// own (EZGH Inference's 410, 502 and 504) in its overlay.
func openAIErrors(e manifest.Endpoint) map[string]string {
	out := map[string]string{
		"400": "OpenAIInvalidRequest",
		"401": "OpenAIUnauthenticated",
		"403": "OpenAIAccessDenied",
		"404": "OpenAINotFound",
		"429": "OpenAITooManyRequests",
		"500": "OpenAIInternalError",
		"503": "OpenAIUnavailable",
	}
	if e.IdempotencyKey || len(e.Quotas) > 0 {
		out["409"] = "OpenAIConflict"
	}
	if e.Method == "POST" {
		out["413"] = "OpenAIPayloadTooLarge"
	}
	return out
}

// openAIComponents are the kit's for endpoints with errorModel openai, in a document only when it
// has one: the OpenAIError body and its responses.
func openAIComponents() map[string]map[string]any {
	body := map[string]any{"$ref": "#/components/schemas/OpenAIError"}
	response := func(description string, retry bool) map[string]any {
		headers := map[string]any{"X-Request-Id": map[string]any{"$ref": "#/components/headers/X-Request-Id"}}
		if retry {
			headers["Retry-After"] = map[string]any{"$ref": "#/components/headers/Retry-After"}
		}
		return map[string]any{
			"description": description,
			"headers":     headers,
			"content":     map[string]any{"application/json": map[string]any{"schema": body}},
		}
	}
	return map[string]map[string]any{
		"schemas": {
			"OpenAIError": map[string]any{
				"description": "Every non-2xx answer of an OpenAI-compatible endpoint: the OpenAI API's error body, with the platform's code. OpenAI clients read type; branch on type and code, never on message.",
				"type":        "object",
				"required":    []any{"error"},
				"properties": map[string]any{
					"error": map[string]any{
						"type":     "object",
						"required": []any{"message", "type", "param", "code"},
						"properties": map[string]any{
							"message": map[string]any{"type": "string", "description": "What went wrong, for people. A validation failure's problems are appended to it."},
							"type":    map[string]any{"type": "string", "description": "invalid_request_error, authentication_error, permission_error, not_found_error, rate_limit_error, insufficient_quota or api_error. Treat it as an open string."},
							"param":   map[string]any{"type": []any{"string", "null"}, "description": "The request field at fault (input, input[3], dimensions), or null."},
							"code":    map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64, "description": "The platform's error code, as in every other EZGH Cloud API."},
						},
						"additionalProperties": true,
					},
				},
				"additionalProperties": false,
			},
		},
		"responses": {
			"OpenAIInvalidRequest":  response("invalid_request_error (invalid_request, or the product's own code): the request isn't valid; param names the field.", false),
			"OpenAIUnauthenticated": response("authentication_error (unauthenticated): no bearer credential, or one that doesn't check out. The session cookie isn't read.", false),
			"OpenAIAccessDenied":    response("permission_error (access_denied): the caller's policies don't allow the action.", false),
			"OpenAINotFound":        response("not_found_error (not_found, or the product's own code): unknown, malformed, or in an organization the caller isn't in.", false),
			"OpenAIConflict":        response("invalid_request_error: the Idempotency-Key was used with a different request, or a quota refused it.", false),
			"OpenAIPayloadTooLarge": response("invalid_request_error (payload_too_large): the body is over the endpoint's limit.", false),
			"OpenAITooManyRequests": response("rate_limit_error (too_many_requests), with Retry-After; or insufficient_quota (insufficient_quota), a monthly limit, without one and with x-should-retry: false.", true),
			"OpenAIInternalError":   response("api_error (internal_error): a bug; the details are in the logs, under the request ID.", false),
			"OpenAIUnavailable":     response("api_error (unavailable): a dependency the request needs is down. Retry after Retry-After.", true),
		},
	}
}

// platformComponents are the kit's: the error body, the platform's error responses, the
// credentials, Idempotency-Key and the headers every product sends. Customers read them: name
// public concepts only, never internal services, repository paths or local development.
func platformComponents() map[string]map[string]any {
	errorBody := map[string]any{"$ref": "#/components/schemas/Error"}
	requestID := map[string]any{"$ref": "#/components/headers/X-Request-Id"}
	retryAfter := map[string]any{"$ref": "#/components/headers/Retry-After"}
	response := func(description string, retry bool) map[string]any {
		headers := map[string]any{"X-Request-Id": requestID}
		if retry {
			headers["Retry-After"] = retryAfter
		}
		return map[string]any{
			"description": description,
			"headers":     headers,
			"content":     map[string]any{"application/json": map[string]any{"schema": errorBody}},
		}
	}
	return map[string]map[string]any{
		"schemas": {
			"Error": map[string]any{
				"description": "Every non-2xx answer. Clients branch on code, never on message.",
				"type":        "object",
				"required":    []any{"error"},
				"properties": map[string]any{
					"error": map[string]any{
						"type":     "object",
						"required": []any{"code", "message"},
						"properties": map[string]any{
							"code":    map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64},
							"message": map[string]any{"type": "string", "maxLength": 1024},
							"issues": map[string]any{
								"description": "invalid_request: one entry per problem, at a dotted path.",
								"type":        "array",
								"items": map[string]any{
									"type":       "object",
									"required":   []any{"path", "message"},
									"properties": map[string]any{"path": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}},
								},
							},
							"quota": map[string]any{"$ref": "#/components/schemas/QuotaExceeded"},
						},
						"additionalProperties": true,
					},
				},
				"additionalProperties": false,
			},
			"QuotaExceeded": map[string]any{
				"description": "quota_exceeded: the quota that refused the request. Never with Retry-After.",
				"type":        "object",
				"required":    []any{"id", "kind", "scope", "limit"},
				"properties": map[string]any{
					"id":                map[string]any{"type": "string"},
					"kind":              map[string]any{"enum": []any{"allocation", "rate", "usage", "concurrency"}},
					"scope":             map[string]any{"enum": []any{"organization", "project", "resource"}},
					"scopeResourceName": map[string]any{"type": "string"},
					"region":            map[string]any{"type": "string"},
					"limit":             map[string]any{"type": "integer", "minimum": 0},
					"used":              map[string]any{"type": "integer", "minimum": 0},
					"window":            map[string]any{"type": "string", "description": "rate only: an ISO 8601 duration, PT1S to PT1H."},
					"periodStart":       map[string]any{"type": "string", "format": "date-time"},
					"resetsAt":          map[string]any{"type": "string", "format": "date-time"},
					"inFlight":          map[string]any{"type": "integer", "minimum": 0},
				},
				"additionalProperties": false,
			},
		},
		"responses": {
			"InvalidRequest":  response("invalid_request (with issues), invalid_cursor, or the endpoint's own 400 codes.", false),
			"Unauthenticated": response("unauthenticated: no credential, or one that doesn't check out.", false),
			"AccessDenied":    response("access_denied: the caller's policies don't allow the action; forbidden_origin: a cookie-authenticated write from an origin that isn't allowed; spend_limit_reached.", false),
			"NotFound":        response("not_found: unknown, malformed, or in an organization the caller isn't in.", false),
			"Conflict":        response("quota_exceeded (an allocation quota, with quota), idempotency_conflict, idempotency_in_progress (with Retry-After), organization_deletion_pending, operation_in_progress, or the endpoint's own 409 codes.", true),
			"PayloadTooLarge": response("payload_too_large: the body is over the endpoint's limit (1 MiB unless it says otherwise).", false),
			"TooManyRequests": response("too_many_requests, with Retry-After; or quota_exceeded (a rate, usage or concurrency quota, with quota), never with Retry-After.", true),
			"InternalError":   response("internal_error: a bug; the details are in the logs, under the request ID.", false),
			"Unavailable":     response("unavailable: a dependency the request needs is down. Retry after Retry-After.", true),
		},
		"parameters": {
			"IdempotencyKey": map[string]any{
				"name":        "Idempotency-Key",
				"in":          "header",
				"required":    false,
				"description": "Makes a retry safe: the same key and request get the stored response.",
				"schema":      map[string]any{"type": "string", "pattern": "^[\\x21-\\x7e]{1,200}$"},
			},
		},
		"headers": {
			"X-Request-Id": map[string]any{"description": "The request's ID, in every log line and its Trails event.", "schema": map[string]any{"type": "string"}},
			"Retry-After":  map[string]any{"description": "Whole seconds to wait before retrying. Never on quota_exceeded.", "schema": map[string]any{"type": "integer", "minimum": 1}},
		},
		"securitySchemes": {
			"sessionCookie": map[string]any{"type": "apiKey", "in": "cookie", "name": "__Secure-better-auth.session_token", "description": "A signed-in person's session, set by login.ezghcloud.com."},
			"apiKey":        map[string]any{"type": "http", "scheme": "bearer", "description": "A customer API key: Bearer ezgh_…"},
			"oauth":         map[string]any{"type": "http", "scheme": "bearer", "description": "An OAuth access token (Bearer ezgh_at_…) or an EZGH SSO resource token (Bearer ezgh_sat_…)"},
		},
	}
}
