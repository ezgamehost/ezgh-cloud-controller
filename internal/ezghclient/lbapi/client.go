// Package lbapi is a typed client for lb-api (EZGH Load Balancers): a project's load balancers and
// their listeners, and target groups and their targets.
package lbapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/computeapi"
)

// Client calls lb-api.
type Client struct {
	API *apiclient.Client
}

// Operation is a create's or delete's 202: lb-api's operations are the contract's, like compute-api's.
type Operation = computeapi.Operation

// LastChange is how a load balancer's latest change is going. State is applying, applied or
// not_applied, with why.
type LastChange struct {
	Generation int     `json:"generation"`
	State      string  `json:"state"`
	Reason     *string `json:"reason"`
}

// LoadBalancer is a load balancer. Slug (lbr_…) is what customers name it by; a path also takes its
// ID, a UUID. Status is provisioning, active, deleting or deleted.
type LoadBalancer struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Name           string            `json:"name"`
	Description    *string           `json:"description"`
	Tags           map[string]string `json:"tags"`
	VPC            string            `json:"vpc"`
	Subnet         string            `json:"subnet"`
	// IPv4Address is nil on a load balancer with only IPv6. IPv6Address is nil while IPv6 is off or
	// on its way.
	IPv4Address *string     `json:"ipv4Address"`
	IPv6        bool        `json:"ipv6"`
	IPv6Address *string     `json:"ipv6Address"`
	Status      string      `json:"status"`
	LastChange  *LastChange `json:"lastChange"`
	CreatedAt   string      `json:"createdAt"`
	UpdatedAt   string      `json:"updatedAt"`
}

// LoadBalancerCreate is a create's body: Subnet is its sbn_ ID, IPv4Address its pip_ ID (optional
// with IPv6). IPv6 nil leaves it to lb-api: on where IPv6 is open.
type LoadBalancerCreate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Subnet      string            `json:"subnet"`
	IPv4Address string            `json:"ipv4Address,omitempty"`
	IPv6        *bool             `json:"ipv6,omitempty"`
}

// LoadBalancerUpdate is an update's body: only what's set changes. Tags replace its tags.
type LoadBalancerUpdate struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
	IPv6        *bool              `json:"ipv6,omitempty"`
}

// Listener is a record of its load balancer. ID (lsn_…) is its only name. Protocol is tcp, tls,
// http or https. Certificate is a tls or https listener's crt_ ID. TargetGroup is nil on an http or
// https listener whose default action isn't a forward to exactly one group; DefaultAction and Rules
// are an http or https listener's (nil and empty on tcp and tls).
type Listener struct {
	ID                 string  `json:"id"`
	LoadBalancer       string  `json:"loadBalancer"`
	Protocol           string  `json:"protocol"`
	Port               int     `json:"port"`
	TargetGroup        *string `json:"targetGroup"`
	DefaultAction      *Action `json:"defaultAction"`
	Rules              []Rule  `json:"rules"`
	IdleTimeoutSeconds int     `json:"idleTimeoutSeconds"`
	Certificate        *string `json:"certificate"`
	// AdditionalCertificates are a tls or https listener's certificates beyond Certificate, picked
	// by the client's SNI.
	AdditionalCertificates []string `json:"additionalCertificates"`
	AllowedSourceCidrs     []string `json:"allowedSourceCidrs"`
	CreatedAt              string   `json:"createdAt"`
	UpdatedAt              string   `json:"updatedAt"`
}

// Rule is one of an http or https listener's ordered rules: the first whose Match matches takes
// the request. Match's fields AND, and the entries within a field OR; an empty one matches anything.
type Rule struct {
	Name   *string `json:"name"`
	Match  Match   `json:"match"`
	Action Action  `json:"action"`
}

// Match is what a rule matches. Paths' Type is exact or prefix.
type Match struct {
	Hosts       []string    `json:"hosts"`
	Paths       []PathMatch `json:"paths"`
	Methods     []string    `json:"methods"`
	Headers     []NameValue `json:"headers"`
	QueryParams []NameValue `json:"queryParams"`
}

// PathMatch is a path to match: exact, or a prefix on segment boundaries.
type PathMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// NameValue is a header or query parameter to match exactly.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Action is forward, redirect or fixedResponse, with that one set.
type Action struct {
	Type          string         `json:"type"`
	Forward       *Forward       `json:"forward,omitempty"`
	Redirect      *Redirect      `json:"redirect,omitempty"`
	FixedResponse *FixedResponse `json:"fixedResponse,omitempty"`
}

// Forward sends each request to one of up to 5 http target groups, by weight.
type Forward struct {
	TargetGroups []WeightedGroup `json:"targetGroups"`
}

// WeightedGroup is a forward's group; a forward to one group may leave out its weight.
type WeightedGroup struct {
	TargetGroup string `json:"targetGroup"`
	Weight      *int   `json:"weight,omitempty"`
}

// Redirect replaces each part of the request's URL it sets; nil keeps the request's. The query is
// always kept. StatusCode is 301, 302 (the default), 307 or 308.
type Redirect struct {
	Scheme     *string `json:"scheme,omitempty"`
	Host       *string `json:"host,omitempty"`
	Port       *int    `json:"port,omitempty"`
	Path       *string `json:"path,omitempty"`
	StatusCode *int    `json:"statusCode,omitempty"`
}

// FixedResponse answers itself. ContentType is text/plain (the default), text/html or
// application/json; Body is at most 1024 bytes.
type FixedResponse struct {
	StatusCode  int     `json:"statusCode"`
	ContentType *string `json:"contentType,omitempty"`
	Body        *string `json:"body,omitempty"`
}

// ListenerCreate is a create's body: TargetGroup for tcp and tls; DefaultAction (or TargetGroup
// alone, for a forward to it) and Rules for http and https. Rules is lb-api's own JSON, as given.
type ListenerCreate struct {
	Protocol               string          `json:"protocol"`
	Port                   int             `json:"port"`
	TargetGroup            string          `json:"targetGroup,omitempty"`
	DefaultAction          *Action         `json:"defaultAction,omitempty"`
	Rules                  json.RawMessage `json:"rules,omitempty"`
	IdleTimeoutSeconds     *int            `json:"idleTimeoutSeconds,omitempty"`
	Certificate            *string         `json:"certificate,omitempty"`
	AdditionalCertificates []string        `json:"additionalCertificates,omitempty"`
	AllowedSourceCidrs     []string        `json:"allowedSourceCidrs,omitempty"`
}

// ListenerUpdate is an update's body: only what's set changes. Protocol changes only within a layer
// (tcp and tls, http and https). Rules replaces the whole list.
type ListenerUpdate struct {
	Protocol           *string         `json:"protocol,omitempty"`
	Port               *int            `json:"port,omitempty"`
	TargetGroup        *string         `json:"targetGroup,omitempty"`
	DefaultAction      *Action         `json:"defaultAction,omitempty"`
	Rules              json.RawMessage `json:"rules,omitempty"`
	IdleTimeoutSeconds *int            `json:"idleTimeoutSeconds,omitempty"`
	Certificate        *string         `json:"certificate,omitempty"`
	// AdditionalCertificates replaces the list; a pointer to an empty one empties it.
	AdditionalCertificates *[]string `json:"additionalCertificates,omitempty"`
	AllowedSourceCidrs     []string  `json:"allowedSourceCidrs,omitempty"`
}

// HealthCheck is how a target group checks its targets. Port nil is the target's own.
type HealthCheck struct {
	Protocol           string `json:"protocol"`
	Port               *int   `json:"port"`
	Path               string `json:"path"`
	IntervalSeconds    int    `json:"intervalSeconds"`
	TimeoutSeconds     int    `json:"timeoutSeconds"`
	HealthyThreshold   int    `json:"healthyThreshold"`
	UnhealthyThreshold int    `json:"unhealthyThreshold"`
	Matcher            string `json:"matcher"`
}

// HealthCheckInput is a health check in a request: only what's set changes. Port is raw because
// null means something: back to the target's own port.
type HealthCheckInput struct {
	Protocol           *string         `json:"protocol,omitempty"`
	Port               json.RawMessage `json:"port,omitempty"`
	Path               *string         `json:"path,omitempty"`
	IntervalSeconds    *int            `json:"intervalSeconds,omitempty"`
	TimeoutSeconds     *int            `json:"timeoutSeconds,omitempty"`
	HealthyThreshold   *int            `json:"healthyThreshold,omitempty"`
	UnhealthyThreshold *int            `json:"unhealthyThreshold,omitempty"`
	Matcher            *string         `json:"matcher,omitempty"`
}

// TargetGroup is a target group. Slug (tgp_…) is what customers name it by. LoadBalancer is nil
// until a listener uses it.
type TargetGroup struct {
	ID                         string            `json:"id"`
	Slug                       string            `json:"slug"`
	ResourceName               string            `json:"resourceName"`
	OrganizationID             string            `json:"organizationId"`
	ProjectID                  string            `json:"projectId"`
	Region                     string            `json:"region"`
	Name                       string            `json:"name"`
	Description                *string           `json:"description"`
	Tags                       map[string]string `json:"tags"`
	VPC                        string            `json:"vpc"`
	Status                     string            `json:"status"`
	LoadBalancer               *string           `json:"loadBalancer"`
	Protocol                   string            `json:"protocol"`
	Port                       int               `json:"port"`
	DeregistrationDelaySeconds int               `json:"deregistrationDelaySeconds"`
	ProxyProtocol              string            `json:"proxyProtocol"`
	HealthCheck                HealthCheck       `json:"healthCheck"`
	CreatedAt                  string            `json:"createdAt"`
	UpdatedAt                  string            `json:"updatedAt"`
}

// TargetGroupCreate is a create's body.
type TargetGroupCreate struct {
	Name                       string            `json:"name"`
	Description                *string           `json:"description,omitempty"`
	Tags                       map[string]string `json:"tags,omitempty"`
	VPC                        string            `json:"vpc"`
	Protocol                   *string           `json:"protocol,omitempty"`
	Port                       int               `json:"port"`
	DeregistrationDelaySeconds *int              `json:"deregistrationDelaySeconds,omitempty"`
	ProxyProtocol              *string           `json:"proxyProtocol,omitempty"`
	HealthCheck                *HealthCheckInput `json:"healthCheck,omitempty"`
}

// TargetGroupUpdate is an update's body: only what's set changes.
type TargetGroupUpdate struct {
	Name                       *string            `json:"name,omitempty"`
	Description                *string            `json:"description,omitempty"`
	Tags                       *map[string]string `json:"tags,omitempty"`
	Port                       *int               `json:"port,omitempty"`
	DeregistrationDelaySeconds *int               `json:"deregistrationDelaySeconds,omitempty"`
	ProxyProtocol              *string            `json:"proxyProtocol,omitempty"`
	HealthCheck                *HealthCheckInput  `json:"healthCheck,omitempty"`
}

// Target is a record of its target group, named by its ID, a UUID: a private interface or address. State is initial,
// healthy, unhealthy, unused or draining; Reason says why, or is nil. Port nil is the group's.
type Target struct {
	ID               string  `json:"id"`
	TargetGroup      string  `json:"targetGroup"`
	NetworkInterface *string `json:"networkInterface"`
	PrivateIPv4      string  `json:"privateIpv4"`
	Port             *int    `json:"port"`
	Instance         *string `json:"instance"`
	State            string  `json:"state"`
	Reason           *string `json:"reason"`
	CreatedAt        string  `json:"createdAt"`
	UpdatedAt        string  `json:"updatedAt"`
}

// TargetCreate is a register's body: NetworkInterface (eni_…) or PrivateIPv4.
type TargetCreate struct {
	NetworkInterface *string `json:"networkInterface,omitempty"`
	PrivateIPv4      *string `json:"privateIpv4,omitempty"`
	Port             *int    `json:"port,omitempty"`
}

type loadBalancerPage struct {
	LoadBalancers []LoadBalancer `json:"loadBalancers"`
	NextCursor    *string        `json:"nextCursor"`
}

type listenerPage struct {
	Listeners  []Listener `json:"listeners"`
	NextCursor *string    `json:"nextCursor"`
}

type targetGroupPage struct {
	TargetGroups []TargetGroup `json:"targetGroups"`
	NextCursor   *string       `json:"nextCursor"`
}

type targetPage struct {
	Targets    []Target `json:"targets"`
	NextCursor *string  `json:"nextCursor"`
}

type operationPage struct {
	Operations []Operation `json:"operations"`
	NextCursor *string     `json:"nextCursor"`
}

var p = apiclient.Path

func projectPath(orgID, projectID string) string {
	return p("/v1/organizations/{}/projects/{}", orgID, projectID)
}

func lbPath(orgID, projectID, lb string) string {
	return projectPath(orgID, projectID) + p("/load-balancers/{}", lb)
}

func groupPath(orgID, projectID, group string) string {
	return projectPath(orgID, projectID) + p("/target-groups/{}", group)
}

func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

// Every write but an operation's cancel declares Idempotency-Key.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Query: q, Body: body, Create: method != http.MethodGet}, out)
	return err
}

// ListLoadBalancers reads a page of the project's load balancers, newest first.
func (c *Client) ListLoadBalancers(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[LoadBalancer], error) {
	var out loadBalancerPage
	err := c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+"/load-balancers", pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[LoadBalancer]{Items: out.LoadBalancers, NextCursor: out.NextCursor}, err
}

// GetLoadBalancer reads a load balancer.
func (c *Client) GetLoadBalancer(ctx context.Context, orgID, projectID, lb string) (*LoadBalancer, error) {
	var out LoadBalancer
	return &out, c.do(ctx, http.MethodGet, lbPath(orgID, projectID, lb), nil, nil, &out)
}

// CreateLoadBalancer starts creating one, and answers the operation.
func (c *Client) CreateLoadBalancer(ctx context.Context, orgID, projectID string, body LoadBalancerCreate) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodPost, projectPath(orgID, projectID)+"/load-balancers", nil, body, &out)
}

// UpdateLoadBalancer changes a load balancer.
func (c *Client) UpdateLoadBalancer(ctx context.Context, orgID, projectID, lb string, body LoadBalancerUpdate) (*LoadBalancer, error) {
	var out LoadBalancer
	return &out, c.do(ctx, http.MethodPatch, lbPath(orgID, projectID, lb), nil, body, &out)
}

// DeleteLoadBalancer starts deleting one, and answers the operation (409 load_balancer_referenced
// while a firewall rule names it).
func (c *Client) DeleteLoadBalancer(ctx context.Context, orgID, projectID, lb string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodDelete, lbPath(orgID, projectID, lb), nil, nil, &out)
}

// ListListeners reads a page of a load balancer's listeners.
func (c *Client) ListListeners(ctx context.Context, orgID, projectID, lb string, limit int, cursor string) (apiclient.Page[Listener], error) {
	var out listenerPage
	err := c.do(ctx, http.MethodGet, lbPath(orgID, projectID, lb)+"/listeners", pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[Listener]{Items: out.Listeners, NextCursor: out.NextCursor}, err
}

// GetListener reads a listener.
func (c *Client) GetListener(ctx context.Context, orgID, projectID, lb, listener string) (*Listener, error) {
	var out Listener
	return &out, c.do(ctx, http.MethodGet, lbPath(orgID, projectID, lb)+p("/listeners/{}", listener), nil, nil, &out)
}

// CreateListener adds a listener.
func (c *Client) CreateListener(ctx context.Context, orgID, projectID, lb string, body ListenerCreate) (*Listener, error) {
	var out Listener
	return &out, c.do(ctx, http.MethodPost, lbPath(orgID, projectID, lb)+"/listeners", nil, body, &out)
}

// UpdateListener changes a listener.
func (c *Client) UpdateListener(ctx context.Context, orgID, projectID, lb, listener string, body ListenerUpdate) (*Listener, error) {
	var out Listener
	return &out, c.do(ctx, http.MethodPatch, lbPath(orgID, projectID, lb)+p("/listeners/{}", listener), nil, body, &out)
}

// DeleteListener removes a listener.
func (c *Client) DeleteListener(ctx context.Context, orgID, projectID, lb, listener string) error {
	return c.do(ctx, http.MethodDelete, lbPath(orgID, projectID, lb)+p("/listeners/{}", listener), nil, nil, nil)
}

// ListTargetGroups reads a page of the project's target groups, newest first.
func (c *Client) ListTargetGroups(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[TargetGroup], error) {
	var out targetGroupPage
	err := c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+"/target-groups", pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[TargetGroup]{Items: out.TargetGroups, NextCursor: out.NextCursor}, err
}

// GetTargetGroup reads a target group.
func (c *Client) GetTargetGroup(ctx context.Context, orgID, projectID, group string) (*TargetGroup, error) {
	var out TargetGroup
	return &out, c.do(ctx, http.MethodGet, groupPath(orgID, projectID, group), nil, nil, &out)
}

// CreateTargetGroup creates a target group.
func (c *Client) CreateTargetGroup(ctx context.Context, orgID, projectID string, body TargetGroupCreate) (*TargetGroup, error) {
	var out TargetGroup
	return &out, c.do(ctx, http.MethodPost, projectPath(orgID, projectID)+"/target-groups", nil, body, &out)
}

// UpdateTargetGroup changes a target group.
func (c *Client) UpdateTargetGroup(ctx context.Context, orgID, projectID, group string, body TargetGroupUpdate) (*TargetGroup, error) {
	var out TargetGroup
	return &out, c.do(ctx, http.MethodPatch, groupPath(orgID, projectID, group), nil, body, &out)
}

// DeleteTargetGroup deletes a target group (409 target_group_in_use while a listener uses it).
func (c *Client) DeleteTargetGroup(ctx context.Context, orgID, projectID, group string) error {
	return c.do(ctx, http.MethodDelete, groupPath(orgID, projectID, group), nil, nil, nil)
}

// ListTargets reads a page of a target group's targets.
func (c *Client) ListTargets(ctx context.Context, orgID, projectID, group string, limit int, cursor string) (apiclient.Page[Target], error) {
	var out targetPage
	err := c.do(ctx, http.MethodGet, groupPath(orgID, projectID, group)+"/targets", pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[Target]{Items: out.Targets, NextCursor: out.NextCursor}, err
}

// RegisterTarget registers a target.
func (c *Client) RegisterTarget(ctx context.Context, orgID, projectID, group string, body TargetCreate) (*Target, error) {
	var out Target
	return &out, c.do(ctx, http.MethodPost, groupPath(orgID, projectID, group)+"/targets", nil, body, &out)
}

// DeregisterTarget starts a target draining. It goes when the group's deregistration delay ends.
func (c *Client) DeregisterTarget(ctx context.Context, orgID, projectID, group, target string) (*Target, error) {
	var out Target
	return &out, c.do(ctx, http.MethodDelete, groupPath(orgID, projectID, group)+p("/targets/{}", target), nil, nil, &out)
}

// CancelOperation asks an operation to stop (409 operation_finished or operation_not_cancellable).
func (c *Client) CancelOperation(ctx context.Context, orgID, id string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: p("/v1/organizations/{}/operations/{}", orgID, id)}, &out)
	return &out, err
}

// ListOperations reads a page of the organization's load balancer operations, newest first.
func (c *Client) ListOperations(ctx context.Context, orgID string, limit int, cursor string) (apiclient.Page[Operation], error) {
	var out operationPage
	err := c.do(ctx, http.MethodGet, p("/v1/organizations/{}/operations", orgID), pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[Operation]{Items: out.Operations, NextCursor: out.NextCursor}, err
}

// GetOperation reads an operation.
func (c *Client) GetOperation(ctx context.Context, orgID, id string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodGet, p("/v1/organizations/{}/operations/{}", orgID, id), nil, nil, &out)
}
