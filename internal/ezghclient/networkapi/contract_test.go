package networkapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against network-api's own OpenAPI document (api/openapi/network-api.json).
func TestClientMatchesNetworkAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/network-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Network", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, p := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	eni, pip, vpc, sbn, ngw := "eni_k2m9x0a7qpe4", "pip_k2m9x0a7qpe4", "vpc_k2m9x0a7qpe4", "sbn_k2m9x0a7qpe4", "ngw_k2m9x0a7qpe4"
	opID := "019a12f0-1234-7000-8000-000000000001"
	rtb, off := "rtb_k2m9x0a7qpe4", false
	routes := []RouteInput{{Destination: "0.0.0.0/0", Target: RouteTargetInput{Type: "networkGateway", ID: ngw}}}
	yes, name, tags, fws := true, "web", map[string]string{"team": "a"}, []string{"fwl_k2m9x0a7qpe4"}
	openapitest.Check(t, spec, rec, []openapitest.Call{
		{Name: "ListNetworkInterfaces", Run: func() error {
			_, err := c.ListNetworkInterfaces(ctx, o, p, "ins_k2m9x0a7qpe4", "available", 20, "c")
			return err
		}, Response: interfacePage{}},
		{Name: "GetNetworkInterface", Run: func() error { _, err := c.GetNetworkInterface(ctx, o, p, eni); return err }, Response: NetworkInterface{}},
		{Name: "CreateNetworkInterface", Run: func() error {
			_, err := c.CreateNetworkInterface(ctx, o, p, NetworkInterfaceCreate{Name: "web", Description: &name, Firewalls: &fws, IPv4: &InterfaceIPv4Ask{Allocate: &yes}, Tags: tags})
			return err
		}, Request: NetworkInterfaceCreate{}, Response: NetworkInterface{}},
		{Name: "CreatePrivateNetworkInterface", Run: func() error {
			_, err := c.CreateNetworkInterface(ctx, o, p, NetworkInterfaceCreate{Name: "db", Subnet: sbn, PrivateIPv4Address: "10.0.1.20", IPv4: &InterfaceIPv4Ask{Allocate: &yes}, IPv6: &InterfaceIPv6Ask{Allocate: true}})
			return err
		}, Request: NetworkInterfaceCreate{}, Response: NetworkInterface{}},
		{Name: "UpdateNetworkInterface", Run: func() error {
			_, err := c.UpdateNetworkInterface(ctx, o, p, eni, NetworkInterfaceUpdate{Name: &name, Description: &name, Tags: &tags, SourceDestCheck: &off})
			return err
		}, Request: NetworkInterfaceUpdate{}, Response: NetworkInterface{}},
		{Name: "DeleteNetworkInterface", Run: func() error { return c.DeleteNetworkInterface(ctx, o, p, eni) }},
		{Name: "ListIpv4Addresses", Run: func() error { _, err := c.ListIPv4Addresses(ctx, o, p, eni, 20, "c"); return err }, Response: addressPage{}},
		{Name: "GetIpv4Address", Run: func() error { _, err := c.GetIPv4Address(ctx, o, p, pip); return err }, Response: IPv4Address{}},
		{Name: "AllocateIpv4Address", Run: func() error {
			_, err := c.AllocateIPv4Address(ctx, o, p, IPv4AddressCreate{Name: "web", Tags: tags})
			return err
		}, Request: IPv4AddressCreate{}, Response: IPv4Address{}},
		{Name: "UpdateIpv4Address", Run: func() error {
			_, err := c.UpdateIPv4Address(ctx, o, p, pip, IPv4AddressUpdate{Name: &name, AutoRelease: &yes, Tags: &tags})
			return err
		}, Request: IPv4AddressUpdate{}, Response: IPv4Address{}},
		{Name: "AssociateIpv4Address", Run: func() error {
			_, err := c.AssociateIPv4Address(ctx, o, p, pip, IPv4AddressAssociate{NetworkInterface: eni, AllowReassociation: true})
			return err
		}, Request: IPv4AddressAssociate{}, Response: IPv4Address{}},
		{Name: "DisassociateIpv4Address", Run: func() error { _, err := c.DisassociateIPv4Address(ctx, o, p, pip); return err }, Response: IPv4Address{}},
		{Name: "ReleaseIpv4Address", Run: func() error { return c.ReleaseIPv4Address(ctx, o, p, pip) }},
		{Name: "ListVpcs", Run: func() error { _, err := c.ListVPCs(ctx, o, p, 20, "c"); return err }, Response: vpcPage{}},
		{Name: "GetVpc", Run: func() error { _, err := c.GetVPC(ctx, o, p, vpc); return err }, Response: VPC{}},
		{Name: "CreateVpc", Run: func() error {
			_, err := c.CreateVPC(ctx, o, p, VPCCreate{Name: "main", Description: &name, Tags: tags, IPv4CIDR: "10.0.0.0/16"})
			return err
		}, Request: VPCCreate{}, Response: VPC{}},
		{Name: "UpdateVpc", Run: func() error {
			_, err := c.UpdateVPC(ctx, o, p, vpc, VPCUpdate{Name: &name, Description: &name, Tags: &tags})
			return err
		}, Request: VPCUpdate{}, Response: VPC{}},
		{Name: "DeleteVpc", Run: func() error { return c.DeleteVPC(ctx, o, p, vpc) }},
		{Name: "ListSubnets", Run: func() error { _, err := c.ListSubnets(ctx, o, p, vpc, 20, "c"); return err }, Response: subnetPage{}},
		{Name: "GetSubnet", Run: func() error { _, err := c.GetSubnet(ctx, o, p, sbn); return err }, Response: Subnet{}},
		{Name: "CreateSubnet", Run: func() error {
			_, err := c.CreateSubnet(ctx, o, p, SubnetCreate{Name: "app", Description: &name, Tags: tags, VPC: vpc, IPv4CIDR: "10.0.1.0/24"})
			return err
		}, Request: SubnetCreate{}, Response: Subnet{}},
		{Name: "UpdateSubnet", Run: func() error {
			_, err := c.UpdateSubnet(ctx, o, p, sbn, SubnetUpdate{Name: &name, Description: &name, Tags: &tags, AutoAssignPublicIPv4: &yes, AutoAssignPublicIPv6: &yes, RouteTable: json.RawMessage(`"` + rtb + `"`)})
			return err
		}, Request: SubnetUpdate{}, Response: Subnet{}},
		{Name: "ListNetworkGateways", Run: func() error { _, err := c.ListNetworkGateways(ctx, o, p, vpc, 20, "c"); return err }, Response: networkGatewayPage{}},
		{Name: "GetNetworkGateway", Run: func() error { _, err := c.GetNetworkGateway(ctx, o, p, ngw); return err }, Response: NetworkGateway{}},
		{Name: "CreateNetworkGateway", Run: func() error {
			_, err := c.CreateNetworkGateway(ctx, o, p, NetworkGatewayCreate{Name: "edge", Description: &name, Tags: tags})
			return err
		}, Request: NetworkGatewayCreate{}, Response: NetworkGateway{}},
		{Name: "UpdateNetworkGateway", Run: func() error {
			_, err := c.UpdateNetworkGateway(ctx, o, p, ngw, NetworkGatewayUpdate{Name: &name, Description: &name, Tags: &tags})
			return err
		}, Request: NetworkGatewayUpdate{}, Response: NetworkGateway{}},
		{Name: "AttachNetworkGateway", Run: func() error { _, err := c.AttachNetworkGateway(ctx, o, p, ngw, vpc); return err }, Request: NetworkGatewayAttach{}, Response: NetworkGateway{}},
		{Name: "DetachNetworkGateway", Run: func() error { _, err := c.DetachNetworkGateway(ctx, o, p, ngw); return err }, Response: NetworkGateway{}},
		{Name: "DeleteNetworkGateway", Run: func() error { return c.DeleteNetworkGateway(ctx, o, p, ngw) }},
		{Name: "DeleteSubnet", Run: func() error { return c.DeleteSubnet(ctx, o, p, sbn) }},
		{Name: "ListRouteTables", Run: func() error { _, err := c.ListRouteTables(ctx, o, p, vpc, 20, "c"); return err }, Response: routeTablePage{}},
		{Name: "GetRouteTable", Run: func() error { _, _, err := c.GetRouteTable(ctx, o, p, rtb); return err }, Response: RouteTable{}},
		{Name: "CreateRouteTable", Run: func() error {
			_, err := c.CreateRouteTable(ctx, o, p, RouteTableCreate{Name: "private", Description: &name, Tags: tags, VPC: vpc})
			return err
		}, Request: RouteTableCreate{}, Response: RouteTable{}},
		{Name: "UpdateRouteTable", Run: func() error {
			_, err := c.UpdateRouteTable(ctx, o, p, rtb, RouteTableUpdate{Name: &name, Description: &name, Tags: &tags})
			return err
		}, Request: RouteTableUpdate{}, Response: RouteTable{}},
		{Name: "ReplaceRoutes", Run: func() error {
			_, err := c.ReplaceRoutes(ctx, o, p, rtb, []json.RawMessage{json.RawMessage(`{"destination":"0.0.0.0/0","target":{"type":"networkGateway","id":"` + ngw + `"}}`)}, `"3"`)
			return err
		}, Request: routesReplace{}, Response: RouteTable{}},
		{Name: "SetRoutes", Run: func() error { _, err := c.SetRoutes(ctx, o, p, rtb, routes, ""); return err }, Request: routesSet{}, Response: RouteTable{}},
		{Name: "RemoveRoutes", Run: func() error { _, err := c.RemoveRoutes(ctx, o, p, rtb, []string{"0.0.0.0/0"}); return err }, Request: routesRemove{}, Response: RouteTable{}},
		{Name: "DeleteRouteTable", Run: func() error { return c.DeleteRouteTable(ctx, o, p, rtb) }},
		{Name: "AssignPrivateAddress", Run: func() error {
			_, err := c.AssignPrivateAddress(ctx, o, p, eni, PrivateAddressAssign{Family: "ipv4", Address: "10.0.1.21"})
			return err
		}, Request: PrivateAddressAssign{}, Response: PrivateAddressOperation{}},
		{Name: "UnassignPrivateAddress", Run: func() error {
			_, err := c.UnassignPrivateAddress(ctx, o, p, eni, PrivateAddressUnassign{Family: "ipv4", Address: "10.0.1.21", ExpectedGeneration: "7"})
			return err
		}, Request: PrivateAddressUnassign{}, Response: PrivateAddressOperation{}},
		{Name: "ReassignPrivateAddress", Run: func() error {
			_, err := c.ReassignPrivateAddress(ctx, o, p, eni, PrivateAddressReassign{Family: "ipv6", Address: "fd3c::a", ExpectedGeneration: "7", DestinationInterface: "eni_d3st00000001"})
			return err
		}, Request: PrivateAddressReassign{}, Response: PrivateAddressOperation{}},
		{Name: "AssignPrivatePrefix", Run: func() error {
			_, err := c.AssignPrivatePrefix(ctx, o, p, eni, PrivatePrefixAssign{Family: "ipv4", Prefix: "10.0.1.16/28"})
			return err
		}, Request: PrivatePrefixAssign{}, Response: PrivateAddressOperation{}},
		{Name: "UnassignPrivatePrefix", Run: func() error {
			_, err := c.UnassignPrivatePrefix(ctx, o, p, eni, PrivatePrefixUnassign{Family: "ipv4", Prefix: "10.0.1.16/28", ExpectedGeneration: "3"})
			return err
		}, Request: PrivatePrefixUnassign{}, Response: PrivateAddressOperation{}},
		{Name: "GetPrivateAddressOperation", Run: func() error { _, err := c.GetPrivateAddressOperation(ctx, o, p, opID); return err }, Response: PrivateAddressOperation{}},
		{Name: "ListPrivateAddressOperations", Run: func() error {
			_, err := c.ListPrivateAddressOperations(ctx, o, p, eni, 20, "c")
			return err
		}, Response: operationPage{}},
	}, nil)
}
