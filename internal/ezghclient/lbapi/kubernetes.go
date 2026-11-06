package lbapi

import (
	"context"
	"net/http"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// What the L7 controllers need beyond the CLI's client: http and https listeners
// with header modifiers (lb-stack P5, K11), read with their ETag and changed with If-Match.
// The types are lb-api's answers, every field present, so a desired listener built from them
// compares equal to the one read back when nothing has to change.

// L7Listener is an http or https listener as lb-api answers it.
type L7Listener struct {
	ID                     string   `json:"id"`
	Protocol               string   `json:"protocol"`
	Port                   int      `json:"port"`
	DefaultAction          L7Action `json:"defaultAction"`
	Rules                  []L7Rule `json:"rules"`
	Certificate            *string  `json:"certificate"`
	AdditionalCertificates []string `json:"additionalCertificates"`
}

// L7Rule is one ordered rule.
type L7Rule struct {
	Name   *string  `json:"name"`
	Match  Match    `json:"match"`
	Action L7Action `json:"action"`
}

// L7Action is forward, redirect or fixedResponse, with that one set and the others null.
type L7Action struct {
	Type          string           `json:"type"`
	Forward       *L7Forward       `json:"forward"`
	Redirect      *L7Redirect      `json:"redirect"`
	FixedResponse *L7FixedResponse `json:"fixedResponse"`
}

// L7Forward is a forward with P5's header modifiers. A nil modifier isn't sent at all, so a
// listener without them works while lb-api's LB_HEADER_MODIFIERS is off; lb-api answers null.
type L7Forward struct {
	TargetGroups           []L7WeightedGroup `json:"targetGroups"`
	RequestHeaderModifier  *HeaderModifier   `json:"requestHeaderModifier,omitempty"`
	ResponseHeaderModifier *HeaderModifier   `json:"responseHeaderModifier,omitempty"`
}

// L7WeightedGroup is a forward's group, always with its weight (lb-api answers one).
type L7WeightedGroup struct {
	TargetGroup string `json:"targetGroup"`
	Weight      int    `json:"weight"`
}

// HeaderModifier is Gateway API's: each list at most 16 entries, every field present.
type HeaderModifier struct {
	Set    []NameValue `json:"set"`
	Add    []NameValue `json:"add"`
	Remove []string    `json:"remove"`
}

// L7Redirect replaces each part it sets; StatusCode is 301, 302, 307 or 308.
type L7Redirect struct {
	Scheme     *string `json:"scheme"`
	Host       *string `json:"host"`
	Port       *int    `json:"port"`
	Path       *string `json:"path"`
	StatusCode int     `json:"statusCode"`
}

// L7FixedResponse answers itself.
type L7FixedResponse struct {
	StatusCode  int    `json:"statusCode"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
}

// L7ListenerCreate is an http or https listener's create body.
type L7ListenerCreate struct {
	Protocol               string   `json:"protocol"`
	Port                   int      `json:"port"`
	DefaultAction          L7Action `json:"defaultAction"`
	Rules                  []L7Rule `json:"rules"`
	Certificate            *string  `json:"certificate,omitempty"`
	AdditionalCertificates []string `json:"additionalCertificates,omitempty"`
}

// L7ListenerUpdate replaces the default action and every rule; Protocol and the certificates
// change only when set.
type L7ListenerUpdate struct {
	Protocol               *string   `json:"protocol,omitempty"`
	DefaultAction          L7Action  `json:"defaultAction"`
	Rules                  []L7Rule  `json:"rules"`
	Certificate            *string   `json:"certificate,omitempty"`
	AdditionalCertificates *[]string `json:"additionalCertificates,omitempty"`
}

// GetL7Listener reads a listener and its ETag.
func (c *Client) GetL7Listener(ctx context.Context, orgID, projectID, lb, listener string) (*L7Listener, string, error) {
	var out L7Listener
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: lbPath(orgID, projectID, lb) + p("/listeners/{}", listener)}, &out)
	if err != nil {
		return nil, "", err
	}
	return &out, resp.Header.Get("ETag"), nil
}

// CreateL7Listener adds an http or https listener.
func (c *Client) CreateL7Listener(ctx context.Context, orgID, projectID, lb string, body L7ListenerCreate) (*L7Listener, error) {
	var out L7Listener
	return &out, c.do(ctx, http.MethodPost, lbPath(orgID, projectID, lb)+"/listeners", nil, body, &out)
}

// UpdateL7Listener changes a listener if it's still at ifMatch (412 precondition_failed if not).
func (c *Client) UpdateL7Listener(ctx context.Context, orgID, projectID, lb, listener string, body L7ListenerUpdate, ifMatch string) (*L7Listener, error) {
	var out L7Listener
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPatch, Path: lbPath(orgID, projectID, lb) + p("/listeners/{}", listener), Body: body, Create: true, IfMatch: ifMatch}, &out)
	return &out, err
}
