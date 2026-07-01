// Package gails is a thin client for the Gail's Bakery ordering backend
// (the VMOS / Vita Mojo platform that powers order.gailsbread.co.uk).
//
// Public catalog endpoints (stores, menu, bundles) need no authentication —
// only the tenant header. User endpoints (profile, loyalty, subscriptions)
// carry a bearer token obtained from the email/password auth endpoint.
package gails

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	BaseURL = "https://vmos2.vmos.io"

	// Tenant for Gail's Bakery on the VMOS platform.
	TenantUUID = "1e2d00fd-212c-4556-8763-3f614fe9f2fa"

	// Sensible defaults captured from the Gail's web ordering site.
	DefaultStoreUUID = "9b607569-f04d-11eb-9486-0676c9cc7839"
	DefaultMenuUUID  = "b8e28ac3-4d9a-48ea-969a-03702840c5cd"
	DefaultLocale    = "en-GB"

	// Adyen payment provider used by the Gail's web checkout.
	DefaultPaymentProviderUUID = "b46b4168-961a-40c3-9f9b-e8909abd7589"

	originHeader  = "https://gails.vmos.io"
	refererHeader = "https://gails.vmos.io/"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)

// Client talks to the Gail's backend. The zero value is not usable; use New.
type Client struct {
	http     *http.Client
	email    string
	password string

	mu    sync.Mutex
	token string
}

// New returns a client. email and password may be empty if only public
// catalog endpoints are used; authenticated tools will error without them.
func New(email, password string) *Client {
	return &Client{
		http:     &http.Client{Timeout: 30 * time.Second},
		email:    email,
		password: password,
	}
}

// HasCredentials reports whether email and password were provided.
func (c *Client) HasCredentials() bool {
	return c.email != "" && c.password != ""
}

// baseHeaders are sent on every request. Accept-Encoding is forced to identity
// so we never have to decompress br/gzip ourselves.
func (c *Client) applyBaseHeaders(req *http.Request) {
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", "en-GB,en;q=0.9")
	req.Header.Set("accept-encoding", "identity")
	req.Header.Set("origin", originHeader)
	req.Header.Set("referer", refererHeader)
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("tenant", TenantUUID)
	req.Header.Set("x-requested-from", "online")
}

// do issues a request and returns the decoded JSON body. opts header values
// (store, menu, locale, authorization) are layered on top of the base headers.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (json.RawMessage, error) {
	u := BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	c.applyBaseHeaders(req)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// Signal expired/invalid token so authenticated callers can re-login once.
		return nil, fmt.Errorf("%s %s returned HTTP 401: %s: %w", method, path, snippet(raw), errUnauthorized)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s returned HTTP %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null"), nil
	}
	return json.RawMessage(raw), nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// envelope is the standard { "payload": ... } wrapper the API uses.
type envelope struct {
	Payload json.RawMessage `json:"payload"`
}

// unwrap extracts the payload field, falling back to the raw body if absent.
func unwrap(raw json.RawMessage) json.RawMessage {
	var env envelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Payload) > 0 {
		return env.Payload
	}
	return raw
}

// --- Public request helpers ----------------------------------------------

// GetJSON performs an unauthenticated GET and returns the unwrapped payload.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, headers map[string]string) (any, error) {
	raw, err := c.do(ctx, http.MethodGet, path, query, nil, headers)
	if err != nil {
		return nil, err
	}
	return decode(unwrap(raw)), nil
}

// errUnauthorized is returned (wrapped) by do on an HTTP 401 so authenticated
// callers can transparently re-authenticate.
var errUnauthorized = errors.New("unauthorized")

// resetToken clears the cached bearer token so the next call logs in afresh.
func (c *Client) resetToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// doAuth performs an authenticated request, transparently re-authenticating once
// if the cached token has expired (HTTP 401). This handles JWT expiry without a
// server restart.
func (c *Client) doAuth(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (json.RawMessage, error) {
	h, err := c.authHeaders(ctx, headers)
	if err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, method, path, query, body, h)
	if err != nil && errors.Is(err, errUnauthorized) {
		c.resetToken()
		h, err = c.authHeaders(ctx, headers)
		if err != nil {
			return nil, err
		}
		raw, err = c.do(ctx, method, path, query, body, h)
	}
	return raw, err
}

// GetJSONAuth performs an authenticated GET and returns the unwrapped payload.
func (c *Client) GetJSONAuth(ctx context.Context, path string, query url.Values, headers map[string]string) (any, error) {
	raw, err := c.doAuth(ctx, http.MethodGet, path, query, nil, headers)
	if err != nil {
		return nil, err
	}
	return decode(unwrap(raw)), nil
}

// JSONAuth performs an authenticated request with a JSON body (e.g. PATCH/POST).
func (c *Client) JSONAuth(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) (any, error) {
	raw, err := c.doAuth(ctx, method, path, query, body, headers)
	if err != nil {
		return nil, err
	}
	return decode(unwrap(raw)), nil
}

// decode turns a raw JSON payload into a generic Go value so the MCP layer can
// re-marshal it as structured content.
func decode(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// --- Authentication -------------------------------------------------------

type authResponse struct {
	Payload struct {
		Token struct {
			Value string `json:"value"`
		} `json:"token"`
	} `json:"payload"`
}

// ensureToken logs in (once) and caches the bearer token.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	if !c.HasCredentials() {
		return "", fmt.Errorf("authentication required: set GAILS_EMAIL and GAILS_PASSWORD")
	}

	raw, err := c.do(ctx, http.MethodPost, "/user/v1/auth", nil, map[string]string{
		"email":    c.email,
		"password": c.password,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("login failed: %w", err)
	}
	var ar authResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return "", fmt.Errorf("login: cannot parse token: %w", err)
	}
	if ar.Payload.Token.Value == "" {
		return "", fmt.Errorf("login: empty token in response")
	}
	c.token = ar.Payload.Token.Value
	return c.token, nil
}

// TokenUser holds the identity fields the order payload needs, read from the
// JWT returned at login.
type TokenUser struct {
	UUID            string
	Email           string
	FirstName       string
	Phone           string
	MemberNumber    string
	ActeolMemberNum string
}

// UserInfo decodes the (unverified) JWT body to extract the signed-in user's
// identity. We only read claims we already trust from our own login, so no
// signature check is required.
func (c *Client) UserInfo(ctx context.Context) (TokenUser, error) {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return TokenUser{}, err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return TokenUser{}, fmt.Errorf("unexpected token format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenUser{}, fmt.Errorf("decode token: %w", err)
	}
	var claims struct {
		User struct {
			UUID    string `json:"uuid"`
			Email   string `json:"email"`
			Profile struct {
				FirstName          string `json:"firstName"`
				Phone              string `json:"phone"`
				MemberNumber       string `json:"memberNumber"`
				ActeolMemberNumber string `json:"acteolMemberNumber"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return TokenUser{}, fmt.Errorf("parse token claims: %w", err)
	}
	return TokenUser{
		UUID:            claims.User.UUID,
		Email:           claims.User.Email,
		FirstName:       claims.User.Profile.FirstName,
		Phone:           claims.User.Profile.Phone,
		MemberNumber:    claims.User.Profile.MemberNumber,
		ActeolMemberNum: claims.User.Profile.ActeolMemberNumber,
	}, nil
}

// authHeaders builds the header map for an authenticated request.
func (c *Client) authHeaders(ctx context.Context, extra map[string]string) (map[string]string, error) {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	h := map[string]string{"authorization": "Bearer " + token}
	for k, v := range extra {
		h[k] = v
	}
	return h, nil
}
