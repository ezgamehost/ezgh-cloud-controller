package config_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLocalDefaults(t *testing.T) {
	e := config.New(env(map[string]string{}))
	p := e.LoadPlatform("widgets")
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	// Locally, no QUOTAS_API_URL means no calls to Quotas (the local fallback).
	if !p.Local || p.Region != "us-local-1" || !slices.Equal(p.Regions, []string{"us-local-1"}) || !p.CORSOrigins.Any || p.DatabaseURL != "postgres://postgres:postgres@127.0.0.1:5432/widgets" || p.MeteringCredential != "" || p.QuotasAPIURL != "" || p.PublicBaseURL != "https://widgets.ezghcloud.localhost" {
		t.Errorf("%+v", p)
	}
}

// APP_ENV=production refuses every local default, and reports every missing value at once.
func TestProductionRefusesLocalDefaults(t *testing.T) {
	e := config.New(env(map[string]string{"APP_ENV": "production"}))
	e.LoadPlatform("widgets")
	err := e.Err()
	if err == nil {
		t.Fatal("production started with local defaults")
	}
	for _, name := range []string{"REGION", "DATABASE_URL", "ORG_API_URL", "AUDIT_API_URL", "METERING_INGEST_URL", "CORS_ORIGINS", "PUBLIC_BASE_URL", "INTERNAL_API_TOKEN", "CURSOR_SECRET", "METERING_INGEST_CREDENTIAL", "QUOTAS_API_URL", "QUOTAS_API_CREDENTIAL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("no complaint about %s", name)
		}
	}
}

func TestProductionRefusesWildcardCORSAndBadValues(t *testing.T) {
	e := config.New(env(map[string]string{
		"APP_ENV": "production", "CORS_ORIGINS": "*", "CURSOR_SECRET": "short", "METERING_INGEST_CREDENTIAL": "pk_nope",
		"PORT": "99999", "REGION": "mars-1", "QUOTAS_API_URL": "quotas-api:4800", "QUOTAS_API_CREDENTIAL": "rk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	}))
	e.LoadPlatform("widgets")
	e.Port(4700)
	err := e.Err()
	for _, want := range []string{"CORS_ORIGINS=* is only allowed", "CURSOR_SECRET must be at least 32", "METERING_INGEST_CREDENTIAL must be pk_", "PORT must be", "REGION \"mars-1\"", "QUOTAS_API_URL must be an http(s) URL", "QUOTAS_API_CREDENTIAL must be qk_"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestProductionWithEverythingSet(t *testing.T) {
	e := config.New(env(map[string]string{
		"APP_ENV": "production", "REGION": "us-west-1", "DATABASE_URL": "postgres://x@y/z",
		"ORG_API_URL": "http://org-api.login-stack.svc.cluster.local:4100", "AUDIT_API_URL": "http://audit-api:4400",
		"METERING_INGEST_URL": "http://metering-ingest:4600", "CORS_ORIGINS": "https://console.ezghcloud.com",
		"PUBLIC_BASE_URL":    "https://Widgets.ezghcloud.com/",
		"INTERNAL_API_TOKEN": strings.Repeat("t", 32), "CURSOR_SECRET": strings.Repeat("c", 32),
		"METERING_INGEST_CREDENTIAL": "pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
		"QUOTAS_API_URL":             "http://quotas-api.quotas-stack.svc.cluster.local:4800",
		"QUOTAS_API_CREDENTIAL":      "qk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	}))
	p := e.LoadPlatform("widgets")
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	if p.Local || p.CORSOrigins.Any || p.CORSOrigins.List[0] != "https://console.ezghcloud.com" || p.PublicBaseURL != "https://widgets.ezghcloud.com" {
		t.Errorf("%+v", p)
	}
}

// A service that isn't a metering producer starts in production without metering's settings.
func TestLoadCoreSkipsMetering(t *testing.T) {
	env := map[string]string{
		"APP_ENV": "production", "REGION": "us-west-1", "DATABASE_URL": "postgres://x@db/quotas",
		"ORG_API_URL": "http://org-api:4100", "AUDIT_API_URL": "http://audit-api:4400", "CORS_ORIGINS": "https://console.ezghcloud.com",
		"PUBLIC_BASE_URL":    "https://quotas.ezghcloud.com",
		"INTERNAL_API_TOKEN": "0123456789abcdef0123", "CURSOR_SECRET": "0123456789abcdef0123456789abcdef",
	}
	e := config.New(func(k string) string { return env[k] })
	e.LoadCore("quotas")
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	e = config.New(func(k string) string { return env[k] })
	e.LoadPlatform("widgets")
	if e.Err() == nil {
		t.Fatal("LoadPlatform needs metering's settings in production")
	}
}

// production is TestLoadCoreSkipsMetering's settings with extra.
func production(extra map[string]string) func(string) string {
	m := map[string]string{
		"APP_ENV": "production", "REGION": "us-west-1", "DATABASE_URL": "postgres://x@db/quotas",
		"ORG_API_URL": "http://org-api:4100", "AUDIT_API_URL": "http://audit-api:4400", "CORS_ORIGINS": "https://console.ezghcloud.com",
		"PUBLIC_BASE_URL":    "https://quotas.ezghcloud.com",
		"INTERNAL_API_TOKEN": "0123456789abcdef0123", "CURSOR_SECRET": "0123456789abcdef0123456789abcdef",
	}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

// Without REGIONS a deployment knows only its own region: production never offers us-local-1.
func TestRegionsDefaultToTheDeploymentsRegion(t *testing.T) {
	e := config.New(production(nil))
	p := e.LoadCore("quotas")
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Regions, []string{"us-west-1"}) {
		t.Errorf("regions %v, want [us-west-1]", p.Regions)
	}
	e = config.New(env(map[string]string{"REGION": "us-local-1"}))
	if p := e.LoadCore("quotas"); e.Err() != nil || !slices.Equal(p.Regions, []string{"us-local-1"}) {
		t.Errorf("local regions %v (%v), want [us-local-1]", p.Regions, e.Err())
	}
}

func TestExplicitRegions(t *testing.T) {
	e := config.New(production(map[string]string{"REGIONS": " us-east-2, us-west-1,,us-west-1 "}))
	p := e.LoadCore("quotas")
	if err := e.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Regions, []string{"us-east-2", "us-west-1"}) {
		t.Errorf("regions %v", p.Regions)
	}
	e = config.New(env(map[string]string{"REGIONS": "us-west-1,us-local-1"}))
	if p := e.LoadCore("quotas"); e.Err() != nil || !slices.Equal(p.Regions, []string{"us-west-1", "us-local-1"}) {
		t.Errorf("local regions %v (%v)", p.Regions, e.Err())
	}
}

func TestBadRegions(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"REGIONS": "us-east-2"}, `REGION "us-west-1" isn't one of REGIONS (us-east-2)`},
		{map[string]string{"REGIONS": "us-west-1,Mars"}, `REGIONS "Mars" isn't a region like us-west-1`},
		{map[string]string{"REGION": "mars-1", "REGIONS": "mars-1"}, `REGION "mars-1" isn't a region like us-west-1`},
		{map[string]string{"REGION": "us-local-1"}, `REGION "us-local-1" is only allowed when APP_ENV=local`},
		{map[string]string{"REGIONS": "us-west-1,us-local-1"}, `REGIONS "us-local-1" is only allowed when APP_ENV=local`},
	} {
		e := config.New(production(c.env))
		e.LoadCore("quotas")
		if err := e.Err(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want %q", c.env, err, c.want)
		}
	}
}

// PUBLIC_BASE_URL is an https origin, returned canonical; anything else fails, local included.
func TestPublicBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://quotas.ezghcloud.com":          "https://quotas.ezghcloud.com",
		"https://Quotas.EzghCloud.com:443/":     "https://quotas.ezghcloud.com",
		"https://quotas.ezghcloud.localhost":    "https://quotas.ezghcloud.localhost",
		"http://quotas.ezghcloud.com":           "",
		"https://quotas.ezghcloud.com:8443":     "",
		"https://quotas.ezghcloud.com/v1":       "",
		"https://quotas.ezghcloud.com?x=1":      "",
		"https://user@quotas.ezghcloud.com":     "",
		"quotas.ezghcloud.com":                  "",
		"https://quotas.ezghcloud.com/#section": "",
	} {
		for _, appEnv := range []string{"local", "production"} {
			e := config.New(production(map[string]string{"APP_ENV": appEnv, "PUBLIC_BASE_URL": raw}))
			p := e.LoadCore("quotas")
			err := e.Err()
			switch {
			case want != "" && (err != nil || p.PublicBaseURL != want):
				t.Errorf("%s %q: %q %v", appEnv, raw, p.PublicBaseURL, err)
			case want == "" && (err == nil || !strings.Contains(err.Error(), "PUBLIC_BASE_URL must be an https origin")):
				t.Errorf("%s %q: accepted as %q", appEnv, raw, p.PublicBaseURL)
			}
		}
	}
}

// ORG_API_SERVICE_CREDENTIAL is optional everywhere, production included, and checked when set.
func TestOrgAPIServiceCredential(t *testing.T) {
	e := config.New(production(nil))
	if p := e.LoadCore("quotas"); e.Err() != nil || p.OrgAPIServiceCredential != "" {
		t.Fatalf("%v %q", e.Err(), p.OrgAPIServiceCredential)
	}
	const key = "ok_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	e = config.New(production(map[string]string{"ORG_API_SERVICE_CREDENTIAL": key}))
	if p := e.LoadCore("quotas"); e.Err() != nil || p.OrgAPIServiceCredential != key {
		t.Fatalf("%v %q", e.Err(), p.OrgAPIServiceCredential)
	}
	e = config.New(production(map[string]string{"ORG_API_SERVICE_CREDENTIAL": "rk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}))
	if e.LoadCore("quotas"); e.Err() == nil || !strings.Contains(e.Err().Error(), "ORG_API_SERVICE_CREDENTIAL must be ok_") {
		t.Fatalf("an rk_ key was accepted: %v", e.Err())
	}
}

// SHUTDOWN_DRAIN_SECONDS: 20 by default, 1-3600 when set.
func TestShutdownDrain(t *testing.T) {
	e := config.New(production(nil))
	if p := e.LoadCore("quotas"); e.Err() != nil || p.ShutdownDrain != 20*time.Second {
		t.Fatalf("%v %v", e.Err(), p.ShutdownDrain)
	}
	e = config.New(production(map[string]string{"SHUTDOWN_DRAIN_SECONDS": "960"}))
	if p := e.LoadCore("mcpcentral"); e.Err() != nil || p.ShutdownDrain != 960*time.Second {
		t.Fatalf("%v %v", e.Err(), p.ShutdownDrain)
	}
	for _, bad := range []string{"0", "3601", "ten"} {
		e = config.New(production(map[string]string{"SHUTDOWN_DRAIN_SECONDS": bad}))
		if e.LoadCore("quotas"); e.Err() == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
