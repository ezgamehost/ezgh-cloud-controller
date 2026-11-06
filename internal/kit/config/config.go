// Package config reads a service's environment with the platform's rules
// (contracts/deployment.md, "Configuration"), the same as login-stack's service-kit config.ts:
//
//   - APP_ENV=local (the default) allows local defaults and CORS_ORIGINS=*.
//   - Any other APP_ENV is production: every value a command uses must be set explicitly, and a
//     service refuses to start without it. A misconfigured service should crash at start, not
//     misbehave at 3 a.m.
//   - A value that is set but invalid fails everywhere, local included.
//
// Errors are collected, so one start reports every problem.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resourceurl"
)

// Env reads settings through getenv (os.Getenv in production, a map in tests).
type Env struct {
	getenv func(string) string
	AppEnv string
	Local  bool
	errs   []error
}

// New reads APP_ENV and starts collecting settings.
func New(getenv func(string) string) *Env {
	appEnv := strings.TrimSpace(getenv("APP_ENV"))
	if appEnv == "" {
		appEnv = "local"
	}
	return &Env{getenv: getenv, AppEnv: appEnv, Local: appEnv == "local"}
}

// Fail records a problem found outside the helpers.
func (e *Env) Fail(format string, args ...any) { e.errs = append(e.errs, fmt.Errorf(format, args...)) }

// Err is every problem found, or nil.
func (e *Env) Err() error { return errors.Join(e.errs...) }

// Optional is name's value, or "".
func (e *Env) Optional(name string) string { return strings.TrimSpace(e.getenv(name)) }

// Required is name's value. Locally a missing value falls back to localDefault; in production
// (or with no local default) it's an error.
func (e *Env) Required(name, localDefault string) string {
	if v := e.Optional(name); v != "" {
		return v
	}
	if e.Local && localDefault != "" {
		return localDefault
	}
	e.Fail("missing required environment variable %s", name)
	return ""
}

// Int is name as an integer in [min, max], or def when it isn't set.
func (e *Env) Int(name string, def, min, max int) int {
	raw := e.Optional(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		e.Fail("%s must be an integer from %d to %d, got %q", name, min, max, raw)
		return def
	}
	return n
}

// Port is PORT, defaulting to the service's own port (from its manifest) everywhere: the port is
// part of the image, not a secret or an environment difference.
func (e *Env) Port(def int) int { return e.Int("PORT", def, 1, 65535) }

// URL is a required http(s) base URL, without a trailing slash.
func (e *Env) URL(name, localDefault string) string {
	raw := e.Required(name, localDefault)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		e.Fail("%s must be an http(s) URL, got %q", name, raw)
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// OptionalURL is an http(s) base URL that may be unset (""): required outside APP_ENV=local
// when requiredOutsideLocal, and checked whenever it's set.
func (e *Env) OptionalURL(name string, requiredOutsideLocal bool) string {
	if e.Optional(name) == "" {
		if requiredOutsideLocal && !e.Local {
			e.Fail("missing required environment variable %s", name)
		}
		return ""
	}
	return e.URL(name, "")
}

// Secret is a required secret of at least minBytes bytes. Local defaults are for the local stack
// only; production must set its own.
func (e *Env) Secret(name, localDefault string, minBytes int) string {
	v := e.Required(name, localDefault)
	if v != "" && len(v) < minBytes {
		e.Fail("%s must be at least %d bytes", name, minBytes)
	}
	return v
}

// Credential is a per-service bearer credential another service issued (<prefix>_….…), e.g.
// METERING_INGEST_CREDENTIAL with prefix pk. required: in production it must be set; locally a
// missing one is allowed and the caller decides what to skip.
func (e *Env) Credential(name, prefix string, required bool) string {
	v := e.Optional(name)
	if v == "" {
		if required && !e.Local {
			e.Fail("missing required environment variable %s", name)
		}
		return ""
	}
	if !credential.Valid(v, prefix) {
		e.Fail("%s must be %s_<16 [a-z0-9]>.<43 base64url characters>", name, prefix)
		return ""
	}
	return v
}

// PublicBaseURL is PUBLIC_BASE_URL, the product's public base URL: an https origin
// (https://<namespace>.ezghcloud.com in production; locally https://<namespace>.ezghcloud.localhost,
// portless's URL, by default). It's returned canonical (host lowercased, no ":443", no trailing
// slash). Anything but an https origin (http, another port, a path, a query) fails everywhere:
// with an EZGH SSO resource token the kit sends org-api requestUrl, built from it and the
// request's raw path, never from Host or X-Forwarded-* (contracts/api-conventions.md, "Canonical
// resource URLs").
func (e *Env) PublicBaseURL(namespace string) string {
	raw := e.Required("PUBLIC_BASE_URL", "https://"+namespace+".ezghcloud.localhost")
	if raw == "" {
		return ""
	}
	origin, ok := resourceurl.Origin(raw)
	if !ok {
		e.Fail("PUBLIC_BASE_URL must be an https origin like https://%s.ezghcloud.com, got %q", namespace, raw)
		return ""
	}
	return origin
}

// CORSOrigins is CORS_ORIGINS: a comma-separated allowlist, or "*" (the local default), which is
// refused outside APP_ENV=local.
func (e *Env) CORSOrigins() httpapi.Origins {
	raw := e.Required("CORS_ORIGINS", "*")
	if raw == "*" {
		if !e.Local {
			e.Fail("CORS_ORIGINS=* is only allowed when APP_ENV=local")
		}
		return httpapi.Origins{Any: true}
	}
	var list []string
	for _, origin := range strings.Split(raw, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			e.Fail("CORS_ORIGINS: %q isn't an origin like https://console.ezghcloud.com", origin)
			continue
		}
		list = append(list, origin)
	}
	return httpapi.Origins{List: list}
}

// Local defaults for the platform's settings, from the local stacks' scripts.
const (
	LocalRegion           = "us-local-1"
	LocalOrgAPIURL        = "https://orgs.ezghcloud.localhost"
	LocalAuditAPIURL      = "http://audit-stack-audit-api.test:4400"
	LocalMeteringURL      = "http://billing-stack-metering-ingest.test:4600"
	LocalQuotasAPIURL     = "http://quotas-stack-quotas-api.test:4800"
	LocalInternalAPIToken = "local-internal-token"
	localCursorSecret     = "local-cursor-secret-not-for-production-0000"
)

// Platform is what every product reads to plug into the platform.
type Platform struct {
	AppEnv string
	Local  bool
	Region string
	// Regions are the regions this deployment accepts in resource names and offers customers
	// (REGIONS, comma-separated). By default only Region: us-local-1 locally, us-west-1 in
	// production.
	Regions     []string
	DatabaseURL string
	OrgAPIURL   string
	AuditAPIURL string
	MeteringURL string
	CORSOrigins httpapi.Origins
	// PublicBaseURL is the product's public base URL, an https origin (PUBLIC_BASE_URL), for
	// authz.Client.PublicBaseURL: with an EZGH SSO resource token, Check sends org-api requestUrl.
	PublicBaseURL string
	// RedisURL is optional: rate quotas and rate limits fail open without it.
	RedisURL string
	// OrgAPIServiceCredential is the namespace's org-api ok_… key (ORG_API_SERVICE_CREDENTIAL), for
	// authz.Client.ServiceCredential: [being built] org-api's internal operations its registration
	// declares in orgApi.operations, such as authz.CheckBatch's permissions.batch. Optional
	// everywhere (most products call none); checked whenever it's set. A product that needs it
	// requires it itself.
	OrgAPIServiceCredential string
	// InternalAPIToken is the shared X-Internal-Token (contracts/service-auth.md, "Today").
	InternalAPIToken string
	CursorSecret     string
	// MeteringCredential is the producer key, pk_…; empty locally means the usage relay,
	// watermarks and closures don't run.
	MeteringCredential string
	// RegistrationCredential is the namespace's rk_… key for org-api's registration API; empty
	// locally means registration sends X-Internal-Token, which org-api accepts with APP_ENV=local.
	RegistrationCredential string
	// QuotasAPIURL is quotas-api, in-cluster (contracts/quotas.md, "Configuration in a product").
	// Empty is only allowed locally: then no calls are made, allocation quotas use the manifest's
	// defaults and every other quota is unlimited.
	QuotasAPIURL string
	// QuotasAPICredential is the namespace's qk_… key, for checks and for registering quotas;
	// empty locally means X-Internal-Token, which quotas-api accepts with APP_ENV=local.
	QuotasAPICredential string
	// ShutdownDrain is how long requests in flight may run on at shutdown (SHUTDOWN_DRAIN_SECONDS,
	// 1-3600, default 20), for service.ServeOptions: a product with long endpoints
	// (timeoutSeconds) raises it, and its pods' terminationGracePeriodSeconds above it.
	ShutdownDrain time.Duration
}

// LoadPlatform reads the platform settings of a metering producer: LoadCore's, and metering's.
// service names the local database (postgres://…/<service>).
func (e *Env) LoadPlatform(service string) Platform {
	p := e.LoadCore(service)
	p.MeteringURL = e.URL("METERING_INGEST_URL", LocalMeteringURL)
	p.MeteringCredential = e.Credential("METERING_INGEST_CREDENTIAL", "pk", true)
	p.QuotasAPIURL = e.OptionalURL("QUOTAS_API_URL", true)
	p.QuotasAPICredential = e.Credential("QUOTAS_API_CREDENTIAL", "qk", true)
	return p
}

// LoadCore reads the settings every service needs, without metering's: a service that isn't a
// metering producer (Quotas) doesn't need METERING_INGEST_URL or METERING_INGEST_CREDENTIAL.
// service is the namespace: it names the local database (postgres://…/<service>) and the local
// PUBLIC_BASE_URL (https://<service>.ezghcloud.localhost).
func (e *Env) LoadCore(service string) Platform {
	p := Platform{AppEnv: e.AppEnv, Local: e.Local}
	p.Region = e.Required("REGION", LocalRegion)
	e.region("REGION", p.Region)
	// Only the deployment's own region unless REGIONS says otherwise: us-local-1 locally, and
	// us-west-1 alone in production, so a deployment never offers customers a region it doesn't
	// run in (Quotas' customer list answers these).
	if p.Region != "" {
		p.Regions = []string{p.Region}
	}
	if raw := e.Optional("REGIONS"); raw != "" {
		p.Regions = nil
		for _, r := range strings.Split(raw, ",") {
			if r = strings.TrimSpace(r); r != "" && !contains(p.Regions, r) {
				e.region("REGIONS", r)
				p.Regions = append(p.Regions, r)
			}
		}
		if p.Region != "" && !contains(p.Regions, p.Region) {
			e.Fail("REGION %q isn't one of REGIONS (%s)", p.Region, strings.Join(p.Regions, ", "))
		}
	}
	p.DatabaseURL = e.Required("DATABASE_URL", "postgres://postgres:postgres@127.0.0.1:5432/"+service)
	p.OrgAPIURL = e.URL("ORG_API_URL", LocalOrgAPIURL)
	p.AuditAPIURL = e.URL("AUDIT_API_URL", LocalAuditAPIURL)
	p.CORSOrigins = e.CORSOrigins()
	p.PublicBaseURL = e.PublicBaseURL(service)
	p.RedisURL = e.Optional("REDIS_URL")
	p.OrgAPIServiceCredential = e.Credential("ORG_API_SERVICE_CREDENTIAL", "ok", false)
	p.InternalAPIToken = e.Secret("INTERNAL_API_TOKEN", LocalInternalAPIToken, 16)
	p.CursorSecret = e.Secret("CURSOR_SECRET", localCursorSecret, 32)
	p.ShutdownDrain = time.Duration(e.Int("SHUTDOWN_DRAIN_SECONDS", 20, 1, 3600)) * time.Second
	return p
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]+$`)

// region checks one of name's regions: shaped like us-west-1 (contracts/resources.md, "Regions"),
// and the local stack's region only with APP_ENV=local.
func (e *Env) region(name, region string) {
	switch {
	case region == "":
	case !regionPattern.MatchString(region):
		e.Fail("%s %q isn't a region like us-west-1", name, region)
	case region == LocalRegion && !e.Local:
		e.Fail("%s %q is only allowed when APP_ENV=local", name, region)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
