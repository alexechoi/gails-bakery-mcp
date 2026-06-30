package adyen

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func TestEncryptFieldRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := &Encryptor{pub: &priv.PublicKey}

	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	blob, err := e.EncryptField("cvc", "737", now)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// 1) compact JWE has exactly 5 dot-separated parts.
	if parts := strings.Split(blob, "."); len(parts) != 5 {
		t.Fatalf("want 5 JWE parts, got %d", len(parts))
	}

	// 2) protected header matches Adyen's exactly.
	hdrJSON, _ := base64.RawURLEncoding.DecodeString(strings.Split(blob, ".")[0])
	var hdr map[string]any
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if hdr["alg"] != "RSA-OAEP" || hdr["enc"] != "A256CBC-HS512" || hdr["version"] != "1" {
		t.Fatalf("unexpected header: %v", hdr)
	}

	// 3) decrypts back to the original payload incl. generationtime.
	obj, err := jose.ParseEncryptedCompact(blob,
		[]jose.KeyAlgorithm{jose.RSA_OAEP},
		[]jose.ContentEncryption{jose.A256CBC_HS512})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dec, err := obj.Decrypt(priv)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(dec, &got); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if got["cvc"] != "737" {
		t.Fatalf("cvc mismatch: %q", got["cvc"])
	}
	if got["generationtime"] != "2026-06-30T12:00:00.000Z" {
		t.Fatalf("generationtime mismatch: %q", got["generationtime"])
	}
}

func TestParsePublicKey(t *testing.T) {
	// exponent 10001 (hex) == 65537
	pub, err := parsePublicKey("10001|CE997E1FEE44C92AD3C84B8797CA9313")
	if err != nil {
		t.Fatal(err)
	}
	if pub.E != 65537 {
		t.Fatalf("exponent: got %d want 65537", pub.E)
	}
	if pub.N == nil || pub.N.Sign() <= 0 {
		t.Fatal("expected a positive modulus")
	}
	if _, err := parsePublicKey("nopipe"); err == nil {
		t.Fatal("expected error for malformed key")
	}
}
