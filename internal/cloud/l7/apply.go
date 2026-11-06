package l7

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/certificatesapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

// State is what exists for one Ingress group or Gateway, kept in annotation
// lb.ezghcloud.com/state on its objects: lb's state, the group it belongs to, and the
// certificates imported from Secrets.
type State struct {
	// Group names the owner (an Ingress group or "<ns>/<gateway>"), so an Ingress moved to
	// another group isn't read as holding the new one's state.
	Group string `json:"group,omitempty"`
	// Key starts the group's idempotency keys for as long as it lives (its first member's UID).
	Key string `json:"key,omitempty"`
	lb.State
	Certificates map[string]Imported `json:"certificates,omitempty"` // "<ns>/<secret>" → crt_
}

// Imported is a certificate imported from a Secret: its crt_ and a hash of what was imported (the
// Secret's content is never kept).
type Imported struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// Empty reports whether nothing is left to delete.
func (s State) Empty() bool { return s.State.Empty() && len(s.Certificates) == 0 }

// readState is the state an object's annotation holds.
func readState(ann map[string]string) (State, error) {
	var s State
	if v := ann[lb.StateAnnotation]; v != "" {
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			return State{}, fmt.Errorf("%s: %w", lb.StateAnnotation, err)
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

// Apply makes what exists match want: the certificates its listeners import, then (through
// lb.Apply) the pip_, an http target group per backend and the load balancer, then each listener
// with one PATCH carrying its default action and every rule, under If-Match; then the groups and
// imported certificates no listener uses any more. A want with Name "" tears everything down, the
// imported certificates last. Like lb.Apply, it returns the state so far even with an error.
func Apply(ctx context.Context, c *ezghclient.Clients, cfg config.Config, have State, want Model) (State, error) {
	st := have
	st.Certificates, st.Forged = maps.Clone(have.Certificates), nil
	if st.Empty() {
		if err := adoptCertificates(ctx, c, cfg, &st, want); err != nil {
			return have, err
		}
	}
	if want.Name != "" {
		for _, ref := range slices.Sorted(maps.Keys(want.Secrets)) {
			if err := importSecret(ctx, c, cfg, &st, want, ref); err != nil {
				return st, err
			}
		}
	}
	m := lb.Model{Key: want.Key, Name: want.Name, Named: want.Named, Subnet: cfg.Subnet, Tags: want.Tags}
	for _, b := range want.Backends {
		m.Groups = append(m.Groups, lb.GroupModel{Key: b.Key, Protocol: "http", Port: b.Port, ProxyProtocol: "none", HealthCheck: b.HealthCheck})
	}
	if want.Name != "" {
		m.SyncListeners = func(ctx context.Context, lst *lb.State) error { return syncListeners(ctx, c, cfg, lst, &st, want) }
	}
	forged := st.Forged
	var err error
	st.State, err = lb.Apply(ctx, c, cfg, st.State, m)
	st.Forged = append(forged, st.Forged...)
	if err != nil {
		return st, err
	}
	// Certificates go once no listener can use them: the listeners were just made to match, or
	// there's no load balancer.
	if st.LoadBalancer != "" && st.Status != "active" {
		return st, nil
	}
	for _, ref := range slices.Sorted(maps.Keys(st.Certificates)) {
		if want.Name != "" && want.Secrets[ref] != nil {
			continue
		}
		ours, err := ownCertificate(ctx, c, cfg, &st, want, ref)
		if err != nil {
			return st, err
		}
		if ours {
			if _, err := c.Certificates.DeleteCertificate(ctx, cfg.Org, cfg.Project, st.Certificates[ref].ID); err != nil {
				if err := certGone(ctx, c, cfg, err); err != nil {
					return st, err
				}
			}
		}
		delete(st.Certificates, ref)
		st.Epoch++
	}
	return st, nil
}

// certName is the name of the certificate imported for ref.
func certName(want Model, ref string) string { return cut(want.lbName() + "-" + ref) }

func (m Model) lbName() string { return cmp.Or(m.Name, m.Named) }

func hasTags(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return len(want) > 0
}

// ownCertificate checks the crt_ state names for ref before it's changed or deleted: it must carry
// want's tags and the name imported for ref (an adopted one, the name's prefix). One that's gone
// reads as false; one that isn't want's is forgotten (a forged state) and reads as false.
func ownCertificate(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model, ref string) (bool, error) {
	id := st.Certificates[ref].ID
	crt, err := c.Certificates.GetCertificate(ctx, cfg.Org, cfg.Project, id)
	if err != nil {
		return false, certGone(ctx, c, cfg, err)
	}
	name := crt.Name == certName(want, ref)
	if strings.HasPrefix(ref, adoptedPrefix) {
		name = strings.HasPrefix(crt.Name, cut(want.lbName()+"-"))
	}
	if want.lbName() == "" || !hasTags(crt.Tags, want.Tags) || !name {
		st.Forged = append(st.Forged, "certificate "+id)
		return false, nil
	}
	return true, nil
}

// adoptedPrefix keys a certificate found by tags and name whose Secret isn't wanted any more.
const adoptedPrefix = "adopted/"

// adoptCertificates finds what a lost state (kubectl replace, GitOps) left imported, by want's
// tags and name, so it's re-imported in place or deleted instead of duplicated.
func adoptCertificates(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model) error {
	if want.lbName() == "" || len(want.Tags) == 0 {
		return nil
	}
	crts, err := all(func(cursor string) (apiclient.Page[certificatesapi.Certificate], error) {
		return c.Certificates.ListCertificates(ctx, cfg.Org, cfg.Project, 100, cursor)
	})
	if err != nil {
		return err
	}
	for _, crt := range crts {
		if !hasTags(crt.Tags, want.Tags) || !strings.HasPrefix(crt.Name, cut(want.lbName()+"-")) {
			continue
		}
		key := adoptedPrefix + crt.Slug
		for ref := range want.Secrets {
			if crt.Name == certName(want, ref) {
				key = ref
			}
		}
		if st.Certificates == nil {
			st.Certificates = map[string]Imported{}
		}
		if _, dup := st.Certificates[key]; dup {
			key = adoptedPrefix + crt.Slug
		}
		st.Certificates[key] = Imported{ID: crt.Slug} // no hash: imported again, in place
	}
	return nil
}

// certGone is a delete's error, with a 404 as done while the project itself answers.
func certGone(ctx context.Context, c *ezghclient.Clients, cfg config.Config, err error) error {
	if !ezghclient.IsNotFound(err) {
		return err
	}
	if _, perr := c.Certificates.ListCertificates(ctx, cfg.Org, cfg.Project, 1, ""); perr != nil {
		return fmt.Errorf("not found, and project %s doesn't answer either: %w", cfg.Project, perr)
	}
	return nil
}

// secretHash fingerprints a TLS Secret's pair, so it's imported again only when it changes.
func secretHash(s *corev1.Secret) string {
	sum := sha256.Sum256(bytes.Join([][]byte{s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]}, []byte{0}))
	return hex.EncodeToString(sum[:])
}

// importSecret imports ref's pair into a crt_ owned by want, or a new version of the one it owns
// when the pair changed. The key goes into the request body only: never a log line or event.
func importSecret(ctx context.Context, c *ezghclient.Clients, cfg config.Config, st *State, want Model, ref string) error {
	s := want.Secrets[ref]
	hash := secretHash(s)
	have, ok := st.Certificates[ref]
	if ok && have.Hash == hash {
		return nil
	}
	if ok {
		ours, err := ownCertificate(ctx, c, cfg, st, want, ref)
		if err != nil {
			return err
		}
		if !ours {
			delete(st.Certificates, ref) // gone or forged: imported anew
			ok = false
		}
	}
	leaf, chain := splitChain(s.Data[corev1.TLSCertKey])
	key := string(s.Data[corev1.TLSPrivateKeyKey])
	if ok {
		if _, err := c.Certificates.ReimportCertificate(ctx, cfg.Org, cfg.Project, have.ID, certificatesapi.CertificateReimport{Certificate: leaf, CertificateChain: chain, PrivateKey: key}); err != nil {
			return err
		}
	} else {
		crt, err := c.Certificates.ImportCertificate(ezghclient.WithIdempotencyKey(ctx, want.key(st.Epoch, "-crt-"+importKey(ref, hash))), cfg.Org, cfg.Project,
			certificatesapi.CertificateImport{Name: certName(want, ref), Tags: want.Tags, Certificate: leaf, CertificateChain: chain, PrivateKey: key})
		if err != nil {
			return err
		}
		have.ID = crt.Slug
	}
	if st.Certificates == nil {
		st.Certificates = map[string]Imported{}
	}
	st.Certificates[ref] = Imported{ID: have.ID, Hash: hash}
	return nil
}

// importKey is a Secret's part of its import's idempotency key: the ref and its content.
func importKey(ref, hash string) string {
	sum := sha256.Sum256([]byte(ref + "\x00" + hash))
	return hex.EncodeToString(sum[:8])
}

// splitChain splits tls.crt into its first certificate and the chain after it (nil when none).
func splitChain(pemData []byte) (string, *string) {
	block, rest := pem.Decode(pemData)
	if block == nil {
		return string(pemData), nil
	}
	leaf := string(pem.EncodeToMemory(block))
	var chain []byte
	for {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		chain = append(chain, pem.EncodeToMemory(b)...)
	}
	if len(chain) == 0 {
		return leaf, nil
	}
	s := string(chain)
	return leaf, &s
}

// syncListeners makes the load balancer's listeners want's: a listener on a port no longer
// wanted, or of the other layer, goes; each wanted one is made, or changed by one PATCH under
// If-Match when it differs (a 412 rereads it once and tries again).
func syncListeners(ctx context.Context, c *ezghclient.Clients, cfg config.Config, lst *lb.State, st *State, want Model) error {
	org, prj := cfg.Org, cfg.Project
	have, err := all(func(cursor string) (apiclient.Page[lbapi.Listener], error) {
		return c.LB.ListListeners(ctx, org, prj, lst.LoadBalancer, 100, cursor)
	})
	if err != nil {
		return err
	}
	wanted := map[int]bool{}
	for _, l := range want.Listeners {
		wanted[l.Port] = true
	}
	byPort := map[int]string{}
	for _, l := range have {
		if wanted[l.Port] && (l.Protocol == "http" || l.Protocol == "https") {
			byPort[l.Port] = l.ID
			continue
		}
		if err := c.LB.DeleteListener(ctx, org, prj, lst.LoadBalancer, l.ID); err != nil && !ezghclient.IsNotFound(err) {
			return err
		}
	}
	lst.Listeners = map[int]string{}
	for _, l := range want.Listeners {
		body, err := wire(l, lst, st)
		if err != nil {
			return err
		}
		id := byPort[l.Port]
		if id == "" {
			// No fixed key: lb-api refuses a second listener on a port, and the list above adopts one.
			made, err := c.LB.CreateL7Listener(ctx, org, prj, lst.LoadBalancer, body)
			if err != nil {
				return err
			}
			lst.Listeners[l.Port] = made.ID
			continue
		}
		lst.Listeners[l.Port] = id
		if err := update(ctx, c, cfg, lst.LoadBalancer, id, body); err != nil {
			return err
		}
	}
	return nil
}

// update PATCHes listener id to body when it differs, with the ETag it was read at.
func update(ctx context.Context, c *ezghclient.Clients, cfg config.Config, lbID, id string, body lbapi.L7ListenerCreate) error {
	for attempt := 0; ; attempt++ {
		cur, etag, err := c.LB.GetL7Listener(ctx, cfg.Org, cfg.Project, lbID, id)
		if err != nil {
			return err
		}
		more := body.AdditionalCertificates
		if cur.Protocol == body.Protocol && equality.Semantic.DeepEqual(cur.DefaultAction, body.DefaultAction) && equality.Semantic.DeepEqual(cur.Rules, body.Rules) &&
			equality.Semantic.DeepEqual(cur.Certificate, body.Certificate) && equality.Semantic.DeepEqual(cur.AdditionalCertificates, more) {
			return nil
		}
		u := lbapi.L7ListenerUpdate{DefaultAction: body.DefaultAction, Rules: body.Rules, Certificate: body.Certificate}
		if cur.Protocol != body.Protocol {
			u.Protocol = &body.Protocol
		}
		if body.Certificate != nil {
			u.AdditionalCertificates = &more
		}
		_, err = c.LB.UpdateL7Listener(ctx, cfg.Org, cfg.Project, lbID, id, u, etag)
		if ezghclient.Code(err) == "precondition_failed" && attempt == 0 {
			continue
		}
		return err
	}
}

// wire is l in lb-api's terms: backends become their tgp_, Secrets their imported crt_, and every
// list is present (lb-api answers [] for an empty one).
func wire(l Listener, lst *lb.State, st *State) (lbapi.L7ListenerCreate, error) {
	body := lbapi.L7ListenerCreate{Protocol: l.Protocol, Port: l.Port, Rules: []lbapi.L7Rule{}}
	certs := slices.Clone(l.Certificates)
	for _, ref := range l.Secrets {
		if im, ok := st.Certificates[ref]; ok {
			certs = append(certs, im.ID)
		}
	}
	if len(certs) > 0 {
		body.Certificate, body.AdditionalCertificates = &certs[0], append([]string{}, certs[1:]...)
	}
	var err error
	if body.DefaultAction, err = wireAction(l.Default, lst); err != nil {
		return body, err
	}
	for _, r := range l.Rules {
		a, err := wireAction(r.Action, lst)
		if err != nil {
			return body, err
		}
		body.Rules = append(body.Rules, lbapi.L7Rule{Action: a, Match: lbapi.Match{
			Hosts: orEmpty(r.Hosts), Paths: orEmpty(r.Paths), Methods: orEmpty(r.Methods), Headers: orEmpty(r.Headers), QueryParams: orEmpty(r.Query),
		}})
	}
	return body, nil
}

func wireAction(a Action, lst *lb.State) (lbapi.L7Action, error) {
	out := lbapi.L7Action{Type: a.Type, Redirect: a.Redirect, FixedResponse: a.Fixed}
	if a.Type != "forward" {
		return out, nil
	}
	f := &lbapi.L7Forward{RequestHeaderModifier: wireHeaders(a.RequestHeaders), ResponseHeaderModifier: wireHeaders(a.ResponseHeaders)}
	for _, w := range a.Forward {
		tg := lst.Groups[w.Backend]
		if tg == "" {
			return out, fmt.Errorf("no target group for backend %s", w.Backend)
		}
		f.TargetGroups = append(f.TargetGroups, lbapi.L7WeightedGroup{TargetGroup: tg, Weight: w.Weight})
	}
	out.Forward = f
	return out, nil
}

func wireHeaders(h *HeaderModifier) *HeaderModifier {
	if h == nil {
		return nil
	}
	return &HeaderModifier{Set: orEmpty(h.Set), Add: orEmpty(h.Add), Remove: orEmpty(h.Remove)}
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// cut keeps a name within lb-api's 100 characters.
func cut(s string) string {
	if r := []rune(s); len(r) > 100 {
		return string(r[:100])
	}
	return s
}

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
