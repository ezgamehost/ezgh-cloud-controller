package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/networkapi"
)

// StateAnnotation keeps State on its owner, so a restart reads it back without a list.
const StateAnnotation = Prefix + "state"

// pageSize is every list's: lb-api and network-api take 1-100 and answer 400 above.
const pageSize = 100

// State is what exists for one owner.
type State struct {
	LoadBalancer string            `json:"loadBalancer,omitempty"`
	IPv4Address  string            `json:"ipv4Address,omitempty"` // pip_
	OwnIPv4      bool              `json:"ownIpv4,omitempty"`     // allocated here, so released here
	Listeners    map[int]string    `json:"listeners,omitempty"`   // port → lsn_
	Groups       map[string]string `json:"groups,omitempty"`      // GroupModel.Key → tgp_
	Status       string            `json:"status,omitempty"`      // the load balancer's
	IPv4         *string           `json:"ipv4,omitempty"`        // its addresses, once it has them
	IPv6         *string           `json:"ipv6,omitempty"`
	IPv4Of       string            `json:"ipv4Of,omitempty"` // the pip_ IPv4 is the address of
	// Operation is the load balancer create's, until it's active: if lb-api undoes the create,
	// the operation says why.
	Operation string `json:"operation,omitempty"`
	// Failed fingerprints the owner when its create failed; the owner (not Apply) skips Apply
	// until the fingerprint changes, so a failing create isn't repeated every cycle.
	Failed string `json:"failed,omitempty"`
	// Epoch moves on whenever something recorded here is gone, and goes into every create's
	// idempotency key: a key is replayed for 24 hours, so a group or load balancer made again
	// (a port re-added, LoadBalancer → ClusterIP → LoadBalancer) must not get the old one back.
	Epoch int `json:"epoch,omitempty"`
	// Forged lists what the state named that isn't this owner's (another's tags or name): dropped
	// from state, never changed or deleted, for a Warning on the object. Not kept.
	Forged []string `json:"-"`
}

func (s *State) forge(kind, id string) { s.Forged = append(s.Forged, kind+" "+id) }

// Empty reports whether nothing is left to delete.
func (s State) Empty() bool { return s.LoadBalancer == "" && s.IPv4Address == "" && len(s.Groups) == 0 }

// ReadState is the state an object's annotations hold.
func ReadState(ann map[string]string) (State, error) {
	var s State
	if v := ann[StateAnnotation]; v != "" {
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			return State{}, fmt.Errorf("%s: %w", StateAnnotation, err)
		}
	}
	return s, nil
}

func (m Model) key(epoch int, suffix string) string {
	k := m.Key
	if epoch > 0 {
		k += "-" + strconv.Itoa(epoch)
	}
	return k + suffix
}

// CreateFailedError is a load balancer create that lb-api undid (ipv6_unavailable,
// subnet_has_no_gateway_route, …).
type CreateFailedError struct{ Operation, Code, Message string }

func (e *CreateFailedError) Error() string {
	return fmt.Sprintf("creating the load balancer failed (%s): %s", e.Code, e.Message)
}

// Apply makes what exists match want, in order: the pip_, the target groups, the load balancer,
// and its listeners once it's active; then the groups want no longer has. A want with Name ""
// deletes the load balancer (waiting out its asynchronous delete), then the groups, then an owned
// pip_; a customer's pip_ is never released. It returns the state so far even with an error:
// whatever was made is recorded, nothing is deleted because a read was refused, and a 404 counts
// as gone only while the project itself answers.
func Apply(ctx context.Context, c *ezghclient.Clients, cfg config.Config, have State, want Model) (State, error) {
	st := have
	st.Listeners, st.Groups, st.Forged = maps.Clone(have.Listeners), maps.Clone(have.Groups), nil
	if st.Empty() && len(want.Tags) > 0 {
		var err error
		if st, err = adopt(ctx, c, cfg, st, want); err != nil {
			return have, err
		}
	}
	if want.Name == "" {
		return teardown(ctx, c, cfg, st, want)
	}
	org, prj := cfg.Org, cfg.Project
	if st.LoadBalancer == "" {
		if err := ownIPv4(ctx, c, cfg, &st, want); err != nil {
			return st, err
		}
	}

	// The address is chosen while there's no load balancer; lb-api can't change one's later.
	if st.LoadBalancer == "" && want.IPv4Address != st.IPv4Address && (want.IPv4Address != "" || !st.OwnIPv4) {
		if st.OwnIPv4 {
			if err := deleted(ctx, c, cfg, c.Network.ReleaseIPv4Address(ctx, org, prj, st.IPv4Address)); err != nil {
				return st, err
			}
			st.Epoch++
		}
		st.IPv4Address, st.OwnIPv4 = want.IPv4Address, false
	}
	if st.IPv4Address == "" {
		ip, err := c.Network.AllocateIPv4Address(ezghclient.WithIdempotencyKey(ctx, want.key(st.Epoch, "-ip")), org, prj,
			networkapi.IPv4AddressCreate{Name: want.Name, Tags: want.Tags})
		if err != nil {
			return st, err
		}
		st.IPv4Address, st.OwnIPv4 = ip.Slug, true
	}

	for _, g := range want.Groups {
		if err := ensureGroup(ctx, c, cfg, &st, want, g); err != nil {
			return st, err
		}
	}

	create, epoch := st.LoadBalancer == "", st.Epoch
	if !create {
		lb, err := c.LB.GetLoadBalancer(ctx, org, prj, st.LoadBalancer)
		isGone := err == nil && lb.Status == "deleted"
		if err != nil {
			if isGone, err = gone(ctx, c, cfg, err); err != nil {
				return st, err
			}
		}
		if !isGone && !want.Owns(lb.Tags, lb.Name, false) {
			// Not ours: forgotten, and the next pass makes ours.
			st.forge("load balancer", st.LoadBalancer)
			st.LoadBalancer, st.Listeners, st.Status, st.IPv4, st.IPv6, st.Operation = "", nil, "", nil, nil, ""
			return st, nil
		}
		if !isGone {
			st.LoadBalancer, st.Status, st.IPv6 = lb.Slug, lb.Status, lb.IPv6Address
			if err := resolveIPv4(ctx, c, cfg, &st, lb.IPv4Address); err != nil {
				return st, err
			}
			if st.Status == "active" {
				st.Operation = ""
			}
		} else {
			if st.Operation != "" {
				op, err := c.LB.GetOperation(ctx, org, st.Operation)
				if err != nil {
					return st, err
				}
				if op.Status == "failed" {
					code, _ := op.Error["code"].(string)
					msg, _ := op.Error["message"].(string)
					st.LoadBalancer, st.Listeners, st.Status, st.IPv4, st.IPv6, st.Operation = "", nil, "", nil, nil, ""
					st.Epoch++
					return st, &CreateFailedError{Operation: op.ID, Code: code, Message: msg}
				}
			}
			// Gone without our asking: made again under a new key (the old one would replay it).
			create, epoch = true, epoch+1
		}
	}
	if create {
		op, err := c.LB.CreateLoadBalancer(ezghclient.WithIdempotencyKey(ctx, want.key(epoch, "")), org, prj,
			lbapi.LoadBalancerCreate{Name: want.Name, Tags: want.Tags, Subnet: want.Subnet, IPv4Address: st.IPv4Address})
		if err != nil {
			// The vanished load balancer stays recorded until its replacement exists.
			return st, err
		}
		// shortcut: the operation names the load balancer by its UUID; the next read records its lbr_.
		st.LoadBalancer, st.Listeners, st.Status, st.IPv4, st.IPv6 = op.Target.ID, nil, "provisioning", nil, nil
		st.Operation, st.Epoch = op.ID, epoch
		return st, nil
	}
	if st.Status != "active" {
		return st, nil
	}
	sync := want.SyncListeners
	if sync == nil {
		sync = func(ctx context.Context, st *State) error { return syncListeners(ctx, c, cfg, st, want) }
	}
	if err := sync(ctx, &st); err != nil {
		return st, err
	}

	wanted := map[string]bool{}
	for _, g := range want.Groups {
		wanted[g.Key] = true
	}
	for _, key := range slices.Sorted(maps.Keys(st.Groups)) {
		if !wanted[key] {
			if err := deleteGroup(ctx, c, cfg, &st, want, key); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}

// deleteGroup deletes the group state names by key, unless it's gone or isn't want's (then it's
// only forgotten).
func deleteGroup(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model, key string) error {
	id := st.Groups[key]
	tg, err := c.LB.GetTargetGroup(ctx, cfg.Org, cfg.Project, id)
	switch {
	case err != nil:
		if isGone, err := gone(ctx, c, cfg, err); !isGone {
			return err
		}
	case !want.Owns(tg.Tags, tg.Name, true):
		st.forge("target group", id)
	default:
		if err := deleted(ctx, c, cfg, c.LB.DeleteTargetGroup(ctx, cfg.Org, cfg.Project, id)); err != nil {
			return err
		}
	}
	delete(st.Groups, key)
	st.Epoch++
	return nil
}

// ownIPv4 checks the pip_ state says was allocated here before it's used or released: one that's
// gone or isn't want's is forgotten.
func ownIPv4(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model) error {
	if !st.OwnIPv4 || st.IPv4Address == "" {
		return nil
	}
	ip, err := c.Network.GetIPv4Address(ctx, cfg.Org, cfg.Project, st.IPv4Address)
	if err != nil {
		if isGone, err := gone(ctx, c, cfg, err); !isGone {
			return err
		}
	} else if want.Owns(ip.Tags, ip.Name, false) {
		return nil
	} else {
		st.forge("IPv4 address", st.IPv4Address)
	}
	st.IPv4Address, st.OwnIPv4 = "", false
	st.Epoch++
	return nil
}

// resolveIPv4 sets st.IPv4 to the address of pip, the load balancer's ipv4Address (a pip_ ID, not
// an address), reading it from network-api only when pip isn't the one st.IPv4 already is.
func resolveIPv4(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, pip *string) error {
	if pip == nil {
		st.IPv4, st.IPv4Of = nil, ""
		return nil
	}
	if st.IPv4 != nil && st.IPv4Of == *pip {
		return nil
	}
	st.IPv4, st.IPv4Of = nil, "" // never published while unresolved
	ip, err := c.Network.GetIPv4Address(ctx, cfg.Org, cfg.Project, *pip)
	if err != nil {
		return err
	}
	if ip.Address != "" {
		st.IPv4, st.IPv4Of = &ip.Address, *pip
	}
	return nil
}

// gone reports whether err is a 404 while the project itself answers: a wrong or deleted project
// in cloud.json must never read as everything being gone.
func gone(ctx context.Context, c *ezghclient.Clients, cfg config.Config, err error) (bool, error) {
	if !ezghclient.IsNotFound(err) {
		return false, err
	}
	if _, perr := c.LB.ListLoadBalancers(ctx, cfg.Org, cfg.Project, 1, ""); perr != nil {
		return false, fmt.Errorf("not found, and project %s doesn't answer either: %w", cfg.Project, perr)
	}
	return true, nil
}

// deleted is a delete's error, with a confirmed 404 as done.
func deleted(ctx context.Context, c *ezghclient.Clients, cfg config.Config, err error) error {
	if err == nil {
		return nil
	}
	_, err = gone(ctx, c, cfg, err)
	return err
}

func ensureGroup(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model, g GroupModel) error {
	org, prj := cfg.Org, cfg.Project
	epoch := st.Epoch
	if id := st.Groups[g.Key]; id != "" {
		tg, err := c.LB.GetTargetGroup(ctx, org, prj, id)
		if err == nil && want.Owns(tg.Tags, tg.Name, true) {
			if u, ok := groupUpdate(tg, g); ok {
				_, err = c.LB.UpdateTargetGroup(ctx, org, prj, id, u)
			}
			return err
		}
		if err == nil {
			st.forge("target group", id) // not ours: forgotten, and ours made
		} else if isGone, err := gone(ctx, c, cfg, err); !isGone {
			return err
		}
		epoch++
	}
	tg, err := c.LB.CreateTargetGroup(ezghclient.WithIdempotencyKey(ctx, want.key(epoch, "-"+groupSuffix(g.Key))), org, prj, lbapi.TargetGroupCreate{
		Name: groupName(want, g), Tags: want.Tags, VPC: cfg.VPC, Protocol: &g.Protocol, Port: g.Port,
		ProxyProtocol: &g.ProxyProtocol, HealthCheck: g.HealthCheck, DeregistrationDelaySeconds: g.DeregistrationDelaySeconds,
	})
	if err != nil {
		return err // a vanished group stays recorded until its replacement exists
	}
	if st.Groups == nil {
		st.Groups = map[string]string{}
	}
	st.Groups[g.Key], st.Epoch = tg.Slug, epoch
	return nil
}

func groupName(want Model, g GroupModel) string { return cut(want.LBName() + "-" + groupSuffix(g.Key)) }

// groupUpdate is the change that brings tg to g, comparing only what g sets.
func groupUpdate(tg *lbapi.TargetGroup, g GroupModel) (lbapi.TargetGroupUpdate, bool) {
	var u lbapi.TargetGroupUpdate
	changed := false
	if tg.Port != g.Port {
		u.Port, changed = &g.Port, true
	}
	if g.ProxyProtocol != "" && tg.ProxyProtocol != g.ProxyProtocol {
		u.ProxyProtocol, changed = &g.ProxyProtocol, true
	}
	if d := g.DeregistrationDelaySeconds; d != nil && *d != tg.DeregistrationDelaySeconds {
		u.DeregistrationDelaySeconds, changed = d, true
	}
	if h := g.HealthCheck; h != nil {
		have := tg.HealthCheck
		port := "null"
		if have.Port != nil {
			port = strconv.Itoa(*have.Port)
		}
		if differs(h.Protocol, have.Protocol) || differs(h.Path, have.Path) || differs(h.Matcher, have.Matcher) ||
			differs(h.IntervalSeconds, have.IntervalSeconds) || differs(h.TimeoutSeconds, have.TimeoutSeconds) ||
			differs(h.HealthyThreshold, have.HealthyThreshold) || differs(h.UnhealthyThreshold, have.UnhealthyThreshold) ||
			h.Port != nil && string(h.Port) != port {
			u.HealthCheck, changed = h, true
		}
	}
	return u, changed
}

func differs[T comparable](want *T, have T) bool { return want != nil && *want != have }

// all reads every page of a list.
func all[T any](list func(cursor string) (apiclient.Page[T], error)) ([]T, error) {
	var out []T
	for cursor := ""; ; {
		page, err := list(cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == nil {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

func syncListeners(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model) error {
	org, prj := cfg.Org, cfg.Project
	have, err := all(func(cursor string) (apiclient.Page[lbapi.Listener], error) {
		return c.LB.ListListeners(ctx, org, prj, st.LoadBalancer, pageSize, cursor)
	})
	if err != nil {
		return err
	}
	byPort := map[int]lbapi.Listener{}
	for _, l := range have {
		byPort[l.Port] = l
	}
	// Removed ports go first, so a swap of ports never trips the listener limit.
	wantPorts := map[int]bool{}
	for _, l := range want.Listeners {
		wantPorts[l.Port] = true
	}
	for _, port := range slices.Sorted(maps.Keys(byPort)) {
		if !wantPorts[port] {
			if err := deleted(ctx, c, cfg, c.LB.DeleteListener(ctx, org, prj, st.LoadBalancer, byPort[port].ID)); err != nil {
				return err
			}
		}
	}
	st.Listeners = map[int]string{}
	for _, l := range want.Listeners {
		group := st.Groups[l.Group]
		var cert *string
		var more []string
		if len(l.Certificates) > 0 {
			cert, more = &l.Certificates[0], l.Certificates[1:]
		}
		cur, ok := byPort[l.Port]
		if !ok {
			// No fixed key: lb-api refuses a second listener on a port, and the list above adopts one.
			made, err := c.LB.CreateListener(ctx, org, prj, st.LoadBalancer, lbapi.ListenerCreate{
				Protocol: l.Protocol, Port: l.Port, TargetGroup: group, IdleTimeoutSeconds: l.IdleTimeoutSeconds,
				Certificate: cert, AdditionalCertificates: more, AllowedSourceCidrs: l.AllowedSourceCidrs,
			})
			if err != nil {
				return err
			}
			st.Listeners[l.Port] = made.ID
			continue
		}
		st.Listeners[l.Port] = cur.ID
		var u lbapi.ListenerUpdate
		changed := false
		if cur.Protocol != l.Protocol {
			u.Protocol, changed = &l.Protocol, true
		}
		if cur.TargetGroup == nil || *cur.TargetGroup != group {
			u.TargetGroup, changed = &group, true
		}
		if differs(l.IdleTimeoutSeconds, cur.IdleTimeoutSeconds) {
			u.IdleTimeoutSeconds, changed = l.IdleTimeoutSeconds, true
		}
		if cert != nil && (cur.Certificate == nil || *cur.Certificate != *cert || !slices.Equal(more, cur.AdditionalCertificates)) {
			u.Certificate, u.AdditionalCertificates, changed = cert, &more, true
		}
		// shortcut: allowed CIDRs are set when given, never reset to lb-api's default when the
		// annotation goes (the default differs with IPv6); delete and re-add the port for that.
		if l.AllowedSourceCidrs != nil && !slices.Equal(l.AllowedSourceCidrs, cur.AllowedSourceCidrs) {
			u.AllowedSourceCidrs, changed = l.AllowedSourceCidrs, true
		}
		if changed {
			if _, err := c.LB.UpdateListener(ctx, org, prj, st.LoadBalancer, cur.ID, u); err != nil {
				return err
			}
		}
	}
	return nil
}

// adopt finds what a lost state annotation (kubectl replace, GitOps) left behind, by both of
// want's tags, so it's reused or deleted instead of duplicated. Only with an empty state, so it's
// rare.
func adopt(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st State, want Model) (State, error) {
	org, prj := cfg.Org, cfg.Project
	if want.LBName() == "" {
		return st, nil
	}
	lbs, err := all(func(cursor string) (apiclient.Page[lbapi.LoadBalancer], error) {
		return c.LB.ListLoadBalancers(ctx, org, prj, pageSize, cursor)
	})
	if err != nil {
		return st, err
	}
	for _, lb := range lbs {
		// shortcut: one load balancer is adopted; a second (two lost states) is left for the customer.
		if want.Owns(lb.Tags, lb.Name, false) && lb.Status != "deleting" && lb.Status != "deleted" && st.LoadBalancer == "" {
			st.LoadBalancer, st.Status = lb.Slug, lb.Status
		}
	}
	groups, err := all(func(cursor string) (apiclient.Page[lbapi.TargetGroup], error) {
		return c.LB.ListTargetGroups(ctx, org, prj, pageSize, cursor)
	})
	if err != nil {
		return st, err
	}
	for _, tg := range groups {
		if !want.Owns(tg.Tags, tg.Name, true) {
			continue
		}
		key := "adopted/" + tg.Slug // not wanted, so deleted once no listener uses it
		for _, g := range want.Groups {
			if groupName(want, g) == tg.Name {
				key = g.Key
			}
		}
		if st.Groups == nil {
			st.Groups = map[string]string{}
		}
		if st.Groups[key] == "" {
			st.Groups[key] = tg.Slug
		} else {
			st.Groups["adopted/"+tg.Slug] = tg.Slug
		}
	}
	pips, err := all(func(cursor string) (apiclient.Page[networkapi.IPv4Address], error) {
		return c.Network.ListIPv4Addresses(ctx, org, prj, "", pageSize, cursor)
	})
	if err != nil {
		return st, err
	}
	for _, ip := range pips {
		if want.Owns(ip.Tags, ip.Name, false) && st.IPv4Address == "" {
			st.IPv4Address, st.OwnIPv4 = ip.Slug, true
		}
	}
	return st, nil
}

func teardown(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st State, want Model) (State, error) {
	org, prj := cfg.Org, cfg.Project
	if st.LoadBalancer != "" {
		lb, err := c.LB.GetLoadBalancer(ctx, org, prj, st.LoadBalancer)
		isGone := err == nil && lb.Status == "deleted"
		if err != nil {
			if isGone, err = gone(ctx, c, cfg, err); err != nil {
				return st, err
			}
		}
		switch {
		case isGone:
		case !want.Owns(lb.Tags, lb.Name, false):
			st.forge("load balancer", st.LoadBalancer) // not ours: never deleted
		case lb.Status == "deleting":
			st.Status = "deleting"
			return st, nil
		default:
			// Its listeners go with it.
			if _, err := c.LB.DeleteLoadBalancer(ctx, org, prj, st.LoadBalancer); err != nil {
				if err := deleted(ctx, c, cfg, err); err != nil {
					return st, err
				}
			}
			st.Status = "deleting"
			return st, nil
		}
		st.LoadBalancer, st.Listeners, st.Status, st.IPv4, st.IPv6, st.Operation = "", nil, "", nil, nil, ""
		st.Epoch++
	}
	for _, key := range slices.Sorted(maps.Keys(st.Groups)) {
		if err := deleteGroup(ctx, c, cfg, &st, want, key); err != nil {
			return st, err
		}
	}
	if err := ownIPv4(ctx, c, cfg, &st, want); err != nil {
		return st, err
	}
	if st.IPv4Address != "" {
		if st.OwnIPv4 {
			if err := deleted(ctx, c, cfg, c.Network.ReleaseIPv4Address(ctx, org, prj, st.IPv4Address)); err != nil {
				return st, err
			}
			st.Epoch++
		}
		st.IPv4Address, st.OwnIPv4 = "", false
	}
	st.Listeners, st.Groups = nil, nil
	return st, nil
}
