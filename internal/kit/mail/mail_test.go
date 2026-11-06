package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestAndErrors(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/acc/email/sending/send" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(429)
		w.Write([]byte(`{"success":false,"errors":[{"code":10004,"message":"email.sending.error.throttled"}]}`))
	}))
	defer srv.Close()
	m := &Mailer{APIURL: srv.URL, AccountID: "acc", Token: "tok", From: "EZGH Cloud <no-reply@mail.ezghcloud.com>", HTTP: srv.Client()}
	status, err := m.Deliver(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t", HTML: "h",
		Headers: map[string]string{"List-Unsubscribe": "<https://x>"}})
	var me *Error
	if status != 429 || !errors.As(err, &me) || !me.Retryable || strings.Contains(err.Error(), "tok") || !strings.Contains(err.Error(), "10004") {
		t.Fatalf("status %d err %v", status, err)
	}
	from := body["from"].(map[string]any)
	if from["address"] != "no-reply@mail.ezghcloud.com" || from["name"] != "EZGH Cloud" {
		t.Fatalf("from %v", from)
	}
	if body["headers"].(map[string]any)["List-Unsubscribe"] != "<https://x>" {
		t.Fatalf("headers %v", body["headers"])
	}
	if body["to"] != "a@example.com" {
		t.Fatalf("one recipient must be a string on the wire: %#v", body["to"])
	}
	if err := m.Send(context.Background(), []string{"a@example.com", "b@example.com"}, "s", "t", "h"); err == nil {
		t.Fatal("Send must surface the refusal")
	}
	if to := body["to"].([]any); len(to) != 2 {
		t.Fatalf("to %v", to)
	}
}

func TestPermanentBounceAndStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"result":{"permanent_bounces":["a@example.com"]}}`))
	}))
	m := &Mailer{APIURL: srv.URL, AccountID: "acc", Token: "tok", From: "a@b.c", HTTP: srv.Client()}
	var me *Error
	if err := m.Send(context.Background(), []string{"a@example.com"}, "s", "t", "h"); !errors.As(err, &me) || me.Retryable {
		t.Fatalf("bounce: %v", err)
	}
	srv.Close()
	for status, retry := range map[int]bool{400: false, 500: true, 409: true} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		m.APIURL, m.HTTP = s.URL, s.Client()
		err := m.Send(context.Background(), []string{"a@example.com"}, "s", "t", "h")
		s.Close()
		if !errors.As(err, &me) || me.Retryable != retry {
			t.Errorf("%d: %v", status, err)
		}
	}
}
