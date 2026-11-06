package orgfeed_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/orgfeed"
)

const cred = "ok_abcdefghijklmnop.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestEventsAndErrors(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cred || r.URL.Path != "/internal/v1/lifecycle-events" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("after") != "41" || r.URL.Query().Get("limit") != "500" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		switch status {
		case http.StatusOK:
			_, _ = w.Write([]byte(`{"events":[{"seq":42,"kind":"bot.deleted","organizationId":"o","projectId":null,"principal":{"type":"bot","id":"b"},"occurredAt":"2026-10-05T00:00:00.000Z"},
				{"seq":43,"kind":"project.deleted","organizationId":"o","projectId":"p","principal":null,"occurredAt":"2026-10-05T00:00:01.000Z"}],"head":43,"asOf":"2026-10-05T00:00:02.000Z"}`))
		case http.StatusTooManyRequests:
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(status)
		default:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"feed_expired","message":"…"}}`))
		}
	}))
	defer srv.Close()
	c := orgfeed.New(srv.URL, cred, nil)
	ctx := context.Background()
	page, err := c.Events(ctx, "41", 500)
	if err != nil || len(page.Events) != 2 || page.Head != 43 || page.Events[0].Principal.ID != "b" || *page.Events[1].ProjectID != "p" ||
		!page.AsOf.Equal(time.Date(2026, 10, 5, 0, 0, 2, 0, time.UTC)) {
		t.Fatalf("%+v %v", page, err)
	}
	status = http.StatusGone
	if _, err := c.Events(ctx, "41", 500); !errors.Is(err, orgfeed.ErrFeedExpired) {
		t.Fatalf("410: %v", err)
	}
	status = http.StatusTooManyRequests
	var e *orgfeed.Error
	if _, err := c.Events(ctx, "41", 500); !errors.As(err, &e) || !errors.Is(err, orgfeed.ErrRateLimited) || e.RetryAfter != 9*time.Second {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusBadGateway
	if _, err := c.Events(ctx, "41", 500); !errors.Is(err, orgfeed.ErrUnavailable) {
		t.Fatalf("502: %v", err)
	}
	if _, err := orgfeed.New(srv.URL, "ok_wrong", nil).Events(ctx, "41", 500); !errors.Is(err, orgfeed.ErrDenied) {
		t.Fatalf("401: %v", err)
	}
	if _, err := c.Events(ctx, "41", 1001); !errors.Is(err, orgfeed.ErrInvalid) {
		t.Fatalf("limit: %v", err)
	}
}

func TestExists(t *testing.T) {
	var reorder bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in orgfeed.ExistsRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || r.Method != http.MethodPost || r.URL.Path != "/internal/v1/principals/exists" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		yes, no := true, false
		out := orgfeed.ExistsAnswer{OrganizationExists: true}
		for i, p := range in.Principals {
			var ex *bool
			switch i % 3 {
			case 0:
				ex = &yes
			case 1:
				ex = &no
			}
			out.Principals = append(out.Principals, orgfeed.PrincipalExists{Type: p.Type, ID: p.ID, Exists: ex})
		}
		for _, p := range in.Projects {
			out.Projects = append(out.Projects, orgfeed.ProjectExists{ID: p, Exists: true})
		}
		if reorder && len(out.Principals) > 1 {
			out.Principals[0], out.Principals[1] = out.Principals[1], out.Principals[0]
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := orgfeed.New(srv.URL, cred, nil)
	ctx := context.Background()
	in := orgfeed.ExistsRequest{OrganizationID: "o", Principals: []orgfeed.Principal{{"user", "a"}, {"bot", "b"}, {"ssoUser", "c"}}, Projects: []string{"p"}}
	got, err := c.Exists(ctx, in)
	if err != nil || !*got.Principals[0].Exists || *got.Principals[1].Exists || got.Principals[2].Exists != nil || !got.Projects[0].Exists {
		t.Fatalf("%+v %v", got, err)
	}
	reorder = true
	if _, err := c.Exists(ctx, in); !errors.Is(err, orgfeed.ErrUnavailable) {
		t.Fatalf("out of order: %v", err)
	}
	many := orgfeed.ExistsRequest{OrganizationID: "o", Projects: make([]string, 101)}
	if _, err := c.Exists(ctx, many); !errors.Is(err, orgfeed.ErrInvalid) {
		t.Fatalf("101 projects: %v", err)
	}
}

func TestEventsErrorKinds(t *testing.T) {
	for status, want := range map[int]error{410: orgfeed.ErrFeedExpired, 429: orgfeed.ErrRateLimited, 401: orgfeed.ErrDenied, 403: orgfeed.ErrDenied, 400: orgfeed.ErrInvalid, 503: orgfeed.ErrUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		_, err := orgfeed.New(srv.URL, cred, nil).Events(context.Background(), "latest", 10)
		srv.Close()
		if !errors.Is(err, want) {
			t.Fatalf("%d: %v", status, err)
		}
	}
}

func TestEventsRefusesNonIncreasingSeq(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"events":[{"seq":5,"kind":"bot.deleted","organizationId":"o","occurredAt":"2026-10-09T00:00:00.000Z"},{"seq":5,"kind":"bot.deleted","organizationId":"o","occurredAt":"2026-10-09T00:00:00.000Z"}],"head":5,"asOf":"2026-10-09T00:00:01.000Z"}`))
	}))
	defer srv.Close()
	if _, err := orgfeed.New(srv.URL, cred, nil).Events(context.Background(), "4", 10); !errors.Is(err, orgfeed.ErrUnavailable) {
		t.Fatalf("repeated seq: %v", err)
	}
}

func TestEventsWithKindsSendsTheFilter(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("kinds")
		_, _ = w.Write([]byte(`{"events":[{"seq":8,"kind":"project.created","organizationId":"o","projectId":"p","principal":null,"occurredAt":"2026-10-14T00:00:00.000Z"}],"head":9,"asOf":"2026-10-14T00:00:01.000Z"}`))
	}))
	defer srv.Close()
	plain := orgfeed.New(srv.URL, cred, nil)
	page, err := plain.WithKinds(orgfeed.ProjectCreated, orgfeed.ProjectDeleted).Events(context.Background(), "7", 100)
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != orgfeed.ProjectCreated || *page.Events[0].ProjectID != "p" || page.Head != 9 {
		t.Fatalf("%+v %v", page, err)
	}
	if got != "project.created,project.deleted" {
		t.Fatalf("kinds = %q", got)
	}
	// The receiver keeps reading the default kinds.
	if _, err := plain.Events(context.Background(), "7", 100); err != nil || got != "" {
		t.Fatalf("plain client after WithKinds: kinds = %q, %v", got, err)
	}
}

// Consumers (registry-stack, workflows-stack) hold the client behind this interface; a filtered
// client must still fit it.
var _ interface {
	Events(ctx context.Context, after string, limit int) (*orgfeed.Page, error)
	Exists(ctx context.Context, in orgfeed.ExistsRequest) (*orgfeed.ExistsAnswer, error)
} = orgfeed.New("http://org-api.test", cred, nil).WithKinds(orgfeed.ProjectCreated)

func TestEventsWithoutKindsSendsNoFilter(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.URL.Query()["kinds"]
		_, _ = w.Write([]byte(`{"events":[],"head":7,"asOf":"2026-10-14T00:00:00.000Z"}`))
	}))
	defer srv.Close()
	if _, err := orgfeed.New(srv.URL, cred, nil).Events(context.Background(), "latest", 100); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("kinds sent without WithKinds")
	}
}

func TestEventsWithKindsRefusesBadFiltersAndUnaskedKinds(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// An org-api that ignores kinds= answers the deletion kinds.
		_, _ = w.Write([]byte(`{"events":[{"seq":8,"kind":"bot.deleted","organizationId":"o","projectId":null,"principal":{"type":"bot","id":"b"},"occurredAt":"2026-10-14T00:00:00.000Z"}],"head":8,"asOf":"2026-10-14T00:00:01.000Z"}`))
	}))
	defer srv.Close()
	c, ctx := orgfeed.New(srv.URL, cred, nil), context.Background()
	for _, kinds := range [][]string{nil, {""}, {"project.created,bot.deleted"}} {
		if _, err := c.WithKinds(kinds...).Events(ctx, "7", 100); !errors.Is(err, orgfeed.ErrInvalid) {
			t.Fatalf("WithKinds(%q): %v", kinds, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d calls for refused filters", calls)
	}
	if _, err := c.WithKinds(orgfeed.ProjectCreated).Events(ctx, "7", 100); !errors.Is(err, orgfeed.ErrUnavailable) {
		t.Fatalf("an event of a kind not asked for: %v", err)
	}
	if _, err := c.WithKinds(orgfeed.ProjectCreated).WithKinds(orgfeed.BotDeleted).Events(ctx, "7", 100); err != nil {
		t.Fatalf("a second WithKinds replaces the first: %v", err)
	}
}
