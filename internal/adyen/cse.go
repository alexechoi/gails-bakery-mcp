// Package adyen implements Adyen client-side encryption (CSE) for card data.
//
// Gail's checkout encrypts card fields (number, expiry, and crucially the CVC
// for a stored card) into JWE blobs before sending them to the backend. This
// package reproduces that step server-side so the MCP server can build payment
// payloads without a browser.
//
// The encryption is a JWE with alg=RSA-OAEP (SHA-1), enc=A256CBC-HS512 and an
// extra "version":"1" header, exactly matching the blobs produced by Adyen Web.
// The RSA public key is fetched from Adyen using the (public) client key.
package adyen

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// DefaultClientKey is the public Adyen client key used by the Gail's web
// checkout. It is not a secret (it ships in the page) but can be overridden.
const DefaultClientKey = "live_G5EJ4AY3DBF6RPG4DERDWBCIGMK4REAT"

// Shared header values for Adyen requests originating from the Gail's site.
const (
	originHeader  = "https://gails.vmos.io"
	refererHeader = "https://gails.vmos.io/"
)

// snippet truncates a response body for error messages.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

const publicKeyBase = "https://checkoutshopper-live.adyen.com/checkoutshopper/v1/clientKeys/"

// Encryptor encrypts card fields with a fetched Adyen public key.
type Encryptor struct {
	pub *rsa.PublicKey
}

// FetchEncryptor retrieves the Adyen public key for clientKey and returns a
// ready-to-use Encryptor. If clientKey is empty, DefaultClientKey is used.
func FetchEncryptor(ctx context.Context, httpClient *http.Client, clientKey string) (*Encryptor, error) {
	if clientKey == "" {
		clientKey = DefaultClientKey
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicKeyBase+clientKey, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch adyen public key: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch adyen public key: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse adyen public key response: %w", err)
	}
	pub, err := parsePublicKey(out.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Encryptor{pub: pub}, nil
}

// NewEncryptor builds an Encryptor from a raw "exponentHex|modulusHex" key.
func NewEncryptor(publicKey string) (*Encryptor, error) {
	pub, err := parsePublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	return &Encryptor{pub: pub}, nil
}

// parsePublicKey parses Adyen's "exponentHex|modulusHex" key format, e.g.
// "10001|CE99...". Both halves are hex.
func parsePublicKey(s string) (*rsa.PublicKey, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "|", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid adyen public key format (want exponent|modulus)")
	}
	exp, ok := new(big.Int).SetString(parts[0], 16)
	if !ok {
		return nil, fmt.Errorf("invalid adyen public key exponent")
	}
	mod, ok := new(big.Int).SetString(parts[1], 16)
	if !ok {
		return nil, fmt.Errorf("invalid adyen public key modulus")
	}
	if !exp.IsInt64() || exp.Int64() <= 0 {
		return nil, fmt.Errorf("invalid adyen public key exponent value")
	}
	return &rsa.PublicKey{N: mod, E: int(exp.Int64())}, nil
}

// EncryptField encrypts a single card field into an Adyen JWE blob. fieldName is
// the Adyen field key ("number", "cvc", "expiryMonth", "expiryYear", ...) and
// value is its plaintext. A generationtime is included as Adyen requires.
func (e *Encryptor) EncryptField(fieldName, value string, now time.Time) (string, error) {
	payload := map[string]string{
		fieldName:        value,
		"generationtime": now.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	return e.encrypt(payload)
}

// EncryptCard encrypts a full set of card fields into a single JWE blob (used
// for the all-in-one card data form rather than per-field secured fields).
func (e *Encryptor) EncryptCard(fields map[string]string, now time.Time) (string, error) {
	payload := make(map[string]string, len(fields)+1)
	for k, v := range fields {
		payload[k] = v
	}
	payload["generationtime"] = now.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return e.encrypt(payload)
}

func (e *Encryptor) encrypt(payload map[string]string) (string, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	enc, err := jose.NewEncrypter(
		jose.A256CBC_HS512,
		jose.Recipient{Algorithm: jose.RSA_OAEP, Key: e.pub},
		(&jose.EncrypterOptions{}).WithHeader("version", "1"),
	)
	if err != nil {
		return "", fmt.Errorf("build adyen encrypter: %w", err)
	}
	obj, err := enc.Encrypt(plaintext)
	if err != nil {
		return "", fmt.Errorf("adyen encrypt: %w", err)
	}
	return obj.CompactSerialize()
}
