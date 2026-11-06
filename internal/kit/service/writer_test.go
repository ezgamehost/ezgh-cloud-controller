package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *deadlineWriter) SetReadDeadline(deadline time.Time) error { w.deadline = deadline; return nil }

// Private streaming handlers must be able to bound reads through the access-log wrapper.
func TestResponseControllerReachesReadDeadline(t *testing.T) {
	underlying := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	wrapped := &statusWriter{ResponseWriter: underlying}
	deadline := time.Now().Add(time.Minute)
	if err := http.NewResponseController(wrapped).SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if !underlying.deadline.Equal(deadline) {
		t.Fatalf("read deadline was not forwarded: %v", underlying.deadline)
	}
}
