package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgprivate/mqttview/internal/plugins/plc"
)

// pluginPath is where the Beckhoff PLC plugin mounts its API.
const pluginPath = "/api/p/" + plc.ID

// Client talks to a running mqttview over its HTTP API.
//
// mqttview authenticates with a session cookie rather than an API key, so the
// client logs in once and lets the cookie jar carry the session. Only GETs are
// ever issued, which also keeps it clear of the CSRF check on writes.
type Client struct {
	base string
	http *http.Client

	mu       sync.Mutex
	email    string
	password string
	loggedIn bool
}

// NewClient builds a client for a base URL such as http://127.0.0.1:8114.
func NewClient(base, email, password string) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookie jar: %w", err)
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("invalid mqttview URL %q: %w", base, err)
	}
	return &Client{
		base:     strings.TrimRight(base, "/"),
		email:    email,
		password: password,
		// No global timeout: a long poll legitimately takes a minute. Each
		// request carries its own context deadline instead.
		http: &http.Client{Jar: jar},
	}, nil
}

// Login establishes a session. It is safe to call repeatedly.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	body, err := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach mqttview at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login as %s failed: %s", c.email, resp.Status)
	}
	c.loggedIn = true
	return nil
}

// get fetches a plugin endpoint into out, logging in first if needed and
// retrying once when the session has expired underneath us.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	c.mu.Lock()
	if !c.loggedIn {
		if err := c.loginLocked(ctx); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.mu.Unlock()

	status, err := c.doGet(ctx, path, query, out)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		c.mu.Lock()
		c.loggedIn = false
		err := c.loginLocked(ctx)
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if status, err = c.doGet(ctx, path, query, out); err != nil {
			return err
		}
	}

	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound, http.StatusForbidden:
		return fmt.Errorf("the Beckhoff PLC plugin is not enabled in mqttview (%s on %s)", http.StatusText(status), path)
	default:
		return fmt.Errorf("mqttview returned %s for %s", http.StatusText(status), path)
	}
}

func (c *Client) doGet(ctx context.Context, path string, query url.Values, out any) (int, error) {
	u := c.base + pluginPath + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach mqttview at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decoding %s: %w", path, err)
	}
	return resp.StatusCode, nil
}

// Status is the plugin's own health and configuration.
type Status struct {
	TopicPrefix string `json:"topicPrefix"`
	MeterPrefix string `json:"meterPrefix"`
	Points      int    `json:"points"`
	Lights      int    `json:"lights"`
	Edges       int    `json:"edges"`
	Seq         uint64 `json:"seq"`
	ReadOnly    bool   `json:"readOnly"`
}

// EdgePage is one read of the signal journal.
type EdgePage struct {
	Edges []plc.Edge `json:"edges"`
	Seq   uint64     `json:"seq"`
}

// Status fetches the plugin status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.get(ctx, "/status", nil, &s)
	return s, err
}

// State fetches the full derived PLC state.
func (c *Client) State(ctx context.Context, connectionID string) (plc.State, error) {
	q := url.Values{}
	if connectionID != "" {
		q.Set("connectionId", connectionID)
	}
	var s plc.State
	err := c.get(ctx, "/state", q, &s)
	return s, err
}

// csrfToken returns the double-submit token mqttview set at login. Writes need
// it echoed in a header; reads do not.
func (c *Client) csrfToken() string {
	u, err := url.Parse(c.base)
	if err != nil {
		return ""
	}
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "mqttview_csrf" {
			return ck.Value
		}
	}
	return ""
}

// SetMapping names a point. This writes to mqttview's own store, not to the
// PLC: it changes what a signal is called here, and nothing in the house.
func (c *Client) SetMapping(ctx context.Context, connectionID string, m plc.Mapping) (plc.Mapping, error) {
	c.mu.Lock()
	if !c.loggedIn {
		if err := c.loginLocked(ctx); err != nil {
			c.mu.Unlock()
			return plc.Mapping{}, err
		}
	}
	c.mu.Unlock()

	payload := struct {
		ConnectionID string `json:"connectionId,omitempty"`
		plc.Mapping
	}{ConnectionID: connectionID, Mapping: m}

	body, err := json.Marshal(payload)
	if err != nil {
		return plc.Mapping{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+pluginPath+"/mappings", bytes.NewReader(body))
	if err != nil {
		return plc.Mapping{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrfToken())

	resp, err := c.http.Do(req)
	if err != nil {
		return plc.Mapping{}, fmt.Errorf("cannot reach mqttview at %s: %w", c.base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return plc.Mapping{}, fmt.Errorf("naming failed: %s", e.Error)
		}
		return plc.Mapping{}, fmt.Errorf("naming failed: %s", resp.Status)
	}

	var out plc.Mapping
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return plc.Mapping{}, err
	}
	return out, nil
}

// Edges reads the signal journal, blocking up to wait for something new.
func (c *Client) Edges(ctx context.Context, connectionID string, since uint64, limit int, rising bool, kind string, wait time.Duration) (EdgePage, error) {
	q := url.Values{}
	if connectionID != "" {
		q.Set("connectionId", connectionID)
	}
	if since > 0 {
		q.Set("since", strconv.FormatUint(since, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if rising {
		q.Set("rising", "true")
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if wait > 0 {
		q.Set("waitMs", strconv.FormatInt(wait.Milliseconds(), 10))
	}

	var page EdgePage
	err := c.get(ctx, "/edges", q, &page)
	return page, err
}
