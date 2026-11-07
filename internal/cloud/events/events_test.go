package events

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

func report(t *testing.T, err error) (string, time.Duration) {
	t.Helper()
	rec := events.NewFakeRecorder(1)
	d := Report(rec, &corev1.Service{}, err)
	return <-rec.Events, d
}

func TestReportPermission(t *testing.T) {
	ev, _ := report(t, &apiclient.Error{Service: "Load Balancers", Status: 403, Code: "access_denied", Message: "This API key isn't allowed to lb.loadBalancers.create"})
	if !strings.HasPrefix(ev, "Warning MissingPermission ") || !strings.Contains(ev, "lb.loadBalancers.create") || !strings.Contains(ev, "KubernetesClusterAccess") {
		t.Fatal(ev)
	}
}

func TestReportCredentials(t *testing.T) {
	ev, d := report(t, &ezghclient.CredentialsError{Err: errString("k8sagent answered 503"), RetryAfter: 3 * time.Second})
	if !strings.HasPrefix(ev, "Warning CredentialsUnavailable ") || d != 3*time.Second {
		t.Fatal(ev, d)
	}
}

func TestReportQuota(t *testing.T) {
	ev, _ := report(t, &apiclient.Error{Status: 409, Code: "quota_exceeded", Message: "This project has 2 load balancers, its limit"})
	if ev != "Warning QuotaExceeded This project has 2 load balancers, its limit" {
		t.Fatal(ev)
	}
}

func TestReportRetryAfter(t *testing.T) {
	ev, d := report(t, &apiclient.Error{Service: "Load Balancers", Status: 429, Code: "too_many_requests", Message: "Slow down", RetryAfter: 30 * time.Second})
	if !strings.HasPrefix(ev, "Warning EZGHAPIError Load Balancers answered 429") || d != 30*time.Second {
		t.Fatal(ev, d)
	}
}

// A refused body says which field and why: the Certificates import's 400 names it.
func TestReportIssues(t *testing.T) {
	ev, _ := report(t, &apiclient.Error{Service: "Certificates", Status: 400, Code: "invalid_request", Message: "Invalid request", Issues: []apiclient.Issue{{Path: "certificate", Message: "must name 1-100 DNS names"}}})
	if !strings.HasSuffix(ev, "Certificates answered 400: Invalid request (invalid_request): certificate must name 1-100 DNS names") {
		t.Fatal(ev)
	}
}

// A long answer is cut to what an event's note takes.
func TestReportLong(t *testing.T) {
	ev, _ := report(t, &apiclient.Error{Service: "Load Balancers", Status: 400, Code: "invalid_request", Message: strings.Repeat("x", 2000)})
	if len(ev) > 1100 {
		t.Fatal(len(ev))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
