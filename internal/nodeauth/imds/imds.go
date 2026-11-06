// Package imds is the node's client of the EZGH instance metadata service (compute-stack
// internal/hostd/metadata): http://169.254.169.254, else http://[fd65:7a67:6800::254] when IPv4
// can't connect. Every request carries EZGH-Metadata: true and a session token, goes through no
// proxy, follows no redirect, and reads at most 1 MiB. ezgh-vpc-cni, ezgh-node-auth and
// ezgh-credential-provider share it, so it imports no Kubernetes package.
package imds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	DefaultBase4 = "http://169.254.169.254"
	DefaultBase6 = "http://[fd65:7a67:6800::254]"
	tokenTTL     = 21600 * time.Second
	readCap      = 1 << 20
)

var (
	ErrNoBot      = errors.New("imds: this instance has no Bot (no_bot)")
	ErrNoUserData = errors.New("imds: this instance has no user data")
)

// Error is a metadata answer other than 2xx. Error() leaves out Message: callers print errors,
// and a message is free text that could carry a key.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("imds: metadata answered %d %s", e.Status, e.Code)
}

// Client talks to the metadata service. The zero value uses the defaults; one per process.
type Client struct {
	HTTP         *http.Client // nil: HTTPClient()
	Base4, Base6 string       // "": DefaultBase4, DefaultBase6

	mu       sync.Mutex
	token    string
	tokenEnd time.Time
}

// HTTPClient is the metadata client's transport: no proxy, no redirects, a 1 s connect and a 5 s
// total timeout.
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: time.Second}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("imds: metadata redirects aren't followed")
		},
	}
}

type Instance struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Project    string      `json:"project"`
	ProjectID  string      `json:"projectId"`
	Region     string      `json:"region"`
	Interfaces []Interface `json:"interfaces"`
}

type Interface struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	MAC         string  `json:"mac"`
	IPv6Address string  `json:"ipv6Address"`
	DeviceIndex *int    `json:"deviceIndex"`
	MTU         int     `json:"mtu"`
	Subnet      *Subnet `json:"subnet"`
	PrivateIPv4 *string `json:"privateIpv4"`
	// DelegatedPrefixes is nil while metadata doesn't know (null): never "none".
	DelegatedPrefixes *[]Ownership `json:"delegatedPrefixes"`
}

type Subnet struct {
	IPv4CIDR string `json:"ipv4Cidr"`
	IPv6CIDR string `json:"ipv6Cidr"`
}

// Ownership is a delegated prefix at its ownership generation (a decimal string).
type Ownership struct {
	Prefix     string `json:"prefix"`
	Generation string `json:"generation"`
}

// Credentials is a fresh key for the instance's Bot. Use it for one call and drop it.
type Credentials struct {
	APIKey    string    `json:"apiKey"`
	Bot       string    `json:"bot"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (c Credentials) String() string {
	return fmt.Sprintf("{apiKey:[REDACTED] bot:%s expiresAt:%s}", c.Bot, c.ExpiresAt.Format(time.RFC3339))
}
func (c Credentials) GoString() string { return c.String() }

func (c *Client) Instance(ctx context.Context) (Instance, error) {
	var in Instance
	b, err := c.get(ctx, "/ezgh/v1/instance")
	if err == nil {
		err = json.Unmarshal(b, &in)
	}
	return in, err
}

// Credentials answers ErrNoBot when the instance has no Bot.
func (c *Client) Credentials(ctx context.Context) (Credentials, error) {
	var cr Credentials
	b, err := c.get(ctx, "/ezgh/v1/credentials")
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "no_bot" {
		return cr, ErrNoBot
	}
	if err != nil {
		return cr, err
	}
	if err := json.Unmarshal(b, &cr); err != nil {
		return Credentials{}, errors.New("imds: credentials aren't JSON")
	}
	return cr, nil
}

// Identity is a compute-signed identity token (JWT) for audience.
func (c *Client) Identity(ctx context.Context, audience string) (string, error) {
	b, err := c.get(ctx, "/ezgh/v1/identity?audience="+url.QueryEscape(audience))
	if err != nil {
		return "", err
	}
	var r struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &r) != nil || r.Token == "" {
		return "", errors.New("imds: no identity token in the answer")
	}
	return r.Token, nil
}

// UserData answers ErrNoUserData when the instance has none.
func (c *Client) UserData(ctx context.Context) ([]byte, error) {
	b, err := c.get(ctx, "/ezgh/v1/user-data")
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil, ErrNoUserData
	}
	return b, err
}

// get reads path with the session token, PUTting a new one once on 401.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.sessionToken(ctx)
		if err != nil {
			return nil, err
		}
		b, err := c.do(ctx, http.MethodGet, path, map[string]string{"EZGH-Metadata-Token": token})
		var e *Error
		if attempt == 0 && errors.As(err, &e) && e.Status == http.StatusUnauthorized {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}
		return b, err
	}
}

// sessionToken is kept in memory until 60 s before its TTL.
func (c *Client) sessionToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenEnd) {
		return c.token, nil
	}
	start := time.Now()
	b, err := c.do(ctx, http.MethodPut, "/ezgh/v1/token", map[string]string{"EZGH-Metadata-Token-TTL-Seconds": "21600"})
	if err != nil {
		return "", err
	}
	c.token, c.tokenEnd = strings.TrimSpace(string(b)), start.Add(tokenTTL-time.Minute)
	return c.token, nil
}

// do tries IPv4, then IPv6 only when IPv4 couldn't connect: any IPv4 answer is final.
func (c *Client) do(ctx context.Context, method, path string, header map[string]string) ([]byte, error) {
	base4, base6 := c.Base4, c.Base6
	if base4 == "" {
		base4 = DefaultBase4
	}
	if base6 == "" {
		base6 = DefaultBase6
	}
	b, err := c.once(ctx, method, base4+path, header)
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		b, err = c.once(ctx, method, base6+path, header)
	}
	return b, err
}

func (c *Client) once(ctx context.Context, method, url string, header map[string]string) ([]byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("EZGH-Metadata", "true")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, readCap+1))
	if err != nil {
		return nil, err
	}
	if len(b) > readCap {
		return nil, errors.New("imds: metadata answer over 1 MiB")
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(b, &body) == nil {
			e.Code, e.Message = body.Error.Code, body.Error.Message
		}
		return nil, e
	}
	return b, nil
}

var defaultHTTP = HTTPClient()
