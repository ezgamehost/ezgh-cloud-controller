package credential_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// statements records the SQL the store sends, so the tests can count round trips.
type statements struct {
	mu  sync.Mutex
	sql []string
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sql = append(s.sql, d.SQL)
	return ctx
}

func (*statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// take returns every statement the store's pool sent since the last call, unfiltered, so a
// BEGIN/COMMIT or any other extra round trip around the ent call shows up in the counts.
func (s *statements) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sql
	s.sql = nil
	return out
}

// tracedStore is a Store on its own pool into d's database, with every statement recorded.
func tracedStore(t *testing.T, d *testdb.DB, prefix string) (*credential.Store, *statements) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := &statements{}
	cfg.ConnConfig.Tracer = rec
	db.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := db.FromPool(pool)
	t.Cleanup(h.Close) // before testdb drops the database
	return &credential.Store{DB: h, Prefix: prefix}, rec
}

func lastUsed(t *testing.T, d *testdb.DB, keyID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := d.Pool.QueryRow(context.Background(), `SELECT last_used_at FROM service_credentials WHERE key_id = $1`, keyID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func TestVerifyRevokeEnt(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	s, rec := tracedStore(t, d, "wk")

	iss, err := s.Issue(ctx, "billing", "admin@example.com", 30)
	if err != nil {
		t.Fatal(err)
	}
	if iss.ExpiresAt.Location() != time.UTC {
		t.Errorf("expiresAt in %v", iss.ExpiresAt.Location())
	}
	rec.take()
	header := "Bearer " + iss.Token

	// First use: one read, then the throttled last_used_at write.
	caller, err := s.Verify(ctx, header)
	if err != nil || caller != "billing" {
		t.Fatalf("Verify = %q, %v", caller, err)
	}
	if got := rec.take(); len(got) != 2 || !strings.HasPrefix(strings.TrimSpace(got[0]), "SELECT") || !strings.HasPrefix(strings.TrimSpace(got[1]), "UPDATE") {
		t.Fatalf("first use sent %q, want one SELECT and one UPDATE", got)
	} else {
		for _, want := range []string{"expires_at > now()", `"revoked_at" IS NULL`, "interval '1 minute'"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("verify query %q lacks %q", got[0], want)
			}
		}
		for _, want := range []string{"SET \"last_used_at\" = now()", "interval '1 minute'"} {
			if !strings.Contains(got[1], want) {
				t.Errorf("touch %q lacks %q", got[1], want)
			}
		}
		for _, q := range got {
			if strings.Contains(q, "?") {
				t.Errorf("unbound placeholder in %q", q)
			}
		}
	}
	first := lastUsed(t, d, iss.KeyID)
	if first == nil {
		t.Fatal("last_used_at not recorded")
	}

	// Within the minute: the hot path is one round trip and writes nothing.
	if caller, err := s.Verify(ctx, header); err != nil || caller != "billing" {
		t.Fatalf("Verify = %q, %v", caller, err)
	}
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("fresh use sent %d statements %q, want 1", len(got), got)
	}
	if again := lastUsed(t, d, iss.KeyID); again == nil || !again.Equal(*first) {
		t.Errorf("last_used_at moved within the minute: %v -> %v", first, again)
	}

	// Over a minute old (by the database's clock): recorded again.
	if _, err := d.Pool.Exec(ctx, `UPDATE service_credentials SET last_used_at = now() - interval '2 minutes' WHERE key_id = $1`, iss.KeyID); err != nil {
		t.Fatal(err)
	}
	var old time.Time
	_ = d.Pool.QueryRow(ctx, `SELECT last_used_at FROM service_credentials WHERE key_id = $1`, iss.KeyID).Scan(&old)
	if _, err := s.Verify(ctx, header); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 2 {
		t.Fatalf("stale use sent %d statements, want 2", len(got))
	}
	if now := lastUsed(t, d, iss.KeyID); now == nil || !now.After(old.Add(time.Minute)) {
		t.Errorf("stale last_used_at %v not refreshed (was %v)", now, old)
	}

	// Failures: a wrong secret, another prefix, an unknown key.
	other, err := s.Issue(ctx, "billing", "admin@example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	rec.take()
	wrongSecret := "Bearer " + iss.KeyID + "." + strings.SplitN(other.Token, ".", 2)[1]
	if _, err := s.Verify(ctx, wrongSecret); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("wrong secret: %v", err)
	}
	if got := rec.take(); len(got) != 1 {
		t.Errorf("wrong secret sent %d statements, want 1 (no last_used_at write)", len(got))
	}
	if _, err := s.Verify(ctx, "Bearer pk_"+strings.TrimPrefix(iss.Token, "wk_")); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("other prefix: %v", err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Errorf("other prefix reached the database: %q", got)
	}
	if _, err := s.Verify(ctx, "Bearer wk_0000000000000000."+strings.SplitN(iss.Token, ".", 2)[1]); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("unknown key: %v", err)
	}

	// Expired by the database's clock.
	if _, err := d.Pool.Exec(ctx, `UPDATE service_credentials SET expires_at = now() - interval '1 second' WHERE key_id = $1`, other.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, "Bearer "+other.Token); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("expired: %v", err)
	}

	// Revoke: true once, then false; unknown keys are false; a revoked key stops at once.
	rec.take()
	if ok, err := s.Revoke(ctx, iss.KeyID); err != nil || !ok {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if got := rec.take(); len(got) != 1 || !strings.Contains(got[0], `SET "revoked_at" = now()`) {
		t.Errorf("revoke sent %q", got)
	}
	var revoked *time.Time
	var dbNow time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT revoked_at, now() FROM service_credentials WHERE key_id = $1`, iss.KeyID).Scan(&revoked, &dbNow); err != nil {
		t.Fatal(err)
	}
	if revoked == nil || revoked.After(dbNow) || dbNow.Sub(*revoked) > time.Minute {
		t.Errorf("revoked_at %v, database now %v", revoked, dbNow)
	}
	if ok, err := s.Revoke(ctx, iss.KeyID); err != nil || ok {
		t.Errorf("second Revoke = %v, %v", ok, err)
	}
	var again *time.Time
	_ = d.Pool.QueryRow(ctx, `SELECT revoked_at FROM service_credentials WHERE key_id = $1`, iss.KeyID).Scan(&again)
	if again == nil || !again.Equal(*revoked) {
		t.Errorf("second Revoke moved revoked_at: %v -> %v", revoked, again)
	}
	if ok, err := s.Revoke(ctx, "wk_0000000000000000"); err != nil || ok {
		t.Errorf("Revoke(unknown) = %v, %v", ok, err)
	}
	if _, err := s.Verify(ctx, header); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("revoked: %v", err)
	}
}

func TestListEnt(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	s, rec := tracedStore(t, d, "qk")

	empty, err := s.List(ctx)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("List on no credentials = %#v, %v; want an empty, non-nil slice", empty, err)
	}

	var keys = map[string]string{}
	for _, c := range []struct{ name, caller, by string }{
		{"b1", "zeta", "ops@example.com"},
		{"a2", "alpha", "admin@example.com"},
		{"a1", "alpha", "admin@example.com"},
	} {
		iss, err := s.Issue(ctx, c.caller, c.by, 7)
		if err != nil {
			t.Fatal(err)
		}
		keys[c.name] = iss.KeyID
	}
	// Distinct creation times (ms precision), out of insertion order; one revoked, one used.
	for name, at := range map[string]string{"a1": "2026-01-01 00:00:00.123+00", "a2": "2026-02-01 00:00:00.456+00", "b1": "2025-12-01 00:00:00+00"} {
		if _, err := d.Pool.Exec(ctx, `UPDATE service_credentials SET created_at = $2::timestamptz WHERE key_id = $1`, keys[name], at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE service_credentials SET revoked_at = '2026-03-01 12:00:00.789+00', last_used_at = '2026-02-28 08:00:00+00' WHERE key_id = $1`, keys["a2"]); err != nil {
		t.Fatal(err)
	}
	rec.take()

	got, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q := rec.take(); len(q) != 1 || strings.Contains(q[0], "secret_hash") {
		t.Errorf("List sent %q; want one statement that never reads secret_hash", q)
	}

	// The oracle: quotas-stack's ListCredentials query today.
	rows, err := d.Pool.Query(ctx, `SELECT key_id, caller, created_by, created_at, expires_at, revoked_at, last_used_at FROM service_credentials ORDER BY caller, created_at`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (credential.Credential, error) {
		var c credential.Credential
		err := r.Scan(&c.KeyID, &c.Caller, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt, &c.RevokedAt, &c.LastUsedAt)
		return c, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(got) != 3 {
		t.Fatalf("List = %d credentials, want %d", len(got), len(want))
	}
	if got[0].KeyID != keys["a1"] || got[1].KeyID != keys["a2"] || got[2].KeyID != keys["b1"] {
		t.Errorf("order %s %s %s, want a1 a2 b1", got[0].KeyID, got[1].KeyID, got[2].KeyID)
	}
	same := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	for i := range got {
		g, w := got[i], want[i]
		if g.KeyID != w.KeyID || g.Caller != w.Caller || g.CreatedBy != w.CreatedBy ||
			!g.CreatedAt.Equal(w.CreatedAt) || !g.ExpiresAt.Equal(w.ExpiresAt) ||
			!same(g.RevokedAt, w.RevokedAt) || !same(g.LastUsedAt, w.LastUsedAt) {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
		for _, at := range []*time.Time{&g.CreatedAt, &g.ExpiresAt, g.RevokedAt, g.LastUsedAt} {
			if at != nil && at.Location() != time.UTC {
				t.Errorf("row %d: time in %v, want UTC", i, at.Location())
			}
		}
	}
	if a2 := got[1]; a2.RevokedAt == nil || a2.RevokedAt.Format(time.RFC3339Nano) != "2026-03-01T12:00:00.789Z" ||
		a2.LastUsedAt == nil || a2.LastUsedAt.Format(time.RFC3339Nano) != "2026-02-28T08:00:00Z" ||
		a2.CreatedAt.Format(time.RFC3339Nano) != "2026-02-01T00:00:00.456Z" {
		t.Errorf("a2 = %+v", a2)
	}
	if got[0].RevokedAt != nil || got[0].LastUsedAt != nil {
		t.Errorf("a1 = %+v, want no revoked or last-used time", got[0])
	}
}

// A credential bound to a namespace (contracts/service-auth.md, "Delegated calls"): only these
// can call auth "delegated" routes, and VerifyCaller names the namespace.
func TestIssueWithNamespace(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	s := &credential.Store{DB: d.DB, Prefix: "bsk"}
	iss, err := s.IssueFor(ctx, credential.IssueRequest{Caller: "compute-api", Namespace: "compute", CreatedBy: "ops@example.com", Days: 30})
	if err != nil {
		t.Fatal(err)
	}
	if iss.Namespace != "compute" || iss.Caller != "compute-api" {
		t.Errorf("issued %+v", iss)
	}
	caller, err := s.VerifyCaller(ctx, "Bearer "+iss.Token)
	if err != nil || caller != (credential.Caller{Name: "compute-api", Namespace: "compute"}) {
		t.Fatalf("VerifyCaller = %+v, %v", caller, err)
	}
	if name, err := s.Verify(ctx, "Bearer "+iss.Token); err != nil || name != "compute-api" {
		t.Errorf("Verify = %q, %v", name, err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Namespace != "compute" {
		t.Errorf("List = %+v, %v", list, err)
	}
	if _, err := s.VerifyCaller(ctx, "Bearer bsk_0000000000000000."+strings.SplitN(iss.Token, ".", 2)[1]); !errors.Is(err, credential.ErrInvalid) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestIssueWithoutNamespace(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	s := &credential.Store{DB: d.DB, Prefix: "wk"}
	iss, err := s.Issue(ctx, "billing", "admin@example.com", 7)
	if err != nil {
		t.Fatal(err)
	}
	if iss.Namespace != "" {
		t.Errorf("Issue bound %q", iss.Namespace)
	}
	caller, err := s.VerifyCaller(ctx, "Bearer "+iss.Token)
	if err != nil || caller != (credential.Caller{Name: "billing"}) {
		t.Fatalf("VerifyCaller = %+v, %v", caller, err)
	}
	if name, err := s.Verify(ctx, "Bearer "+iss.Token); err != nil || name != "billing" {
		t.Errorf("Verify = %q, %v", name, err)
	}
	var ns *string
	if err := d.Pool.QueryRow(ctx, `SELECT namespace FROM service_credentials WHERE key_id = $1`, iss.KeyID).Scan(&ns); err != nil || ns != nil {
		t.Errorf("namespace column = %v, %v; want NULL", ns, err)
	}
	if list, err := s.List(ctx); err != nil || len(list) != 1 || list[0].Namespace != "" {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestIssueBadNamespace(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	s := &credential.Store{DB: d.DB, Prefix: "wk"}
	for _, ns := range []string{"Compute", "c", "compute-api", "compute1", strings.Repeat("a", 33)} {
		if _, err := s.IssueFor(ctx, credential.IssueRequest{Caller: "compute-api", Namespace: ns, CreatedBy: "ops", Days: 1}); err == nil {
			t.Errorf("IssueFor namespace %q succeeded", ns)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM service_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; want none written", n, err)
	}
	// The database refuses it too.
	if _, err := d.Pool.Exec(ctx, `INSERT INTO service_credentials (key_id, caller, secret_hash, expires_at, created_by, namespace) VALUES ('wk_0000000000000000', 'x', 'x', now(), 'x', 'Compute')`); err == nil {
		t.Error("the CHECK constraint accepted Compute")
	}
}
