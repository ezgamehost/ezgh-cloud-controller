package resourceurl_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resourceurl"
)

// contracts/vectors/resource-binding.json: every canonicalization and every requestUrl case.
// match and order are org-api's (an exact comparison it never canonicalizes); a kit only sends
// requestUrl.
func TestResourceBindingVectors(t *testing.T) {
	var file struct {
		Canonicalize []struct {
			Name      string  `json:"name"`
			Input     string  `json:"input"`
			Canonical *string `json:"canonical"`
		} `json:"canonicalize"`
		RequestURLs []struct {
			Name          string            `json:"name"`
			PublicBaseURL string            `json:"publicBaseUrl"`
			RequestTarget string            `json:"requestTarget"`
			Headers       map[string]string `json:"headers"`
			RequestURL    *string           `json:"requestUrl"`
		} `json:"requestUrls"`
	}
	vectors.Load(t, "resource-binding.json", &file)
	if len(file.Canonicalize) < 60 || len(file.RequestURLs) < 15 {
		t.Fatalf("only %d canonicalize and %d requestUrls cases", len(file.Canonicalize), len(file.RequestURLs))
	}
	for _, c := range file.Canonicalize {
		got, ok := resourceurl.Canonical(c.Input)
		switch {
		case c.Canonical == nil && ok:
			t.Errorf("canonicalize %q: accepted as %q", c.Name, got)
		case c.Canonical != nil && (!ok || got != *c.Canonical):
			t.Errorf("canonicalize %q: %q %v, want %q", c.Name, got, ok, *c.Canonical)
		}
		// The canonical form is a fixed point.
		if ok {
			if again, ok := resourceurl.Canonical(got); !ok || again != got {
				t.Errorf("canonicalize %q: %q isn't canonical itself (%q %v)", c.Name, got, again, ok)
			}
		}
	}
	for _, c := range file.RequestURLs {
		// Headers never contribute: RequestURL doesn't take them.
		got, ok := resourceurl.RequestURL(c.PublicBaseURL, c.RequestTarget)
		switch {
		case c.RequestURL == nil && ok:
			t.Errorf("requestUrls %q: built %q", c.Name, got)
		case c.RequestURL != nil && (!ok || got != *c.RequestURL):
			t.Errorf("requestUrls %q: %q %v, want %q", c.Name, got, ok, *c.RequestURL)
		}
	}
}

func TestOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://widgets.ezghcloud.com":        "https://widgets.ezghcloud.com",
		"https://widgets.ezghcloud.com/":       "https://widgets.ezghcloud.com",
		"HTTPS://Widgets.EzghCloud.com:443":    "https://widgets.ezghcloud.com",
		"https://widgets.ezghcloud.localhost/": "https://widgets.ezghcloud.localhost",
		"http://widgets.ezghcloud.com":         "",
		"https://widgets.ezghcloud.com:8443":   "",
		"https://widgets.ezghcloud.com/v1":     "",
		"https://widgets.ezghcloud.com//":      "",
		"https://widgets.ezghcloud.com/?":      "",
		"https://ada@widgets.ezghcloud.com":    "",
		"widgets.ezghcloud.com":                "",
		"":                                     "",
	} {
		got, ok := resourceurl.Origin(in)
		if got != want || ok != (want != "") {
			t.Errorf("Origin(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}
