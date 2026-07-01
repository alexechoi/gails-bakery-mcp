package adyen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// checkoutShopperBase is Adyen's live shopper endpoint that adyen-web talks to
// directly to run the 3DS2 handshake (fingerprint + challenge).
const checkoutShopperBase = "https://checkoutshopper-live.adyen.com/checkoutshopper/v1"

// b64json marshals v and standard-base64-encodes it, matching how adyen-web
// encodes fingerprintResult / challengeResult (e.g. {"threeDSCompInd":"N"}).
func b64json(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeToken decodes a 3DS2 action token (base64 JSON) into a map. Tokens carry
// threeDSMethodURL/threeDSServerTransID (fingerprint) or acsURL/acsTransID +
// the CReq (challenge). Tries the common base64 variants.
func DecodeToken(token string) (map[string]any, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(token); err == nil {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("could not base64/JSON-decode token")
}

func postCheckoutShopper(ctx context.Context, httpClient *http.Client, path, clientKey string, body any) (json.RawMessage, error) {
	if clientKey == "" {
		clientKey = DefaultClientKey
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := checkoutShopperBase + path + "?token=" + url.QueryEscape(clientKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("origin", originHeader)
	req.Header.Set("referer", refererHeader)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adyen 3ds request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("adyen %s returned HTTP %d: %s", path, resp.StatusCode, snippet(raw))
	}
	return json.RawMessage(raw), nil
}

// SubmitFingerprint completes the 3DS2 device-fingerprint step server-side.
// compInd is the threeDSCompInd: "Y" (method completed), "N" (not run — the
// no-browser default), or "U" (unavailable). The response is either a final
// result (often Authorised — frictionless) or a challenge action.
func SubmitFingerprint(ctx context.Context, httpClient *http.Client, clientKey, paymentData, compInd string) (json.RawMessage, error) {
	if paymentData == "" {
		return nil, fmt.Errorf("payment_data is required (from the IdentifyShopper action)")
	}
	if compInd == "" {
		compInd = "N"
	}
	return postCheckoutShopper(ctx, httpClient, "/submitThreeDS2Fingerprint", clientKey, map[string]any{
		"fingerprintResult": b64json(map[string]string{"threeDSCompInd": compInd}),
		"paymentData":       paymentData,
	})
}

// SubmitChallenge submits the 3DS2 challenge result after the shopper has
// authenticated with their bank. transStatus is "Y" (authenticated), "N",
// "U", or "A". The response carries the details/threeDSResult to confirm with.
func SubmitChallenge(ctx context.Context, httpClient *http.Client, clientKey, paymentData, transStatus string) (json.RawMessage, error) {
	if paymentData == "" {
		return nil, fmt.Errorf("payment_data is required (from the ChallengeShopper action)")
	}
	if transStatus == "" {
		transStatus = "Y"
	}
	return postCheckoutShopper(ctx, httpClient, "/submitThreeDS2Challenge", clientKey, map[string]any{
		"challengeResult": b64json(map[string]string{"transStatus": transStatus}),
		"paymentData":     paymentData,
	})
}
