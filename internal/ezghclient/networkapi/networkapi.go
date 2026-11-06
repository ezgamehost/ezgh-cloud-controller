// Package networkapi is a typed client for network-api: a project's network interfaces (each
// with its own routed public IPv6 /64), public IPv4 addresses, VPCs and their subnets, and the
// operations that move secondary private addresses between interfaces.
//
// Every write declares an Idempotency-Key, so each carries one, the same on every attempt.
package networkapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// Client calls network-api.
type Client struct {
	API *apiclient.Client
}

// The shapes of network-api (api/openapi/network-api.json), in the API's order.

// NetworkInterface is a network interface. Slug is its ID (eni_…), which bodies and filters take;
// ID is its UUID.
type NetworkInterface struct {
	ID             string               `json:"id"`
	Slug           string               `json:"slug"`
	Name           string               `json:"name"`
	Description    *string              `json:"description"`
	ResourceName   string               `json:"resourceName"`
	OrganizationID string               `json:"organizationId"`
	ProjectID      string               `json:"projectId"`
	Region         string               `json:"region"`
	Tags           map[string]string    `json:"tags"`
	Status         string               `json:"status"`
	MACAddress     string               `json:"macAddress"`
	MTU            int                  `json:"mtu"`
	IPv6Prefix     *string              `json:"ipv6Prefix"`
	IPv6Address    *string              `json:"ipv6Address"`
	IPv4Address    *InterfaceIPv4       `json:"ipv4Address"`
	Attachment     *InterfaceAttachment `json:"attachment"`
	CreatedAt      string               `json:"createdAt"`
	UpdatedAt      string               `json:"updatedAt"`
	// A private interface's VPC (vpc_…), subnet (sbn_…) and primary private IPv4 address; null
	// on a public one.
	VPC                *string `json:"vpc"`
	Subnet             *string `json:"subnet"`
	PrivateIPv4Address *string `json:"privateIpv4Address"`
	// Realized secondary private addresses; [] on a public interface.
	SecondaryPrivateIPv4Addresses []string `json:"secondaryPrivateIpv4Addresses"`
	SecondaryPrivateIPv6Addresses []string `json:"secondaryPrivateIpv6Addresses"`
	// PrivateAddressAssignments is every realized private address with its generation, the
	// compare-and-swap value unassign and reassign send; [] on a public interface.
	PrivateAddressAssignments []PrivateAddressAssignment `json:"privateAddressAssignments"`
	// PrivateAddressAssignments' prefix counterpart: delegated /28 (IPv4) and /80 (IPv6) prefixes,
	// [] on a public interface; absent from a network-api that predates them.
	PrivatePrefixAssignments []PrivatePrefixAssignment `json:"privatePrefixAssignments"`
	// PublicIPv6Address is <ipv6Prefix>::1. On a private interface, IPv4Address is the public IPv4
	// mapped onto its primary private one and IPv6Prefix its public /64 (Network Gateways).
	PublicIPv6Address *string `json:"publicIpv6Address"`
	// SourceDestCheck is on (true) unless a private interface turned it off to forward traffic (a
	// NAT, VPN or firewall instance); nil from a network-api before route tables.
	SourceDestCheck *bool `json:"sourceDestCheck,omitempty"`
}

// PrivateAddressAssignment is one realized private address of an interface. Generation is a
// decimal string, kept as it is: it's compared, never computed with.
type PrivateAddressAssignment struct {
	Family     string `json:"family"`
	Address    string `json:"address"`
	Primary    bool   `json:"primary"`
	Generation string `json:"generation"`
}

// PrivatePrefixAssignment is one realized delegated prefix (CIDR form) of an interface.
type PrivatePrefixAssignment struct {
	Family     string `json:"family"`
	Prefix     string `json:"prefix"`
	Generation string `json:"generation"`
}

// InterfaceIPv4 is the public IPv4 address associated with an interface.
type InterfaceIPv4 struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Address string `json:"address"`
}

// InterfaceAttachment is the instance an interface is attached to.
type InterfaceAttachment struct {
	Instance            string `json:"instance"`
	DeviceIndex         int    `json:"deviceIndex"`
	Primary             bool   `json:"primary"`
	DeleteOnTermination bool   `json:"deleteOnTermination"`
	AttachedAt          string `json:"attachedAt"`
}

// NetworkInterfaceCreate is a new interface. Firewalls nil is the project's default firewall (a
// private interface's VPC's default firewall), an empty list none. Subnet (sbn_…) makes it
// private, at PrivateIPv4Address when that's set; IPv6 (with Subnet only) asks for a public /64.
type NetworkInterfaceCreate struct {
	Name               string            `json:"name"`
	Description        *string           `json:"description,omitempty"`
	Firewalls          *[]string         `json:"firewalls,omitempty"`
	IPv4               *InterfaceIPv4Ask `json:"ipv4,omitempty"`
	IPv6               *InterfaceIPv6Ask `json:"ipv6,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Subnet             string            `json:"subnet,omitempty"`
	PrivateIPv4Address string            `json:"privateIpv4Address,omitempty"`
}

// InterfaceIPv4Ask gives a new interface a public IPv4 address: a new one (Allocate) or one the
// project has, by its pip_ ID.
type InterfaceIPv4Ask struct {
	// Allocate is a new address; on a private interface it may be false (none, whatever the
	// subnet's auto-assign says).
	Allocate *bool  `json:"allocate,omitempty"`
	Address  string `json:"address,omitempty"`
}

// NetworkInterfaceUpdate renames, describes or retags an interface (Tags replace its tags), or
// turns a private interface's source/destination check off or on.
type NetworkInterfaceUpdate struct {
	Name            *string            `json:"name,omitempty"`
	Description     *string            `json:"description,omitempty"`
	Tags            *map[string]string `json:"tags,omitempty"`
	SourceDestCheck *bool              `json:"sourceDestCheck,omitempty"`
}

// IPv4Address is a public IPv4 address. Slug is its ID (pip_…).
type IPv4Address struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Tags           map[string]string `json:"tags"`
	Status         string            `json:"status"`
	Address        string            `json:"address"`
	AutoRelease    bool              `json:"autoRelease"`
	Association    *Association      `json:"association"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
}

// Association is the interface an address is associated with, and the instance it's on.
// PrivateIPv4Address is the private address it's mapped onto when the interface is private.
type Association struct {
	NetworkInterface   string  `json:"networkInterface"`
	Instance           *string `json:"instance"`
	State              string  `json:"state"`
	PrivateIPv4Address *string `json:"privateIpv4Address"`
}

// InterfaceIPv6Ask asks for a private interface's public IPv6 /64.
type InterfaceIPv6Ask struct {
	Allocate bool `json:"allocate"`
}

// IPv4AddressCreate allocates an address.
type IPv4AddressCreate struct {
	Name string            `json:"name"`
	Tags map[string]string `json:"tags,omitempty"`
}

// IPv4AddressUpdate renames or retags an address, or sets whether it's released with its
// interface.
type IPv4AddressUpdate struct {
	Name        *string            `json:"name,omitempty"`
	AutoRelease *bool              `json:"autoRelease,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
}

// IPv4AddressAssociate associates an address with an interface, by its eni_ ID.
type IPv4AddressAssociate struct {
	NetworkInterface   string `json:"networkInterface"`
	AllowReassociation bool   `json:"allowReassociation,omitempty"`
}

// VPC is a private regional network. Slug is its ID (vpc_…). Its MTU is always 8900.
type VPC struct {
	ID              string            `json:"id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Description     *string           `json:"description"`
	ResourceName    string            `json:"resourceName"`
	OrganizationID  string            `json:"organizationId"`
	ProjectID       string            `json:"projectId"`
	Region          string            `json:"region"`
	Tags            map[string]string `json:"tags"`
	Status          string            `json:"status"`
	IPv4CIDR        string            `json:"ipv4Cidr"`
	IPv6CIDR        string            `json:"ipv6Cidr"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
	MTU             int               `json:"mtu"`
	DefaultFirewall *string           `json:"defaultFirewall"`
	FailureReason   *string           `json:"failureReason"`
}

// VPCCreate is a new VPC; there's no MTU to choose.
type VPCCreate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	IPv4CIDR    string            `json:"ipv4Cidr"`
}

// VPCUpdate renames, describes or retags a VPC or a subnet (Tags replace its tags).
type VPCUpdate struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
}

// SubnetUpdate renames, describes or retags a subnet, or sets its launch defaults for public
// addresses (Network Gateways).
type SubnetUpdate struct {
	Name                 *string            `json:"name,omitempty"`
	Description          *string            `json:"description,omitempty"`
	Tags                 *map[string]string `json:"tags,omitempty"`
	AutoAssignPublicIPv4 *bool              `json:"autoAssignPublicIpv4,omitempty"`
	AutoAssignPublicIPv6 *bool              `json:"autoAssignPublicIpv6,omitempty"`
	// RouteTable is the table's ID (rtb_…) as JSON, or null for the VPC's main table (a *string
	// with omitempty can't send null).
	RouteTable json.RawMessage `json:"routeTable,omitempty"`
}

// Subnet is a range of a VPC. Slug is its ID (sbn_…).
type Subnet struct {
	ID                     string            `json:"id"`
	Slug                   string            `json:"slug"`
	Name                   string            `json:"name"`
	Description            *string           `json:"description"`
	ResourceName           string            `json:"resourceName"`
	OrganizationID         string            `json:"organizationId"`
	ProjectID              string            `json:"projectId"`
	Region                 string            `json:"region"`
	Tags                   map[string]string `json:"tags"`
	Status                 string            `json:"status"`
	IPv4CIDR               string            `json:"ipv4Cidr"`
	IPv6CIDR               string            `json:"ipv6Cidr"`
	CreatedAt              string            `json:"createdAt"`
	UpdatedAt              string            `json:"updatedAt"`
	VPC                    string            `json:"vpc"`
	AvailableIPv4Addresses int               `json:"availableIpv4Addresses"`
	// Launch defaults: a new interface in the subnet without ipv4/ipv6 takes a public IPv4 or /64.
	AutoAssignPublicIPv4 bool `json:"autoAssignPublicIpv4"`
	AutoAssignPublicIPv6 bool `json:"autoAssignPublicIpv6"`
	// RouteTable is the subnet's effective route table (rtb_…): its own, or the VPC's main one;
	// nil from a network-api before route tables.
	RouteTable *string `json:"routeTable,omitempty"`
}

// NetworkGateway is a VPC's internet path. Slug is its ID (ngw_…); VPC is the VPC it's attached
// to (vpc_…), nil while detached.
type NetworkGateway struct {
	ID              string            `json:"id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Description     *string           `json:"description"`
	ResourceName    string            `json:"resourceName"`
	OrganizationID  string            `json:"organizationId"`
	ProjectID       string            `json:"projectId"`
	Region          string            `json:"region"`
	Tags            map[string]string `json:"tags"`
	Status          string            `json:"status"`
	VPC             *string           `json:"vpc"`
	AttachmentState string            `json:"attachmentState"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
}

// NetworkGatewayCreate is a new, detached gateway.
type NetworkGatewayCreate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
}

// NetworkGatewayUpdate renames, describes or retags a gateway (Tags replace its tags).
type NetworkGatewayUpdate struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
}

// NetworkGatewayAttach attaches a gateway to a VPC (vpc_…) of its project.
type NetworkGatewayAttach struct {
	VPC string `json:"vpc"`
}

// SubnetCreate is a new subnet of a VPC (vpc_…).
type SubnetCreate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	VPC         string            `json:"vpc"`
	IPv4CIDR    string            `json:"ipv4Cidr"`
}

// PrivateAddressAssign gives an interface a secondary address of its subnet: Address, or the
// next eligible one when it's empty.
type PrivateAddressAssign struct {
	Family  string `json:"family"`
	Address string `json:"address,omitempty"`
}

// PrivateAddressUnassign takes a secondary address from the interface; ExpectedGeneration is its
// generation (PrivateAddressAssignments), as the API gave it.
type PrivateAddressUnassign struct {
	Family             string `json:"family"`
	Address            string `json:"address"`
	ExpectedGeneration string `json:"expectedGeneration"`
}

// PrivatePrefixAssign delegates a prefix of the interface's subnet: Prefix, or one picked when empty.
type PrivatePrefixAssign struct {
	Family string `json:"family"`
	Prefix string `json:"prefix,omitempty"`
}

// PrivatePrefixUnassign takes a prefix back; ExpectedGeneration is from PrivatePrefixAssignments.
type PrivatePrefixUnassign struct {
	Family             string `json:"family"`
	Prefix             string `json:"prefix"`
	ExpectedGeneration string `json:"expectedGeneration"`
}

// PrivateAddressReassign moves a secondary address to another private interface (eni_…) of the
// same subnet.
type PrivateAddressReassign struct {
	Family               string `json:"family"`
	Address              string `json:"address"`
	ExpectedGeneration   string `json:"expectedGeneration"`
	DestinationInterface string `json:"destinationInterface"`
}

// PrivateAddressOperation is an assign, unassign or reassign. It has succeeded only once Status
// is succeeded: the old owner's permission is withdrawn everywhere and the destination realized.
// A 202 accepting it, or a BlockedReason, says nothing about that.
type PrivateAddressOperation struct {
	ID                   string                 `json:"id"`
	Type                 string                 `json:"type"`
	Status               string                 `json:"status"`
	Phase                string                 `json:"phase"`
	Family               string                 `json:"family"`
	Address              string                 `json:"address"`
	SourceInterface      *string                `json:"sourceInterface"`
	DestinationInterface *string                `json:"destinationInterface"`
	ExpectedGeneration   *string                `json:"expectedGeneration"`
	Generation           string                 `json:"generation"`
	BlockedReason        *string                `json:"blockedReason"`
	CreatedAt            string                 `json:"createdAt"`
	UpdatedAt            string                 `json:"updatedAt"`
	FinishedAt           *string                `json:"finishedAt"`
	Error                *PrivateAddressFailure `json:"error"`
	// RetryAfter is the answer's Retry-After: when to read the operation again (0 once it's
	// finished).
	RetryAfter time.Duration `json:"-"`
}

// PrivateAddressFailure is a failed operation's error.
type PrivateAddressFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type vpcPage struct {
	VPCs       []VPC   `json:"vpcs"`
	NextCursor *string `json:"nextCursor"`
}

type networkGatewayPage struct {
	NetworkGateways []NetworkGateway `json:"networkGateways"`
	NextCursor      *string          `json:"nextCursor"`
}

type subnetPage struct {
	Subnets    []Subnet `json:"subnets"`
	NextCursor *string  `json:"nextCursor"`
}

type operationPage struct {
	Operations []PrivateAddressOperation `json:"operations"`
	NextCursor *string                   `json:"nextCursor"`
}

type interfacePage struct {
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces"`
	NextCursor        *string            `json:"nextCursor"`
}

type addressPage struct {
	IPv4Addresses []IPv4Address `json:"ipv4Addresses"`
	NextCursor    *string       `json:"nextCursor"`
}

var p = apiclient.Path

func pageQuery(limit int, cursor string, filters ...string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	for i := 0; i+1 < len(filters); i += 2 {
		if filters[i+1] != "" {
			q.Set(filters[i], filters[i+1])
		}
	}
	return q
}

func projectPath(orgID, projectID, rest string, values ...string) string {
	return p("/v1/organizations/{}/projects/{}", orgID, projectID) + p(rest, values...)
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: path, Query: query}, out)
	return err
}

// write sends a write with an Idempotency-Key, the same on every attempt.
func (c *Client) write(ctx context.Context, method, path string, body, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Body: body, Create: true}, out)
	return err
}

// ListNetworkInterfaces reads a page of the project's interfaces, newest first; instance (ins_…)
// and status, when set, filter it.
func (c *Client) ListNetworkInterfaces(ctx context.Context, orgID, projectID, instance, status string, limit int, cursor string) (apiclient.Page[NetworkInterface], error) {
	var out interfacePage
	err := c.get(ctx, projectPath(orgID, projectID, "/network-interfaces"), pageQuery(limit, cursor, "instance", instance, "status", status), &out)
	if out.NetworkInterfaces == nil {
		out.NetworkInterfaces = []NetworkInterface{}
	}
	return apiclient.Page[NetworkInterface]{Items: out.NetworkInterfaces, NextCursor: out.NextCursor}, err
}

// GetNetworkInterface reads an interface by ID (eni_…) or UUID.
func (c *Client) GetNetworkInterface(ctx context.Context, orgID, projectID, ni string) (*NetworkInterface, error) {
	var out NetworkInterface
	return &out, c.get(ctx, projectPath(orgID, projectID, "/network-interfaces/{}", ni), nil, &out)
}

// CreateNetworkInterface creates an interface with its /64, and its IPv4 address when asked.
func (c *Client) CreateNetworkInterface(ctx context.Context, orgID, projectID string, body NetworkInterfaceCreate) (*NetworkInterface, error) {
	var out NetworkInterface
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/network-interfaces"), body, &out)
}

// UpdateNetworkInterface renames, describes or retags an interface.
func (c *Client) UpdateNetworkInterface(ctx context.Context, orgID, projectID, ni string, body NetworkInterfaceUpdate) (*NetworkInterface, error) {
	var out NetworkInterface
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/network-interfaces/{}", ni), body, &out)
}

// DeleteNetworkInterface deletes an interface (409 interface_in_use while attached).
func (c *Client) DeleteNetworkInterface(ctx context.Context, orgID, projectID, ni string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/network-interfaces/{}", ni), nil, nil)
}

// ListIPv4Addresses reads a page of the project's addresses, newest first; networkInterface
// (eni_…), when set, filters it.
func (c *Client) ListIPv4Addresses(ctx context.Context, orgID, projectID, networkInterface string, limit int, cursor string) (apiclient.Page[IPv4Address], error) {
	var out addressPage
	err := c.get(ctx, projectPath(orgID, projectID, "/ipv4-addresses"), pageQuery(limit, cursor, "networkInterface", networkInterface), &out)
	if out.IPv4Addresses == nil {
		out.IPv4Addresses = []IPv4Address{}
	}
	return apiclient.Page[IPv4Address]{Items: out.IPv4Addresses, NextCursor: out.NextCursor}, err
}

// GetIPv4Address reads an address by ID (pip_…) or UUID.
func (c *Client) GetIPv4Address(ctx context.Context, orgID, projectID, address string) (*IPv4Address, error) {
	var out IPv4Address
	return &out, c.get(ctx, projectPath(orgID, projectID, "/ipv4-addresses/{}", address), nil, &out)
}

// AllocateIPv4Address allocates a public IPv4 address, metered while it's allocated.
func (c *Client) AllocateIPv4Address(ctx context.Context, orgID, projectID string, body IPv4AddressCreate) (*IPv4Address, error) {
	var out IPv4Address
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/ipv4-addresses"), body, &out)
}

// UpdateIPv4Address renames or retags an address, or sets its autoRelease.
func (c *Client) UpdateIPv4Address(ctx context.Context, orgID, projectID, address string, body IPv4AddressUpdate) (*IPv4Address, error) {
	var out IPv4Address
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/ipv4-addresses/{}", address), body, &out)
}

// AssociateIPv4Address associates an address with an interface.
func (c *Client) AssociateIPv4Address(ctx context.Context, orgID, projectID, address string, body IPv4AddressAssociate) (*IPv4Address, error) {
	var out IPv4Address
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/ipv4-addresses/{}/associate", address), body, &out)
}

// DisassociateIPv4Address disassociates an address from its interface.
func (c *Client) DisassociateIPv4Address(ctx context.Context, orgID, projectID, address string) (*IPv4Address, error) {
	var out IPv4Address
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/ipv4-addresses/{}/disassociate", address), struct{}{}, &out)
}

// ReleaseIPv4Address releases an address (409 address_associated until it's disassociated).
func (c *Client) ReleaseIPv4Address(ctx context.Context, orgID, projectID, address string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/ipv4-addresses/{}", address), nil, nil)
}

// ListVPCs reads a page of the project's VPCs.
func (c *Client) ListVPCs(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[VPC], error) {
	var out vpcPage
	err := c.get(ctx, projectPath(orgID, projectID, "/vpcs"), pageQuery(limit, cursor), &out)
	if out.VPCs == nil {
		out.VPCs = []VPC{}
	}
	return apiclient.Page[VPC]{Items: out.VPCs, NextCursor: out.NextCursor}, err
}

// GetVPC reads a VPC by ID (vpc_…) or UUID.
func (c *Client) GetVPC(ctx context.Context, orgID, projectID, vpc string) (*VPC, error) {
	var out VPC
	return &out, c.get(ctx, projectPath(orgID, projectID, "/vpcs/{}", vpc), nil, &out)
}

// CreateVPC creates a VPC with its generated IPv6 /56 and its default firewall.
func (c *Client) CreateVPC(ctx context.Context, orgID, projectID string, body VPCCreate) (*VPC, error) {
	var out VPC
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/vpcs"), body, &out)
}

// UpdateVPC renames, describes or retags a VPC.
func (c *Client) UpdateVPC(ctx context.Context, orgID, projectID, vpc string, body VPCUpdate) (*VPC, error) {
	var out VPC
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/vpcs/{}", vpc), body, &out)
}

// DeleteVPC deletes a VPC and its default firewall (409 vpc_in_use while it has subnets).
func (c *Client) DeleteVPC(ctx context.Context, orgID, projectID, vpc string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/vpcs/{}", vpc), nil, nil)
}

// ListSubnets reads a page of the project's subnets; vpc (vpc_…), when set, filters it.
func (c *Client) ListSubnets(ctx context.Context, orgID, projectID, vpc string, limit int, cursor string) (apiclient.Page[Subnet], error) {
	var out subnetPage
	err := c.get(ctx, projectPath(orgID, projectID, "/subnets"), pageQuery(limit, cursor, "vpc", vpc), &out)
	if out.Subnets == nil {
		out.Subnets = []Subnet{}
	}
	return apiclient.Page[Subnet]{Items: out.Subnets, NextCursor: out.NextCursor}, err
}

// GetSubnet reads a subnet by ID (sbn_…) or UUID.
func (c *Client) GetSubnet(ctx context.Context, orgID, projectID, subnet string) (*Subnet, error) {
	var out Subnet
	return &out, c.get(ctx, projectPath(orgID, projectID, "/subnets/{}", subnet), nil, &out)
}

// CreateSubnet creates a subnet of a VPC, with the VPC's next free IPv6 /64.
func (c *Client) CreateSubnet(ctx context.Context, orgID, projectID string, body SubnetCreate) (*Subnet, error) {
	var out Subnet
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/subnets"), body, &out)
}

// UpdateSubnet renames, describes or retags a subnet.
func (c *Client) UpdateSubnet(ctx context.Context, orgID, projectID, subnet string, body SubnetUpdate) (*Subnet, error) {
	var out Subnet
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/subnets/{}", subnet), body, &out)
}

// DeleteSubnet deletes a subnet (409 subnet_in_use while interfaces are in it).
func (c *Client) DeleteSubnet(ctx context.Context, orgID, projectID, subnet string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/subnets/{}", subnet), nil, nil)
}

// ListNetworkGateways lists the project's network gateways, newest first, or the one attached to
// vpc (vpc_…) when it's set.
func (c *Client) ListNetworkGateways(ctx context.Context, orgID, projectID, vpc string, limit int, cursor string) (apiclient.Page[NetworkGateway], error) {
	var out networkGatewayPage
	err := c.get(ctx, projectPath(orgID, projectID, "/network-gateways"), pageQuery(limit, cursor, "vpc", vpc), &out)
	if out.NetworkGateways == nil {
		out.NetworkGateways = []NetworkGateway{}
	}
	return apiclient.Page[NetworkGateway]{Items: out.NetworkGateways, NextCursor: out.NextCursor}, err
}

// GetNetworkGateway reads a gateway by ID (ngw_…) or UUID.
func (c *Client) GetNetworkGateway(ctx context.Context, orgID, projectID, gateway string) (*NetworkGateway, error) {
	var out NetworkGateway
	return &out, c.get(ctx, projectPath(orgID, projectID, "/network-gateways/{}", gateway), nil, &out)
}

// CreateNetworkGateway creates a detached gateway.
func (c *Client) CreateNetworkGateway(ctx context.Context, orgID, projectID string, body NetworkGatewayCreate) (*NetworkGateway, error) {
	var out NetworkGateway
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/network-gateways"), body, &out)
}

// UpdateNetworkGateway renames, describes or retags a gateway.
func (c *Client) UpdateNetworkGateway(ctx context.Context, orgID, projectID, gateway string, body NetworkGatewayUpdate) (*NetworkGateway, error) {
	var out NetworkGateway
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/network-gateways/{}", gateway), body, &out)
}

// AttachNetworkGateway attaches a gateway to a VPC (409 vpc_has_gateway: a VPC has at most one).
func (c *Client) AttachNetworkGateway(ctx context.Context, orgID, projectID, gateway, vpc string) (*NetworkGateway, error) {
	var out NetworkGateway
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/network-gateways/{}/attach", gateway), NetworkGatewayAttach{VPC: vpc}, &out)
}

// DetachNetworkGateway detaches a gateway from its VPC (409 gateway_in_use while a public IPv4 is
// mapped onto one of its interfaces or one holds a public /64).
func (c *Client) DetachNetworkGateway(ctx context.Context, orgID, projectID, gateway string) (*NetworkGateway, error) {
	var out NetworkGateway
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/network-gateways/{}/detach", gateway), struct{}{}, &out)
}

// DeleteNetworkGateway deletes a detached gateway (409 gateway_attached while attached).
func (c *Client) DeleteNetworkGateway(ctx context.Context, orgID, projectID, gateway string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/network-gateways/{}", gateway), nil, nil)
}

// RouteTable is a VPC's route table (rtb_…): the VPC's one main table or a custom one. Routes are
// the local routes first, then the rest; Subnets are those that use it (main's by default).
type RouteTable struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	Description    *string           `json:"description"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Tags           map[string]string `json:"tags"`
	VPC            string            `json:"vpc"`
	Main           bool              `json:"main"`
	Routes         []Route           `json:"routes"`
	Subnets        []string          `json:"subnets"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
}

// Route is one route of a table. Target.Type (local, networkGateway, networkInterface, later
// others) and State (active, blackhole) are open sets, shown as they come.
type Route struct {
	Destination string      `json:"destination"`
	Target      RouteTarget `json:"target"`
	State       string      `json:"state"`
}

// RouteTarget is where a route sends traffic; ID is nil for a local route.
type RouteTarget struct {
	Type string  `json:"type"`
	ID   *string `json:"id"`
}

// RouteTableCreate is a new custom table of a VPC (vpc_…), with only its local routes.
type RouteTableCreate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	VPC         string            `json:"vpc"`
}

// RouteTableUpdate renames, describes or retags a table (Tags replace its tags).
type RouteTableUpdate VPCUpdate

// RouteInput is a route to write: a destination CIDR and its target.
type RouteInput struct {
	Destination string           `json:"destination"`
	Target      RouteTargetInput `json:"target"`
}

// RouteTargetInput is a written route's target: networkGateway (ngw_…) or networkInterface (eni_…).
type RouteTargetInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// The routes writes' bodies.
type routesReplace struct {
	Routes []json.RawMessage `json:"routes"`
}
type routesSet struct {
	Routes []RouteInput `json:"routes"`
}
type routesRemove struct {
	Destinations []string `json:"destinations"`
}

type routeTablePage struct {
	RouteTables []RouteTable `json:"routeTables"`
	NextCursor  *string      `json:"nextCursor"`
}

// ListRouteTables lists the project's route tables, newest first, or vpc's (vpc_…) when it's set.
func (c *Client) ListRouteTables(ctx context.Context, orgID, projectID, vpc string, limit int, cursor string) (apiclient.Page[RouteTable], error) {
	var out routeTablePage
	err := c.get(ctx, projectPath(orgID, projectID, "/route-tables"), pageQuery(limit, cursor, "vpc", vpc), &out)
	if out.RouteTables == nil {
		out.RouteTables = []RouteTable{}
	}
	return apiclient.Page[RouteTable]{Items: out.RouteTables, NextCursor: out.NextCursor}, err
}

// GetRouteTable reads a table by ID (rtb_…) or UUID, with its ETag (the routes' revision).
func (c *Client) GetRouteTable(ctx context.Context, orgID, projectID, table string) (*RouteTable, string, error) {
	var out RouteTable
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/route-tables/{}", table)}, &out)
	if err != nil {
		return nil, "", err
	}
	return &out, resp.Header.Get("ETag"), nil
}

// CreateRouteTable creates a custom table of a VPC.
func (c *Client) CreateRouteTable(ctx context.Context, orgID, projectID string, body RouteTableCreate) (*RouteTable, error) {
	var out RouteTable
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/route-tables"), body, &out)
}

// UpdateRouteTable renames, describes or retags a table.
func (c *Client) UpdateRouteTable(ctx context.Context, orgID, projectID, table string, body RouteTableUpdate) (*RouteTable, error) {
	var out RouteTable
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/route-tables/{}", table), body, &out)
}

// DeleteRouteTable deletes a custom table (409 route_table_is_main, route_table_in_use).
func (c *Client) DeleteRouteTable(ctx context.Context, orgID, projectID, table string) error {
	return c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/route-tables/{}", table), nil, nil)
}

// ReplaceRoutes replaces a table's whole customer route set (PUT), only if its routes are still at
// ifMatch when that's set (412 precondition_failed otherwise). Local routes may be included as GET
// shows them.
func (c *Client) ReplaceRoutes(ctx context.Context, orgID, projectID, table string, routes []json.RawMessage, ifMatch string) (*RouteTable, error) {
	var out RouteTable
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPut, Path: projectPath(orgID, projectID, "/route-tables/{}/routes", table), Body: routesReplace{Routes: routes}, IfMatch: ifMatch, Create: true}, &out)
	return &out, err
}

// SetRoutes creates or replaces the route of each given destination (POST routes/set), only if
// the table's routes are still at ifMatch when that's set (412 precondition_failed otherwise): one
// Idempotency-Key per call, reused by its retries.
func (c *Client) SetRoutes(ctx context.Context, orgID, projectID, table string, routes []RouteInput, ifMatch string) (*RouteTable, error) {
	var out RouteTable
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/route-tables/{}/routes/set", table), Body: routesSet{Routes: routes}, IfMatch: ifMatch, Create: true}, &out)
	return &out, err
}

// RemoveRoutes deletes the given destinations' routes (POST routes/remove); an absent one is
// ignored, so a retry is safe.
func (c *Client) RemoveRoutes(ctx context.Context, orgID, projectID, table string, destinations []string) (*RouteTable, error) {
	var out RouteTable
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/route-tables/{}/routes/remove", table), routesRemove{Destinations: destinations}, &out)
}

// operation sends a request whose answer is an operation, keeping the answer's Retry-After.
func (c *Client) operation(ctx context.Context, req apiclient.Request) (*PrivateAddressOperation, error) {
	var out PrivateAddressOperation
	resp, err := c.API.Do(ctx, req, &out)
	if err != nil {
		return nil, err
	}
	out.RetryAfter = apiclient.RetryAfter(resp.Header.Get("Retry-After"))
	return &out, nil
}

// addressWrite starts an assign, unassign or reassign. Each carries an Idempotency-Key, the same
// on every attempt, so a retry after a lost answer gets the same operation back, never a second.
func (c *Client) addressWrite(ctx context.Context, orgID, projectID, ni, action string, body any) (*PrivateAddressOperation, error) {
	return c.operation(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/network-interfaces/{}/private-addresses/"+action, ni), Body: body, Create: true})
}

// AssignPrivateAddress starts giving an interface a secondary private address: a 202 with the
// operation, which is done only when it has succeeded.
func (c *Client) AssignPrivateAddress(ctx context.Context, orgID, projectID, ni string, body PrivateAddressAssign) (*PrivateAddressOperation, error) {
	return c.addressWrite(ctx, orgID, projectID, ni, "assign", body)
}

// UnassignPrivateAddress starts taking a secondary private address from an interface.
func (c *Client) UnassignPrivateAddress(ctx context.Context, orgID, projectID, ni string, body PrivateAddressUnassign) (*PrivateAddressOperation, error) {
	return c.addressWrite(ctx, orgID, projectID, ni, "unassign", body)
}

// ReassignPrivateAddress starts moving a secondary private address from an interface to another.
func (c *Client) ReassignPrivateAddress(ctx context.Context, orgID, projectID, ni string, body PrivateAddressReassign) (*PrivateAddressOperation, error) {
	return c.addressWrite(ctx, orgID, projectID, ni, "reassign", body)
}

// AssignPrivatePrefix starts delegating a prefix to an interface; the operation is a
// PrivateAddressOperation whose Address holds the prefix.
func (c *Client) AssignPrivatePrefix(ctx context.Context, orgID, projectID, ni string, body PrivatePrefixAssign) (*PrivateAddressOperation, error) {
	return c.operation(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/network-interfaces/{}/private-prefixes/assign", ni), Body: body, Create: true})
}

// UnassignPrivatePrefix starts taking a delegated prefix from an interface.
func (c *Client) UnassignPrivatePrefix(ctx context.Context, orgID, projectID, ni string, body PrivatePrefixUnassign) (*PrivateAddressOperation, error) {
	return c.operation(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/network-interfaces/{}/private-prefixes/unassign", ni), Body: body, Create: true})
}

// GetPrivateAddressOperation reads an operation by its UUID.
func (c *Client) GetPrivateAddressOperation(ctx context.Context, orgID, projectID, operationID string) (*PrivateAddressOperation, error) {
	return c.operation(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/private-address-operations/{}", operationID)})
}

// ListPrivateAddressOperations reads a page of the project's address operations; networkInterface
// (eni_…), when set, keeps those involving it.
func (c *Client) ListPrivateAddressOperations(ctx context.Context, orgID, projectID, networkInterface string, limit int, cursor string) (apiclient.Page[PrivateAddressOperation], error) {
	var out operationPage
	err := c.get(ctx, projectPath(orgID, projectID, "/private-address-operations"), pageQuery(limit, cursor, "networkInterface", networkInterface), &out)
	if out.Operations == nil {
		out.Operations = []PrivateAddressOperation{}
	}
	return apiclient.Page[PrivateAddressOperation]{Items: out.Operations, NextCursor: out.NextCursor}, err
}

// OperationFailedError is an operation that finished without succeeding.
type OperationFailedError struct{ Operation *PrivateAddressOperation }

func (e *OperationFailedError) Error() string {
	op := e.Operation
	if op.Error != nil {
		return fmt.Sprintf("%s of %s failed: %s (%s)", op.Type, op.Address, op.Error.Message, op.Error.Code)
	}
	return fmt.Sprintf("%s of %s failed", op.Type, op.Address)
}

// UnknownStatusError is an operation in a status this client doesn't know, or a succeeded one
// that isn't finished. It isn't taken for success or failure: the operation may still be running.
type UnknownStatusError struct{ Operation *PrivateAddressOperation }

func (e *UnknownStatusError) Error() string {
	return fmt.Sprintf("operation %s is %q (phase %q), which ezgh can't take as finished: check it again, or update ezgh", e.Operation.ID, e.Operation.Status, e.Operation.Phase)
}

// defaultPoll is how long to wait for an unfinished operation whose answer had no Retry-After.
const defaultPoll = 2 * time.Second

// WaitPrivateAddressOperation reads the operation, waiting as each answer's Retry-After says,
// until it has finished. It returns the operation once it has succeeded; a failed one is an
// *OperationFailedError, and a status it doesn't know an *UnknownStatusError (never success). A
// blockedReason only means it's still waiting. When ctx ends, it stops reading and returns
// ctx's error with the last operation read: the operation itself carries on (there's no cancel).
func (c *Client) WaitPrivateAddressOperation(ctx context.Context, orgID, projectID, operationID string) (*PrivateAddressOperation, error) {
	for {
		op, err := c.GetPrivateAddressOperation(ctx, orgID, projectID, operationID)
		if err != nil {
			return nil, err
		}
		switch op.Status {
		case "succeeded":
			// Terminal operations are finished and unblocked; anything else isn't taken on trust.
			if op.FinishedAt == nil || op.Phase != "complete" || op.BlockedReason != nil {
				return op, &UnknownStatusError{op}
			}
			return op, nil
		case "failed":
			return op, &OperationFailedError{op}
		case "queued", "running":
		default:
			return op, &UnknownStatusError{op}
		}
		wait := op.RetryAfter
		if wait == 0 {
			wait = defaultPoll
		}
		if err := c.sleep(ctx, wait); err != nil {
			return op, err
		}
	}
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.API.Sleep != nil {
		return c.API.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
