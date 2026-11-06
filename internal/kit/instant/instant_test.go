package instant_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/instants.json, the response form. (The usage form is package usage's.)
func TestResponseInstantVectors(t *testing.T) {
	var file struct {
		Response struct {
			Valid       []string `json:"valid"`
			Invalid     []string `json:"invalid"`
			FromEpochMs []struct {
				Ms      int64  `json:"ms"`
				Instant string `json:"instant"`
			} `json:"fromEpochMs"`
		} `json:"response"`
	}
	vectors.Load(t, "instants.json", &file)
	for _, s := range file.Response.Valid {
		if _, ok := instant.ParseResponse(s); !ok {
			t.Errorf("refused %s", s)
		}
	}
	for _, s := range file.Response.Invalid {
		if _, ok := instant.ParseResponse(s); ok {
			t.Errorf("accepted %s", s)
		}
	}
	for _, c := range file.Response.FromEpochMs {
		if got := instant.Format(instant.FromMillis(c.Ms)); got != c.Instant {
			t.Errorf("%d → %s, want %s", c.Ms, got, c.Instant)
		}
	}
}

func TestParseRequest(t *testing.T) {
	got, ok := instant.ParseRequest("2026-09-28T12:42:17+02:00")
	if !ok || instant.Format(got) != "2026-09-28T10:42:17.000Z" {
		t.Errorf("got %v %v", got, ok)
	}
	for _, bad := range []string{"2026-09-28T10:42:17.0001Z", "2026-09-28 10:42:17Z", "yesterday"} {
		if _, ok := instant.ParseRequest(bad); ok {
			t.Errorf("accepted %s", bad)
		}
	}
}
