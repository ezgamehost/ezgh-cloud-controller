package usage

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/decimal"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
)

// Issue is one problem with an event, as metering-ingest reports it: a dotted path into the
// event ("" for the whole event) and a message that starts with the ingest code when the rule
// has one ("invalid_dimension: …").
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Code is the ingest code the issue's message starts with, or "".
func (i Issue) Code() string {
	if m := codePrefix.FindStringSubmatch(i.Message); m != nil {
		return m[1]
	}
	return ""
}

var codePrefix = regexp.MustCompile(`^([a-z]+(?:_[a-z]+)+): `)

// Options are what validation checks an event against.
type Options struct {
	// Producer is this service's producer name: the only one its credential may report as.
	Producer string
	// Regions are the configured regions (METERING_REGIONS on ingest).
	Regions []string
	// Meters is the registry.
	Meters Registry
	// Now is the clock, for "no more than 5 minutes in the future".
	Now time.Time
}

// Validation rules are login-stack service-kit's validateUsageEvent, the one metering-ingest
// runs, path for path and message for message (contracts/vectors/usage-events.json holds every
// case). Two stages: every field's shape first, all problems at once; only a well-formed event
// is then checked across fields and against its meter, the clock and the configuration.

const (
	msgObject         = "Expected object"
	msgString         = "Expected string"
	msgStringOrNull   = "Expected string | null"
	msgMissing        = "Missing key"
	msgExcess         = "Expected no excess property"
	msgID             = "Expected 1–200 printable ASCII characters, no spaces"
	msgNonEmpty       = "Expected a value with a length of at least 1"
	msgUUID           = "Expected a lowercase UUID"
	msgResourceName   = "Expected a resource name like ezgh:us-west-1:org_…:prj_…:ins_…"
	msgTime           = "Expected a UTC timestamp like 2026-09-28T10:00:00Z or 2026-09-28T10:00:00.250Z"
	msgQuantity       = "Expected a decimal like 12 or -1200.5 (up to 26 digits, 12 after the point)"
	maxEventBytes     = 16 * 1024
	maxSkew           = 5 * time.Minute
	dimensionMaxChars = 128
	tagValueMaxChars  = 256
	tagKeyMaxChars    = 128
	maxTags           = 50
)

var (
	eventID    = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
	regionForm = regexp.MustCompile(`^[a-z0-9-]*$`)
	fields     = []string{"specVersion", "id", "producer", "meter", "organizationId", "projectId", "resourceName", "region", "occurredAt", "window", "quantity", "dimensions", "tags", "correctionOf"}
	slugForm   = regexp.MustCompile(`^[a-z]{3}_[a-z0-9]{12}$`)
)

// UsageResourceName is a usage event's resource: a region and slugs, no /path.
type UsageResourceName struct {
	Region string
	Chain  []string
}

// ParseResourceName reads `ezgh:<region>:org_…[:…]`, without a /path: usage belongs to a
// resource, never to something inside one.
func ParseResourceName(name string) (UsageResourceName, bool) {
	parts := strings.Split(name, ":")
	if len(parts) < 3 || parts[0] != "ezgh" {
		return UsageResourceName{}, false
	}
	region, chain := parts[1], parts[2:]
	if !regionForm.MatchString(region) || !strings.HasPrefix(chain[0], "org_") {
		return UsageResourceName{}, false
	}
	for _, slug := range chain {
		if !slugForm.MatchString(slug) {
			return UsageResourceName{}, false
		}
	}
	return UsageResourceName{Region: region, Chain: chain}, true
}

// Validate checks an event given as generic JSON values (canonical.Decode's output) and returns
// every issue, in no particular order; none means it's valid.
func Validate(event any, o Options) []Issue {
	obj, isObject := event.(map[string]any)
	var issues []Issue
	if isObject {
		if id, ok := obj["meter"].(string); ok {
			if _, known := o.Meters[id]; !known {
				issues = append(issues, Issue{"meter", fmt.Sprintf("unknown_meter: %s isn't a current meter", jsString(id))})
			}
		}
	}
	if !isObject {
		return []Issue{{"", msgObject}}
	}
	if shape := shapeIssues(obj); len(shape) > 0 {
		return append(issues, shape...)
	}
	issues = append(issues, eventIssues(obj)...)
	var meter *Meter
	if m, ok := o.Meters[obj["meter"].(string)]; ok {
		meter = &m
	}
	return append(issues, contextIssues(obj, o, meter)...)
}

// ValidateEvent validates an event this service built.
func ValidateEvent(e *Event, o Options) ([]Issue, error) {
	generic, err := e.Generic()
	if err != nil {
		return nil, err
	}
	return Validate(generic, o), nil
}

func chars(s string) int { return utf8.RuneCountInString(s) }

func validTime(s string) bool {
	_, ok := ParseTime(s)
	return ok
}

// Stage one: each field's shape.
func shapeIssues(e map[string]any) []Issue {
	var issues []Issue
	add := func(path, message string) { issues = append(issues, Issue{path, message}) }
	str := func(key string, check func(string) bool, message string) {
		v, ok := e[key]
		if !ok {
			add(key, msgMissing)
			return
		}
		s, isString := v.(string)
		if !isString {
			add(key, msgString)
			return
		}
		if check != nil && !check(s) {
			add(key, message)
		}
	}
	strOrNull := func(key string, check func(string) bool, message string) {
		v, ok := e[key]
		if !ok {
			add(key, msgMissing)
			return
		}
		if v == nil {
			return
		}
		s, isString := v.(string)
		if !isString {
			add(key, msgStringOrNull)
			return
		}
		if !check(s) {
			add(key, message)
		}
	}

	if v, ok := e["specVersion"]; !ok {
		add("specVersion", msgMissing)
	} else if v != SpecVersion {
		add("specVersion", `Expected "1.0"`)
	}
	str("id", eventID.MatchString, msgID)
	str("producer", func(s string) bool { return s != "" }, msgNonEmpty)
	str("meter", func(s string) bool { return s != "" }, msgNonEmpty)
	str("organizationId", ids.IsUUID, msgUUID)
	strOrNull("projectId", ids.IsUUID, msgUUID)
	str("resourceName", func(s string) bool { _, ok := ParseResourceName(s); return ok }, msgResourceName)
	str("region", nil, "")
	if v, ok := e["occurredAt"]; ok {
		if s, isString := v.(string); !isString {
			add("occurredAt", msgString)
		} else if !validTime(s) {
			add("occurredAt", msgTime)
		}
	}
	if v, ok := e["window"]; ok {
		w, isObject := v.(map[string]any)
		if !isObject {
			add("window", msgObject)
		} else {
			for _, key := range []string{"start", "end"} {
				item, present := w[key]
				s, isString := item.(string)
				switch {
				case !present:
					add("window."+key, msgMissing)
				case !isString:
					add("window."+key, msgString)
				case !validTime(s):
					add("window."+key, msgTime)
				}
			}
			for key := range w {
				if key != "start" && key != "end" {
					add("window."+key, msgExcess)
				}
			}
		}
	}
	str("quantity", decimal.Valid, msgQuantity)
	if v, ok := e["dimensions"]; !ok {
		add("dimensions", msgMissing)
	} else if dims, isObject := v.(map[string]any); !isObject {
		add("dimensions", msgObject)
	} else {
		for key, item := range dims {
			s, isString := item.(string)
			if !isString {
				add("dimensions."+key, msgString)
			} else if chars(s) > dimensionMaxChars {
				add("dimensions."+key, fmt.Sprintf("Expected at most %d characters", dimensionMaxChars))
			}
		}
	}
	if v, ok := e["tags"]; ok {
		tags, isObject := v.(map[string]any)
		if !isObject {
			add("tags", msgObject)
		} else {
			before := len(issues)
			for key, item := range tags {
				s, isString := item.(string)
				if !isString {
					add("tags."+key, msgString)
				} else if chars(s) > tagValueMaxChars {
					add("tags."+key, fmt.Sprintf("Expected at most %d characters", tagValueMaxChars))
				}
			}
			// The tags' own rules, once every value is a string: a count, then each key's length.
			if len(issues) == before {
				if len(tags) > maxTags {
					add("tags", fmt.Sprintf("Expected at most %d tags", maxTags))
				} else {
					for key := range tags {
						if key == "" || chars(key) > tagKeyMaxChars {
							add("tags."+key, fmt.Sprintf("Expected a key of 1–%d characters", tagKeyMaxChars))
						}
					}
				}
			}
		}
	}
	strOrNull("correctionOf", eventID.MatchString, msgID)
	for key := range e {
		if !slices.Contains(fields, key) {
			add(key, msgExcess)
		}
	}
	return issues
}

// Stage two, part one: the rules across fields.
func eventIssues(e map[string]any) []Issue {
	var issues []Issue
	add := func(path, message string) { issues = append(issues, Issue{path, message}) }
	if b, err := canonical.Marshal(e); err == nil && len(b) > maxEventBytes {
		add("", fmt.Sprintf("Events are at most %d bytes as JSON", maxEventBytes))
	}
	_, hasInstant := e["occurredAt"]
	w, hasWindow := e["window"].(map[string]any)
	if hasInstant == hasWindow {
		add("", "Expected exactly one of occurredAt and window")
	}
	if hasWindow {
		start, _ := ParseTime(w["start"].(string))
		end, _ := ParseTime(w["end"].(string))
		if !start.Before(end) {
			add("window.end", "Expected end after start")
		} else if end.After(start.Truncate(time.Hour).Add(time.Hour)) {
			add("window.end", "Expected the window inside one UTC hour (end may be exactly the next hour)")
		}
	}
	quantity := decimal.MustParse(e["quantity"].(string))
	correctionOf, _ := e["correctionOf"].(string)
	if quantity.Sign() < 0 && e["correctionOf"] == nil {
		add("quantity", "Quantities are negative only in corrections (correctionOf)")
	}
	if e["correctionOf"] != nil && correctionOf == e["id"] {
		add("correctionOf", "A correction has an id of its own")
	}
	resource, _ := ParseResourceName(e["resourceName"].(string))
	region := e["region"].(string)
	if region != resource.Region {
		add("region", fmt.Sprintf("invalid_region: expected the resource name's region, %s", jsString(resource.Region)))
	}
	inProject := len(resource.Chain) > 1 && strings.HasPrefix(resource.Chain[1], "prj_")
	if inProject && e["projectId"] == nil {
		add("projectId", "unknown_project: the resource is in a project, so projectId is required")
	}
	if !inProject && e["projectId"] != nil {
		add("projectId", "unknown_project: the resource isn't in a project, so projectId must be null")
	}
	return issues
}

// Stage two, part two: the rules that need the meter, the clock or the configuration.
func contextIssues(e map[string]any, o Options, meter *Meter) []Issue {
	var issues []Issue
	add := func(path, message string) { issues = append(issues, Issue{path, message}) }
	w, hasWindow := e["window"].(map[string]any)
	occurredAt, hasInstant := e["occurredAt"].(string)
	var latest time.Time
	latestPath := ""
	if hasWindow {
		latest, _ = ParseTime(w["end"].(string))
		latestPath = "window.end"
	} else if hasInstant {
		latest, _ = ParseTime(occurredAt)
		latestPath = "occurredAt"
	}
	if latestPath != "" && latest.After(o.Now.Add(maxSkew)) {
		add(latestPath, "Expected no more than 5 minutes in the future")
	}
	producer := e["producer"].(string)
	if producer != o.Producer {
		add("producer", fmt.Sprintf("producer_mismatch: this credential reports as %s", jsString(o.Producer)))
	}
	region := e["region"].(string)
	if region != "" && !slices.Contains(o.Regions, region) {
		add("region", "invalid_region: expected one of "+strings.Join(o.Regions, ", "))
	}
	if tags, ok := e["tags"].(map[string]any); ok {
		for key := range tags {
			if strings.HasPrefix(key, "ezgh:") {
				add("tags."+key, "Tag keys starting with ezgh: are set by the system")
			}
		}
	}

	if meter == nil {
		return issues
	}
	// Deprecated meters still take events: producers switch at an hour boundary, and usage from
	// before the switch keeps arriving until the late-data cutoff. Only retired ones are refused.
	if meter.Status == "retired" {
		add("meter", fmt.Sprintf("unknown_meter: %s isn't a current meter", jsString(e["meter"].(string))))
		return issues
	}
	if !slices.Contains(meter.Producers, producer) {
		add("meter", fmt.Sprintf("meter_not_owned: %s doesn't report %s", jsString(producer), meter.ID))
	}
	if meter.Kind == Interval && !hasWindow {
		add("window", "Interval meters report a window")
	}
	if meter.Kind == Gauge && !hasInstant {
		add("occurredAt", "Gauge meters report an instant (occurredAt)")
	}
	quantity := decimal.MustParse(e["quantity"].(string))
	if meter.Kind == Gauge && quantity.Sign() < 0 {
		add("quantity", "A gauge level can't be negative")
	}
	// A gauge's correction replaces a sample under an ID of its own: <original id>:c<n>.
	if correctionOf, ok := e["correctionOf"].(string); ok && meter.Kind == Gauge {
		id := e["id"].(string)
		n := ""
		if strings.HasPrefix(id, correctionOf+":c") {
			n = id[len(correctionOf)+2:]
		}
		if !positiveInteger.MatchString(n) {
			add("id", "A gauge correction's id is <original id>:c<n>")
		}
	}
	resource, _ := ParseResourceName(e["resourceName"].(string))
	if !strings.HasPrefix(resource.Chain[len(resource.Chain)-1], meter.ResourceType+"_") {
		add("resourceName", fmt.Sprintf("Expected a %s_ resource", meter.ResourceType))
	}
	if meter.Global && region != "" {
		add("region", `invalid_region: this meter is global, so region is ""`)
	}
	if !meter.Global && region == "" {
		add("region", "invalid_region: this meter is regional")
	}
	if quantity.Cmp(decimal.MustParse(meter.MaxQuantity)) > 0 {
		add("quantity", fmt.Sprintf("Expected at most %s per event", meter.MaxQuantity))
	}
	dims := e["dimensions"].(map[string]any)
	for key, item := range dims {
		value := item.(string)
		spec, declared := meter.Dimensions[key]
		if !declared {
			add("dimensions."+key, fmt.Sprintf("invalid_dimension: %s has no dimension %s", meter.ID, jsString(key)))
			continue
		}
		if spec.Values != nil && !slices.Contains(spec.Values, value) {
			add("dimensions."+key, "invalid_dimension: expected one of "+strings.Join(spec.Values, ", "))
		}
		if spec.Pattern != nil {
			re, err := regexp.Compile(*spec.Pattern)
			if err != nil || !re.MatchString(value) {
				add("dimensions."+key, "invalid_dimension: expected a value matching "+*spec.Pattern)
			}
		}
	}
	for key, spec := range meter.Dimensions {
		if _, present := dims[key]; spec.Required && !present {
			add("dimensions."+key, "invalid_dimension: required")
		}
	}
	return issues
}

var positiveInteger = regexp.MustCompile(`^[1-9]\d*$`)

// jsString is JSON.stringify of a string, as the messages quote values.
func jsString(s string) string {
	b, err := canonical.Marshal(s)
	if err != nil {
		b, _ = json.Marshal(s)
	}
	return string(b)
}
