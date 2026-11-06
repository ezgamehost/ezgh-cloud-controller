package resource

import (
	"maps"
	"slices"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
)

// registered is every registered resource type's Trails type, by slug prefix (contracts/resources.md,
// "Resource types"; contracts/vectors/resource-types.json checks it): what a Trails event's
// resources[].type shows for a resource of that prefix, whichever product records the event.
var registered = map[string]string{
	"alr": "AlertRule",              // obs
	"app": "Application",            // sso
	"bkt": "Bucket",                 // objects
	"bot": "Bot",                    // org-api
	"cpt": "ContactPoint",           // obs
	"crt": "Certificate",            // certificates
	"ctk": "ClientToken",            // obs
	"dau": "DomainAuthorization",    // certificates
	"dbb": "Backup",                 // databases
	"dbg": "BlueGreenDeployment",    // databases
	"dbi": "Database",               // databases
	"dgp": "Group",                  // sso
	"dir": "Directory",              // sso
	"dom": "Domain",                 // domains
	"dsh": "Dashboard",              // obs
	"dst": "Dataset",                // obs
	"eab": "AcmeBinding",            // certificates
	"eni": "NetworkInterface",       // network
	"fap": "AccessPoint",            // files
	"fen": "Environment",            // flags
	"fld": "Folder",                 // org-api
	"flg": "Flag",                   // flags
	"fmt": "MountTarget",            // files
	"fsg": "Segment",                // flags
	"fsk": "SdkKey",                 // flags
	"fsy": "FileSystem",             // files
	"fwl": "Firewall",               // firewall
	"grp": "Group",                  // org-api
	"gtr": "GitRepository",          // git
	"idp": "IdentityProvider",       // sso
	"igk": "IngestKey",              // org-api
	"img": "Image",                  // images
	"ina": "Account",                // inference
	"ins": "Instance",               // compute
	"iss": "Issue",                  // obs
	"kad": "Addon",                  // kubernetes
	"kae": "AccessEntry",            // kubernetes
	"kcl": "Cluster",                // kubernetes
	"kms": "Key",                    // keys
	"kng": "NodeGroup",              // kubernetes
	"kpa": "PodIdentityAssociation", // kubernetes
	"lbr": "LoadBalancer",           // lb
	"mcn": "Connection",             // mcpcentral
	"mcs": "Server",                 // mcpcentral
	"mgw": "Gateway",                // mcpcentral
	"mps": "PersonalServer",         // mcpcentral
	"oak": "AccessKey",              // objects
	"obt": "Batch",                  // ocr
	"oip": "OidcProvider",           // org-api
	"org": "Organization",           // org-api
	"pca": "CertificateAuthority",   // certificates
	"pct": "PrivateCertificate",     // certificates
	"pip": "Ipv4Address",            // network
	"pol": "Policy",                 // org-api
	"prc": "Processor",              // ocr
	"prj": "Project",                // org-api
	"ptc": "PullThroughCacheRule",   // registry
	"qsb": "Subscription",           // queues
	"que": "Queue",                  // queues
	"rep": "Repository",             // registry
	"sec": "Secret",                 // secrets
	"sil": "Silence",                // obs
	"slo": "Slo",                    // obs
	"snp": "Snapshot",               // blockstore
	"ssh": "SshKey",                 // compute
	"svc": "Service",                // obs
	"tdl": "TrailDelivery",          // trails
	"tgp": "TargetGroup",            // lb
	"usr": "User",                   // sso
	"vip": "Vip",                    // network
	"vol": "Volume",                 // blockstore
	"wfa": "App",                    // workflows
	"wfs": "Schedule",               // workflows
	"wgt": "Widget",                 // widgets
	"zon": "Zone",                   // dns
}

// Unknown is the Trails type of a resource whose prefix isn't registered.
const Unknown = "Unknown"

// TypeOf is the Trails type of the resource a name identifies, by its last slug's prefix: a
// registered type (contracts/resources.md, "Resource types"), else Unknown. A product's own types
// are registered too; Call.AddResource prefers its manifest's, so a type not yet in the table types
// in its own product.
func TypeOf(name string) string {
	slug := LastSlug(name)
	if len(slug) < 4 || slug[3] != '_' {
		return Unknown
	}
	if t, ok := registered[slug[:3]]; ok {
		return t
	}
	return Unknown
}

// RegisteredType is the Trails type of a registered prefix (wgt → Widget), if it is one.
func RegisteredType(prefix string) (string, bool) {
	t, ok := registered[prefix]
	return t, ok
}

// RegisteredPrefixes are the registered prefixes, sorted.
func RegisteredPrefixes() []string { return slices.Sorted(maps.Keys(registered)) }

// ProjectOf is the name of the project a resource sits in (ezgh::org_…:prj_…), whatever its region,
// for a name under a project; false for an organization, a project itself, or what isn't a name.
func ProjectOf(name string) (string, bool) {
	chain, _, _ := strings.Cut(name, "/")
	parts := strings.Split(chain, ":")
	if len(parts) < 5 || parts[0] != Partition || !strings.HasPrefix(parts[2], "org_") || !strings.HasPrefix(parts[3], "prj_") ||
		!ids.IsSlug(parts[2]) || !ids.IsSlug(parts[3]) {
		return "", false
	}
	return Project(parts[2], parts[3]), true
}
