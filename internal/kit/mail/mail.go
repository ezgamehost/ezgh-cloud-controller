// Package mail sends transactional email through Cloudflare Email Sending.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultAPIURL is Cloudflare's REST API base.
const DefaultAPIURL = "https://api.cloudflare.com/client/v4"

// Message is one email. From empty means the Mailer's From.
type Message struct {
	From, Subject, Text, HTML string
	To                        []string
	Headers                   map[string]string // List-Unsubscribe, ...
}

// Send sends one email from the Mailer's From. Failures are *Error.
func (c *Mailer) Send(ctx context.Context, to []string, subject, text, html string) error {
	_, err := c.Deliver(ctx, Message{To: to, Subject: subject, Text: text, HTML: html})
	return err
}

// Error says why an email wasn't sent. Retryable: the same email could go through later (no
// answer, 409 "setup still in progress", 429, 5xx). It never carries the token, the body or links.
type Error struct {
	Status    int
	Codes     []string
	Retryable bool
	msg       string
}

func (e *Error) Error() string { return e.msg }

// Mailer sends through Cloudflare Email Sending: POST
// {APIURL}/accounts/{AccountID}/email/sending/send, bearer token, 10 s timeout,
// body {"from":{"address","name"},"to","subject","text","html","headers"}.
type Mailer struct {
	APIURL, AccountID, Token, From string // APIURL empty: DefaultAPIURL
	HTTP                           *http.Client
}

var namedAddress = regexp.MustCompile(`^\s*"?([^"<]*?)"?\s*<\s*([^<>\s]+@[^<>\s]+)\s*>\s*$`)

type cfAddress struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

// parseAddress turns `EZGH Cloud Alerts <alerts@alerts.ezghcloud.com>` into {address, name}.
func parseAddress(v string) cfAddress {
	if m := namedAddress.FindStringSubmatch(v); m != nil {
		return cfAddress{Address: m[2], Name: m[1]}
	}
	return cfAddress{Address: strings.TrimSpace(v)}
}

type cfAnswer struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    any `json:"code"`
		Message any `json:"message"`
	} `json:"errors"`
	Result *struct {
		PermanentBounces []any `json:"permanent_bounces"`
	} `json:"result"`
}

// Deliver posts m and returns the provider's HTTP status (0 when it never answered). Success is HTTP 2xx and success: true; a non-empty result.permanent_bounces is a
// permanent failure.
func (c *Mailer) Deliver(ctx context.Context, m Message) (int, error) {
	base := c.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	endpoint := strings.TrimRight(base, "/") + "/accounts/" + url.PathEscape(c.AccountID) + "/email/sending/send"
	from := m.From
	if from == "" {
		from = c.From
	}
	req := map[string]any{"from": parseAddress(from), "to": toField(m.To), "subject": m.Subject, "text": m.Text, "html": m.HTML}
	if len(m.Headers) > 0 {
		req["headers"] = m.Headers
	}
	body, err := json.Marshal(req)
	if err != nil {
		return 0, &Error{msg: "the email couldn't be encoded"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, &Error{msg: "the Cloudflare Email Sending request couldn't be built"}
	}
	hr.Header.Set("Authorization", "Bearer "+c.Token)
	hr.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		msg := "couldn't reach Cloudflare Email Sending"
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			msg = "Cloudflare Email Sending didn't answer in 10 s"
		}
		return 0, &Error{Retryable: true, msg: msg}
	}
	defer resp.Body.Close()
	var answer cfAnswer
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
	var codes []string
	for _, e := range answer.Errors {
		var parts []string
		for _, v := range []any{e.Code, e.Message} {
			switch v := v.(type) {
			case string:
				parts = append(parts, v)
			case float64:
				parts = append(parts, fmt.Sprintf("%.0f", v))
			}
		}
		if len(parts) > 0 {
			codes = append(codes, strings.Join(parts, " "))
		}
	}
	status := resp.StatusCode
	if status < 200 || status >= 300 || !answer.Success {
		msg := fmt.Sprintf("Cloudflare Email Sending refused the email: %d", status)
		if len(codes) > 0 {
			msg += " (" + strings.Join(codes, ", ") + ")"
		}
		return status, &Error{Status: status, Codes: codes, msg: msg,
			Retryable: status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500}
	}
	if answer.Result != nil && len(answer.Result.PermanentBounces) > 0 {
		return status, &Error{Status: status, msg: "the recipient's server permanently refused the email"}
	}
	return status, nil
}

// toField keeps the wire shape every caller has always sent: a string for one recipient, an array for
// several.
func toField(to []string) any {
	if len(to) == 1 {
		return to[0]
	}
	return to
}
