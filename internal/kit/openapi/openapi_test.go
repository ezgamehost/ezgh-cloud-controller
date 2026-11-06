package openapi_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/openapi"
)

func widgets(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// overlay describes every customer endpoint of m with a 200, except the ones in skip.
func overlay(m *manifest.Manifest, skip ...string) map[string]any {
	paths := map[string]any{}
	for _, e := range m.Endpoints {
		if !openapi.Customer(e) || slices.Contains(skip, e.Method+" "+e.Path) {
			continue
		}
		path := openapi.ToOpenAPIPath(e.Path)
		item, _ := paths[path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[path] = item
		}
		op := map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}
		if e.Trail == nil {
			op["operationId"] = "GetOperation"
		}
		item[strings.ToLower(e.Method)] = op
	}
	return map[string]any{"info": map[string]any{"title": "Widgets API", "version": "1.0.0"}, "paths": paths}
}

func build(t *testing.T, m *manifest.Manifest, o map[string]any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(o)
	doc, err := openapi.Build(m, raw)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(doc, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func TestTheDocumentIsTheCustomerEndpoints(t *testing.T) {
	m := widgets(t)
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	ops, _ := openapi.Operations(raw)
	var want []string
	for _, e := range m.Endpoints {
		if openapi.Customer(e) {
			want = append(want, e.Method+" "+e.Path)
		}
	}
	slices.Sort(want)
	if !slices.Equal(ops, want) || len(ops) != 10 {
		t.Fatalf("operations %v\nwant %v", ops, want)
	}
	if doc["openapi"] != "3.1.0" || doc["servers"].([]any)[0].(map[string]any)["url"] != "https://widgets.ezghcloud.com" {
		t.Fatalf("header: %v %v", doc["openapi"], doc["servers"])
	}
	if strings.Contains(string(raw), "/internal/") || strings.Contains(string(raw), `"/health"`) || strings.Contains(string(raw), `"/openapi.json"`) {
		t.Fatal("the document has a route that isn't a customer endpoint")
	}

	// What the manifest says: the operation ID, the summary from the action, path parameters,
	// Idempotency-Key, the credentials and the platform's errors.
	create := doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/widgets"].(map[string]any)["post"].(map[string]any)
	if create["operationId"] != "CreateWidget" || create["summary"] != "Create widgets" || create["x-ezgh-action"] != "widgets.widgets.create" {
		t.Fatalf("create: %v", create)
	}
	params, _ := json.Marshal(create["parameters"])
	if string(params) != `[{"in":"path","name":"orgId","required":true,"schema":{"description":"UUID or prefixed ID (org_…)","type":"string"}},{"in":"path","name":"projectId","required":true,"schema":{"description":"UUID or prefixed ID (prj_…)","type":"string"}},{"$ref":"#/components/parameters/IdempotencyKey"}]` {
		t.Fatalf("parameters: %s", params)
	}
	responses := create["responses"].(map[string]any)
	for _, status := range []string{"200", "400", "401", "403", "404", "409", "413", "429", "500", "503"} {
		if responses[status] == nil {
			t.Errorf("create has no %s", status)
		}
	}
	// The three customer credentials, the bearer ones first.
	var schemes []string
	for _, requirement := range create["security"].([]any) {
		for name := range requirement.(map[string]any) {
			schemes = append(schemes, name)
		}
	}
	if strings.Join(schemes, ",") != "apiKey,oauth,sessionCookie" {
		t.Errorf("security: %v", create["security"])
	}
	list := doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/widgets"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)
	if list["413"] != nil {
		t.Error("a GET has no body to be too large")
	}
	components := doc["components"].(map[string]any)
	for _, name := range []string{"schemas", "responses", "parameters", "headers", "securitySchemes"} {
		if components[name] == nil {
			t.Errorf("components.%s", name)
		}
	}
}

func TestOverlayRefinements(t *testing.T) {
	m := widgets(t)
	o := overlay(m)
	list := o["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/widgets"].(map[string]any)["get"].(map[string]any)
	list["summary"] = "List a project's widgets"
	list["parameters"] = []any{
		map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}},
		map[string]any{"name": "projectId", "in": "path", "description": "The project"},
	}
	list["responses"] = map[string]any{"200": map[string]any{"description": "OK"}, "400": map[string]any{"description": "Its own 400"}}
	o["components"] = map[string]any{"schemas": map[string]any{"Widget": map[string]any{"type": "object"}}}
	doc, err := build(t, m, o)
	if err != nil {
		t.Fatal(err)
	}
	op := doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/widgets"].(map[string]any)["get"].(map[string]any)
	params := op["parameters"].([]any)
	if op["summary"] != "List a project's widgets" || len(params) != 3 || params[1].(map[string]any)["description"] != "The project" || params[1].(map[string]any)["required"] != true || params[2].(map[string]any)["name"] != "limit" {
		t.Fatalf("%v", op)
	}
	if op["responses"].(map[string]any)["400"].(map[string]any)["description"] != "Its own 400" {
		t.Fatal("the overlay's own response for a status wins")
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if schemas["Widget"] == nil || schemas["Error"] == nil {
		t.Fatalf("schemas: %v", schemas)
	}
}

// Every way the overlay and the manifest can drift apart is refused, with where.
func TestDriftIsRefused(t *testing.T) {
	m := widgets(t)
	o := overlay(m, "DELETE /v1/organizations/:orgId/operations/:operationId")
	paths := o["paths"].(map[string]any)
	paths["/v1/organizations/{orgId}/gadgets"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	paths["/internal/v1/usage/totals"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	widget := paths["/v1/organizations/{orgId}/projects/{projectId}/widgets/{widgetId}"].(map[string]any)
	widget["get"].(map[string]any)["responses"] = map[string]any{"404": map[string]any{"description": "x"}}
	widget["get"].(map[string]any)["security"] = []any{}
	widget["patch"].(map[string]any)["operationId"] = "PatchWidget"
	widget["delete"].(map[string]any)["parameters"] = []any{map[string]any{"name": "gadgetId", "in": "path"}}
	widget["parameters"] = []any{}
	paths["/v1/organizations/{orgId}/operations/{operationId}"].(map[string]any)["get"].(map[string]any)["operationId"] = ""
	o["servers"] = []any{}
	delete(o["info"].(map[string]any), "version")
	o["components"] = map[string]any{"schemas": map[string]any{"Error": map[string]any{}}}

	_, err := build(t, m, o)
	if err == nil {
		t.Fatal("drift was accepted")
	}
	for _, want := range []string{
		"DELETE /v1/organizations/:orgId/operations/:operationId: a customer endpoint with no operation in the overlay",
		"GET /v1/organizations/:orgId/gadgets: in the overlay, but not a route in the manifest",
		"GET /internal/v1/usage/totals: isn't a customer endpoint, so it isn't in the document",
		"GET /v1/organizations/:orgId/projects/:projectId/widgets/:widgetId: responses: needs the success response (2xx)",
		"GET /v1/organizations/:orgId/projects/:projectId/widgets/:widgetId: security: the kit writes it, or it isn't used",
		"PATCH /v1/organizations/:orgId/projects/:projectId/widgets/:widgetId: operationId is the Trails eventName, UpdateWidget",
		"DELETE /v1/organizations/:orgId/projects/:projectId/widgets/:widgetId: parameters[0]: gadgetId isn't a parameter of the path",
		"paths./v1/organizations/{orgId}/projects/{projectId}/widgets/{widgetId}.parameters: a path has only operations",
		"GET /v1/organizations/:orgId/operations/:operationId: operationId: required when the endpoint has no Trails eventName",
		"servers: an overlay has only info, paths, components, tags, externalDocs",
		"info.version: required",
		"components.schemas.Error: the kit defines it",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("no %q in\n%v", want, err)
		}
	}
	if _, err := openapi.Build(m, []byte("[]")); err == nil || !strings.Contains(err.Error(), "the overlay isn't a JSON object") {
		t.Errorf("not an object: %v", err)
	}
}

func TestPaths(t *testing.T) {
	if got := openapi.ToOpenAPIPath("/v1/organizations/:orgId/quotas/:quota"); got != "/v1/organizations/{orgId}/quotas/{quota}" {
		t.Error(got)
	}
	if got := openapi.ToManifestPath("/v1/organizations/{orgId}/quotas/{quota}"); got != "/v1/organizations/:orgId/quotas/:quota" {
		t.Error(got)
	}
}

func TestReadAliasesUseUniqueOverlayOperationIDs(t *testing.T) {
	m := widgets(t)
	var alias manifest.Endpoint
	for _, e := range m.Endpoints {
		if e.Method == "GET" && e.Action == "widgets.widgets.get" {
			alias = e
			break
		}
	}
	alias.Path += "/download"
	m.Endpoints = append(m.Endpoints, alias)
	o := overlay(m)
	if _, err := build(t, m, o); err == nil {
		t.Fatal("duplicate operation IDs accepted")
	}
	op := o["paths"].(map[string]any)[openapi.ToOpenAPIPath(alias.Path)].(map[string]any)["get"].(map[string]any)
	op["operationId"] = "DownloadWidget"
	doc, err := build(t, m, o)
	if err != nil {
		t.Fatal(err)
	}
	got := doc["paths"].(map[string]any)[openapi.ToOpenAPIPath(alias.Path)].(map[string]any)["get"].(map[string]any)["operationId"]
	if got != "DownloadWidget" {
		t.Fatal(got)
	}
}

// The document is for customers: the kit's parts name public concepts only, never internal
// services, repository paths or local-development details.
func TestPlatformPartsNameNoInternals(t *testing.T) {
	m := widgets(t)
	raw, err := openapi.Build(m, mustJSON(t, overlay(m)))
	if err != nil {
		t.Fatal(err)
	}
	for _, internal := range []string{"auth-api", "org-api", "contracts/", ".md", "locally"} {
		if strings.Contains(string(raw), internal) {
			t.Errorf("the document mentions %q", internal)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Ingest-host endpoints aren't customer endpoints of the document: OTLP describes them.
func TestIngestEndpointsAreNotInTheDocument(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	ingest := 0
	for _, e := range m.Endpoints {
		if e.OnIngestHost() {
			ingest++
			if openapi.Customer(e) {
				t.Errorf("%s %s is in the document", e.Method, e.Path)
			}
		}
	}
	if ingest != 4 {
		t.Fatalf("%d ingest endpoints", ingest)
	}
}

// Edge-host endpoints (https://<alias>.<edge domain>) aren't in the API host's document, and an
// overlay that describes one is refused like any other non-customer operation.
func TestEdgeEndpointsAreNotInTheDocument(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	o := overlay(m)
	doc, err := build(t, m, o)
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	if len(paths) != 1 || paths["/v1/organizations/{orgId}/projects/{projectId}/gateway"] == nil {
		t.Fatalf("paths %v", paths)
	}
	o["paths"].(map[string]any)["/central"] = map[string]any{"post": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	if _, err := build(t, m, o); err == nil {
		t.Fatal("an overlay describing an edge-host endpoint was accepted")
	}
}

// Live-host endpoints are customer endpoints: the API host's document describes them, with a
// path-level servers entry naming the live host, or the operation's own when its path also has
// API-host operations. An overlay can't say servers itself.
func TestLiveEndpointsNameTheLiveHost(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	tail := "/v1/organizations/{orgId}/projects/{projectId}/obs/datasets/{datasetId}/logs/tail"
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	liveServers := []any{map[string]any{"url": "https://obs-live.ezghcloud.com"}}
	paths := doc["paths"].(map[string]any)
	item := paths[tail].(map[string]any)
	if !reflect.DeepEqual(item["servers"], liveServers) || item["post"].(map[string]any)["operationId"] != "TailLogs" {
		t.Fatalf("tail %v", item)
	}
	if _, ok := paths["/v1/organizations/{orgId}/projects/{projectId}/obs/datasets"].(map[string]any)["servers"]; ok {
		t.Error("an API-host path names a server")
	}
	if !reflect.DeepEqual(doc["servers"], []any{map[string]any{"url": "https://obs.ezghcloud.com"}}) {
		t.Errorf("document servers %v", doc["servers"])
	}

	// A path with operations on both hosts: only the live operation names the live host.
	mixed := *m
	mixed.Endpoints = append(slices.Clone(m.Endpoints), manifest.Endpoint{Method: "PUT", Path: "/v1/organizations/:orgId/projects/:projectId/obs/datasets/:datasetId/logs/tail", Auth: "caller", Action: "obs.datasets.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "PutTail"}})
	doc, err = build(t, &mixed, overlay(&mixed))
	if err != nil {
		t.Fatal(err)
	}
	item = doc["paths"].(map[string]any)[tail].(map[string]any)
	if _, ok := item["servers"]; ok {
		t.Errorf("a mixed path names a server: %v", item)
	}
	if !reflect.DeepEqual(item["post"].(map[string]any)["servers"], liveServers) {
		t.Errorf("live operation %v", item["post"])
	}
	if _, ok := item["put"].(map[string]any)["servers"]; ok {
		t.Errorf("API-host operation names a server: %v", item["put"])
	}

	o := overlay(m)
	o["paths"].(map[string]any)[tail].(map[string]any)["servers"] = liveServers
	if _, err := build(t, m, o); err == nil {
		t.Error("an overlay naming servers was accepted")
	}
}

// An endpoint with errorModel prometheus documents the PrometheusError responses instead of the
// platform's; a computation POST sharing its GET's event is <eventName>Post unless the overlay
// names it; and a document without such endpoints has no Prometheus components.
func TestPrometheusErrorModel(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	o := overlay(m)
	doc, err := build(t, m, o)
	if err != nil {
		t.Fatal(err)
	}
	item := doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/obs/prometheus/api/v1/query"].(map[string]any)
	get, post := item["get"].(map[string]any), item["post"].(map[string]any)
	if get["operationId"] != "QueryMetrics" || post["operationId"] != "QueryMetricsPost" {
		t.Errorf("operation IDs %v %v", get["operationId"], post["operationId"])
	}
	ref := func(op map[string]any, status string) any {
		r, _ := op["responses"].(map[string]any)[status].(map[string]any)
		return r["$ref"]
	}
	for status, name := range map[string]string{"400": "PrometheusBadData", "401": "PrometheusUnauthorized", "403": "PrometheusForbidden", "404": "PrometheusNotFound", "422": "PrometheusExecution", "429": "PrometheusTooManyRequests", "500": "PrometheusInternal", "503": "PrometheusUnavailable"} {
		for _, op := range []map[string]any{get, post} {
			if got := ref(op, status); got != "#/components/responses/"+name {
				t.Errorf("%s: %v", status, got)
			}
		}
	}
	if ref(post, "413") != "#/components/responses/PrometheusPayloadTooLarge" || ref(get, "413") != nil || ref(post, "409") != nil {
		t.Errorf("413/409: %v %v %v", ref(post, "413"), ref(get, "413"), ref(post, "409"))
	}
	components := doc["components"].(map[string]any)
	if components["schemas"].(map[string]any)["PrometheusError"] == nil || components["schemas"].(map[string]any)["Error"] == nil {
		t.Error("the error schemas are missing")
	}
	// Other operations keep the platform's errors.
	ds := doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/obs/datasets"].(map[string]any)["get"].(map[string]any)
	if ref(ds, "400") != "#/components/responses/InvalidRequest" {
		t.Errorf("datasets 400 %v", ref(ds, "400"))
	}

	// The overlay may name the POST; it can't reuse the GET's.
	o["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/obs/prometheus/api/v1/query"].(map[string]any)["post"].(map[string]any)["operationId"] = "QueryMetricsForm"
	if doc, err = build(t, m, o); err != nil || doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/obs/prometheus/api/v1/query"].(map[string]any)["post"].(map[string]any)["operationId"] != "QueryMetricsForm" {
		t.Errorf("a named POST: %v", err)
	}
	o["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/obs/prometheus/api/v1/query"].(map[string]any)["post"].(map[string]any)["operationId"] = "QueryMetrics"
	if _, err := build(t, m, o); err == nil || !strings.Contains(err.Error(), "operationId QueryMetrics is also") {
		t.Errorf("a duplicate operation ID: %v", err)
	}

	w := widgets(t)
	doc, err = build(t, w, overlay(w))
	if err != nil {
		t.Fatal(err)
	}
	if doc["components"].(map[string]any)["schemas"].(map[string]any)["PrometheusError"] != nil {
		t.Error("a document without Prometheus endpoints has PrometheusError")
	}
}

// An endpoint with errorModel openai documents the OpenAIError responses instead of the
// platform's, and a bearer-only endpoint offers no session cookie; a document without such
// endpoints has no OpenAI components.
func TestOpenAIErrorModel(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/inference.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	post := paths["/v1/organizations/{orgId}/projects/{projectId}/openai/v1/embeddings"].(map[string]any)["post"].(map[string]any)
	get := paths["/v1/organizations/{orgId}/projects/{projectId}/openai/v1/models"].(map[string]any)["get"].(map[string]any)
	ref := func(op map[string]any, status string) any {
		r, _ := op["responses"].(map[string]any)[status].(map[string]any)
		return r["$ref"]
	}
	for status, name := range map[string]string{"400": "OpenAIInvalidRequest", "401": "OpenAIUnauthenticated", "403": "OpenAIAccessDenied", "404": "OpenAINotFound", "429": "OpenAITooManyRequests", "500": "OpenAIInternalError", "503": "OpenAIUnavailable"} {
		for _, op := range []map[string]any{get, post} {
			if got := ref(op, status); got != "#/components/responses/"+name {
				t.Errorf("%s: %v", status, got)
			}
		}
	}
	if ref(post, "413") != "#/components/responses/OpenAIPayloadTooLarge" || ref(get, "413") != nil || ref(post, "409") != nil {
		t.Errorf("413/409: %v %v %v", ref(post, "413"), ref(get, "413"), ref(post, "409"))
	}
	bearerOnly := []any{map[string]any{"apiKey": []any{}}, map[string]any{"oauth": []any{}}}
	for _, op := range []map[string]any{get, post} {
		if !reflect.DeepEqual(op["security"], bearerOnly) {
			t.Errorf("security %v", op["security"])
		}
	}
	components := doc["components"].(map[string]any)
	schema, _ := components["schemas"].(map[string]any)["OpenAIError"].(map[string]any)
	if schema == nil || components["schemas"].(map[string]any)["Error"] == nil {
		t.Fatal("the error schemas are missing")
	}
	inner := schema["properties"].(map[string]any)["error"].(map[string]any)
	if !reflect.DeepEqual(inner["required"], []any{"message", "type", "param", "code"}) {
		t.Errorf("OpenAIError %v", inner)
	}
	// Other operations keep the platform's errors and every credential.
	limits := paths["/v1/organizations/{orgId}/projects/{projectId}/inference/limits"].(map[string]any)["get"].(map[string]any)
	if ref(limits, "400") != "#/components/responses/InvalidRequest" || len(limits["security"].([]any)) != 3 {
		t.Errorf("limits %v %v", ref(limits, "400"), limits["security"])
	}

	// An OpenAI POST that keeps Idempotency-Key can conflict.
	copied := *m
	keyed := &copied
	keyed.Endpoints = slices.Clone(m.Endpoints)
	for i, e := range keyed.Endpoints {
		if e.Method == "POST" && e.ErrorModel == manifest.ErrorModelOpenAI {
			keyed.Endpoints[i].IdempotencyException, keyed.Endpoints[i].IdempotencyKey = "", true
		}
	}
	if doc, err = build(t, keyed, overlay(keyed)); err != nil {
		t.Fatal(err)
	}
	post = doc["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/openai/v1/embeddings"].(map[string]any)["post"].(map[string]any)
	if ref(post, "409") != "#/components/responses/OpenAIConflict" {
		t.Errorf("409 %v", ref(post, "409"))
	}
	if doc["components"].(map[string]any)["responses"].(map[string]any)["OpenAIConflict"] == nil {
		t.Error("OpenAIConflict isn't in the document")
	}

	w := widgets(t)
	doc, err = build(t, w, overlay(w))
	if err != nil {
		t.Fatal(err)
	}
	if doc["components"].(map[string]any)["schemas"].(map[string]any)["OpenAIError"] != nil {
		t.Error("a document without OpenAI endpoints has OpenAIError")
	}
}

// Registry-host endpoints (the product's own OCI server) aren't customer endpoints of the
// document: the OCI distribution spec describes them.
func TestRegistryEndpointsAreNotInTheDocument(t *testing.T) {
	var v struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	vectors.Load(t, "manifest-registry.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	registry := 0
	for _, e := range m.Endpoints {
		if e.OnRegistryHost() {
			registry++
			if openapi.Customer(e) {
				t.Errorf("%s %s is in the document", e.Method, e.Path)
			}
		}
	}
	if registry != 16 {
		t.Fatalf("%d registry endpoints", registry)
	}
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	for path := range doc["paths"].(map[string]any) {
		if strings.HasPrefix(path, "/v2") {
			t.Errorf("%s is in the document", path)
		}
	}
	// An overlay that describes one is refused, like any other non-customer operation.
	o := overlay(m)
	o["paths"].(map[string]any)["/v2/"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	if _, err := build(t, m, o); err == nil {
		t.Fatal("an overlay describing GET /v2/ was accepted")
	}
}

// Data-host endpoints (the product's own data server, auth productKey) aren't customer endpoints
// of the document: their keys aren't customer credentials, and OFREP describes Feature Flags'.
func TestDataEndpointsAreNotInTheDocument(t *testing.T) {
	var v struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	vectors.Load(t, "manifest-data.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	data := 0
	for _, e := range m.Endpoints {
		if e.OnDataHost() {
			data++
			if openapi.Customer(e) {
				t.Errorf("%s %s is in the document", e.Method, e.Path)
			}
		}
	}
	if data != 3 {
		t.Fatalf("%d data endpoints", data)
	}
	// Customer stays false for a data endpoint whatever its auth says.
	if openapi.Customer(manifest.Endpoint{Method: "POST", Path: "/v1/sync/stream", Host: manifest.HostData, Auth: "caller"}) {
		t.Error("a data-host endpoint is a customer endpoint")
	}
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	for path := range doc["paths"].(map[string]any) {
		if strings.HasPrefix(path, "/ofrep") || strings.HasPrefix(path, "/v1/sync") {
			t.Errorf("%s is in the document", path)
		}
	}
	// An overlay that describes one is refused, like any other non-customer operation.
	o := overlay(m)
	o["paths"].(map[string]any)["/v1/sync/snapshot"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	if _, err := build(t, m, o); err == nil {
		t.Fatal("an overlay describing GET /v1/sync/snapshot was accepted")
	}
}

// PKI-host endpoints (the product's own PKI servers, auth none) aren't customer endpoints of the
// document: their protocols (RFC 5280, RFC 6960, RFC 8555) describe them.
func TestPKIEndpointsAreNotInTheDocument(t *testing.T) {
	var v struct {
		Manifest json.RawMessage `json:"manifest"`
	}
	vectors.Load(t, "manifest-pki.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	pki := 0
	for _, e := range m.Endpoints {
		if e.OnPKIHost() {
			pki++
			if openapi.Customer(e) {
				t.Errorf("%s %s is in the document", e.Method, e.Path)
			}
		}
	}
	if pki != 11 {
		t.Fatalf("%d PKI endpoints", pki)
	}
	// Customer stays false for a PKI endpoint whatever its auth says.
	if openapi.Customer(manifest.Endpoint{Method: "POST", Path: "/acme/:caId/new-order", Host: manifest.HostPKI, Auth: "caller"}) {
		t.Error("a PKI-host endpoint is a customer endpoint")
	}
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	for path := range doc["paths"].(map[string]any) {
		if strings.HasPrefix(path, "/acme") || strings.HasPrefix(path, "/crl") || strings.HasPrefix(path, "/ca/") || strings.HasPrefix(path, "/ocsp") {
			t.Errorf("%s is in the document", path)
		}
	}
	// An overlay that describes one is refused, like any other non-customer operation.
	o := overlay(m)
	o["paths"].(map[string]any)["/crl/{file}"] = map[string]any{"get": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	if _, err := build(t, m, o); err == nil {
		t.Fatal("an overlay describing GET /crl/{file} was accepted")
	}
}

// Receipt-handle endpoints (the live component's, authorized by the handles in their body) aren't
// customer endpoints of the document; the Queues excerpt's receive poll is, on the live host.
func TestReceiptEndpointsAreNotInTheDocument(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/queues.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	receipts := 0
	for _, e := range m.Endpoints {
		if e.ReceiptHandle() {
			receipts++
			if openapi.Customer(e) {
				t.Errorf("%s %s is in the document", e.Method, e.Path)
			}
		}
	}
	if receipts != 2 {
		t.Fatalf("%d receipt endpoints", receipts)
	}
	doc, err := build(t, m, overlay(m))
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for path := range paths {
		if strings.HasPrefix(path, manifest.ReceiptPathPrefix) {
			t.Errorf("%s is in the document", path)
		}
	}
	if _, ok := paths["/v1/organizations/{orgId}/projects/{projectId}/queues/{queueId}/messages/receive"]; !ok {
		t.Errorf("the receive poll is missing: %v", paths)
	}
	// An overlay that describes one is refused, like any other non-customer operation.
	o := overlay(m)
	o["paths"].(map[string]any)["/v1/receipts/ack"] = map[string]any{"post": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}}
	if _, err := build(t, m, o); err == nil {
		t.Fatal("an overlay describing POST /v1/receipts/ack was accepted")
	}
}

// A …Id parameter the service declared (service.PathIDs) takes a prefixed ID too; any other is a
// UUID.
func TestDeclaredPathIDs(t *testing.T) {
	m := widgets(t)
	raw, _ := json.Marshal(overlay(m))
	schemaOf := func(doc []byte, path, name string) string {
		var d map[string]any
		_ = json.Unmarshal(doc, &d)
		for _, p := range d["paths"].(map[string]any)[path].(map[string]any)["get"].(map[string]any)["parameters"].([]any) {
			if p.(map[string]any)["name"] == name {
				b, _ := json.Marshal(p.(map[string]any)["schema"])
				return string(b)
			}
		}
		return ""
	}
	const widget = "/v1/organizations/{orgId}/projects/{projectId}/widgets/{widgetId}"
	plain, err := openapi.Build(m, raw)
	if err != nil {
		t.Fatal(err)
	}
	declared, err := openapi.BuildWith(m, raw, map[string]string{"widgetId": "wgt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := schemaOf(plain, widget, "widgetId"); got != `{"format":"uuid","type":"string"}` {
		t.Errorf("undeclared: %s", got)
	}
	if got := schemaOf(declared, widget, "widgetId"); got != `{"description":"UUID or prefixed ID (wgt_…)","type":"string"}` {
		t.Errorf("declared: %s", got)
	}
}
