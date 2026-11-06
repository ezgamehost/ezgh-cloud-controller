// Package redis is the smallest Redis client the kit needs: commands over RESP2, for rate limits,
// for services whose counters live in Redis (Quotas), and [being built] pub/sub (Subscribe,
// Publish), for change notifications such as Feature Flags' flags:changes. Callers decide what a failure means (rate
// limits fail open). The client keeps a small pool of connections, so concurrent requests don't
// queue behind one another; it dials on demand and drops a connection on any error that isn't a
// Redis error reply.
package redis

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to one Redis server.
type Client struct {
	addr     string
	password string
	username string
	db       int
	// Timeout bounds each command (connecting included), unless the context's deadline is
	// sooner. Default 500 ms.
	Timeout time.Duration
	// MaxIdle is how many idle connections are kept for reuse. Default 16.
	MaxIdle int

	mu     sync.Mutex
	idle   []*conn
	closed bool
}

type conn struct {
	net.Conn
	rd *bufio.Reader
}

// New parses a redis:// URL: redis://[[user]:password@]host[:port][/db].
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "redis" || u.Host == "" {
		// Never echo rawURL: it may carry the password.
		return nil, errors.New("redis: the URL isn't a valid redis:// URL")
	}
	c := &Client{addr: u.Host, Timeout: 500 * time.Millisecond, MaxIdle: 16}
	if u.Port() == "" {
		c.addr = net.JoinHostPort(u.Hostname(), "6379")
	}
	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		if c.db, err = strconv.Atoi(db); err != nil {
			return nil, fmt.Errorf("redis: database %q isn't a number", db)
		}
	}
	return c, nil
}

// Do sends one command and returns its reply: int64, string, nil, []any, or an error (a Redis
// error reply is a ReplyError).
func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	deadline := time.Now().Add(c.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	cn, err := c.get(deadline)
	if err != nil {
		return nil, err
	}
	if err := cn.SetDeadline(deadline); err != nil {
		_ = cn.Close()
		return nil, err
	}
	reply, err := roundTrip(cn, args)
	var redisErr ReplyError
	if err != nil && !errors.As(err, &redisErr) {
		_ = cn.Close()
		return nil, err
	}
	c.put(cn)
	return reply, err
}

func (c *Client) get(deadline time.Time) (*conn, error) {
	c.mu.Lock()
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	return c.dial(deadline)
}

func (c *Client) put(cn *conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.idle) >= max(c.MaxIdle, 1) {
		_ = cn.Close()
		return
	}
	c.idle = append(c.idle, cn)
}

// Close closes the idle connections; connections in use close when they're returned.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, cn := range c.idle {
		_ = cn.Close()
	}
	c.idle = nil
}

func (c *Client) dial(deadline time.Time) (*conn, error) {
	nc, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", c.addr)
	if err != nil {
		return nil, err
	}
	cn := &conn{Conn: nc, rd: bufio.NewReader(nc)}
	if err := cn.SetDeadline(deadline); err != nil {
		_ = cn.Close()
		return nil, err
	}
	if c.password != "" {
		args := []string{"AUTH", c.password}
		if c.username != "" {
			args = []string{"AUTH", c.username, c.password}
		}
		if _, err := roundTrip(cn, args); err != nil {
			_ = cn.Close()
			return nil, err
		}
	}
	if c.db != 0 {
		if _, err := roundTrip(cn, []string{"SELECT", strconv.Itoa(c.db)}); err != nil {
			_ = cn.Close()
			return nil, err
		}
	}
	return cn, nil
}

func roundTrip(cn *conn, args []string) (any, error) {
	if _, err := cn.Write(encode(args)); err != nil {
		return nil, err
	}
	return readReply(cn.rd)
}

// encode writes a command as a RESP array of bulk strings.
func encode(args []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

// ReplyError is an error reply from Redis.
type ReplyError string

func (e ReplyError) Error() string { return "redis: " + string(e) }

func readReply(rd *bufio.Reader) (any, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if line == "" {
		return nil, errors.New("redis: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, ReplyError(line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		buf := make([]byte, n+2)
		if _, err := readFull(rd, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < 0 {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = readReply(rd); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

func readFull(rd *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := rd.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// Ping checks the server answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "PING")
	return err
}

// Script is a Lua script, sent by its SHA1 once Redis has it.
type Script struct {
	src string
	sha string
}

// NewScript prepares a script.
func NewScript(src string) *Script {
	sum := sha1.Sum([]byte(src))
	return &Script{src: src, sha: hex.EncodeToString(sum[:])}
}

// Eval runs the script with keys and args: EVALSHA, and EVAL when Redis doesn't have it yet
// (after a restart, say).
func (c *Client) Eval(ctx context.Context, s *Script, keys []string, args ...string) (any, error) {
	cmd := make([]string, 0, 3+len(keys)+len(args))
	cmd = append(cmd, "EVALSHA", s.sha, strconv.Itoa(len(keys)))
	cmd = append(append(cmd, keys...), args...)
	reply, err := c.Do(ctx, cmd...)
	var redisErr ReplyError
	if errors.As(err, &redisErr) && strings.HasPrefix(string(redisErr), "NOSCRIPT") {
		cmd[0], cmd[1] = "EVAL", s.src
		return c.Do(ctx, cmd...)
	}
	return reply, err
}
