// Package events turns EZGH API errors into Kubernetes events on the object that caused them, so a
// customer sees why a Service, Ingress or PVC isn't converging. Controllers
// requeue with backoff and never exit on an API error.
package events

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// Event reasons.
const (
	MissingPermission      = "MissingPermission"
	QuotaExceeded          = "QuotaExceeded"
	CredentialsUnavailable = "CredentialsUnavailable"
	EZGHAPIError           = "EZGHAPIError"
	UnsupportedProtocol    = "UnsupportedProtocol"
	TooManyListeners       = "TooManyListeners"
	TargetLimit            = "TargetLimit"
	InvalidAnnotation      = "InvalidAnnotation"
	NoMountTarget          = "NoMountTarget"
)

// Describe is err's event reason and message, at most maxNote bytes.
func Describe(err error) (reason, message string) {
	reason, message = describe(err)
	if len(message) > maxNote {
		message = strings.ToValidUTF8(message[:maxNote-3], "") + "..."
	}
	return reason, message
}

// maxNote is under events.k8s.io/v1's 1 KiB limit on an event's note.
const maxNote = 1000

func describe(err error) (reason, message string) {
	if p, ok := ezghclient.AsPermission(err); ok {
		if p.Action == "" {
			return MissingPermission, "The cluster's Bot was refused: " + p.Message + ". Grant the permission on the Bot, or attach KubernetesClusterAccess."
		}
		return MissingPermission, "The cluster's Bot isn't allowed to " + p.Action + ". Grant it on the Bot, or attach KubernetesClusterAccess."
	}
	if _, msg, ok := ezghclient.AsQuota(err); ok {
		return QuotaExceeded, msg
	}
	if c, ok := ezghclient.AsCredentials(err); ok {
		return CredentialsUnavailable, c.Error()
	}
	if e, ok := apiclient.AsError(err); ok && e.Status == http.StatusUnauthorized {
		// Refused even after a fresh key: the cluster's Bot or its keys are gone.
		return CredentialsUnavailable, fmt.Sprintf("%s refused the cluster's credentials: %s", e.Service, e.Error())
	}
	if e, ok := apiclient.AsError(err); ok {
		msg := fmt.Sprintf("%s answered %d: %s", e.Service, e.Status, e.Error())
		var issues []string
		for _, is := range e.Issues {
			issues = append(issues, strings.TrimSpace(is.Path+" "+is.Message))
		}
		if len(issues) > 0 {
			msg += ": " + strings.Join(issues, "; ")
		}
		return EZGHAPIError, msg
	}
	var t *apiclient.TransportError
	if errors.As(err, &t) {
		return EZGHAPIError, t.Error()
	}
	return EZGHAPIError, err.Error()
}

// Report records err as a Warning event on obj and returns how long its answer asked to wait (0
// leaves the delay to the workqueue's rate limiter).
func Report(rec events.EventRecorder, obj runtime.Object, err error) time.Duration {
	if err == nil {
		return 0
	}
	reason, message := Describe(err)
	rec.Eventf(obj, nil, "Warning", reason, "Reconcile", "%s", message)
	return ezghclient.RetryAfter(err)
}

// Warn records a controller's own refusal (an annotation it can't use, a port it can't serve).
func Warn(rec events.EventRecorder, obj runtime.Object, reason, format string, args ...any) {
	rec.Eventf(obj, nil, "Warning", reason, "Reconcile", format, args...)
}
