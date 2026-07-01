# Payments & 3-D Secure

How the Gail's MCP takes a real order to a paid order, including the tricky
part: completing a bank **3-D Secure (3DS2)** challenge from outside the
official web checkout.

## The checkout chain

| Step | Tool | Endpoint | Notes |
|------|------|----------|-------|
| 1. Build order | `place_order` | `POST /order/v1/commands/create` | Assembles the full basket (user, customers, grouped `itemTypes[].items[]`, prices) from a `bundle_id` + timeslot. No charge. Returns the order `uuid`. |
| 2. Initiate payment | `pay_with_stored_card` | `POST /payment/v2/transactions/order` | CSE-encrypts the CVC, sends `providers[]` with the Adyen-encrypted `paymentMethod`. Returns an array whose `[0].uuid` is the **transactionUUID** and `[0]…action` is the 3DS action. |
| 3a. Frictionless | (same call) | `checkoutshopper …/submitThreeDS2Fingerprint` | Server-side fingerprint (`threeDSCompInd:N`). If the bank authorises → assemble `threeDSResult` → confirm → **paid**, no browser. |
| 3b. Challenged | `hybrid_3ds` | see below | The bank demands step-up; needs a browser. |
| 4. Finalise | (auto) | `POST /payment/v2/transactions/order/{order}/confirm?transactionUUID=…` | Body `{"details":{"threeDSResult":"…"}}`. |

## Card data / CSE

Card number, expiry and CVC are never sent in plaintext. `internal/adyen`
performs **Adyen client-side encryption** in Go: fetch the RSA public key from
`…/checkoutshopper/v1/clientKeys/{clientKey}`, parse the `exponentHex|modulusHex`
format, and JWE-encrypt each field (`alg=RSA-OAEP`, `enc=A256CBC-HS512`,
header `version:"1"`, plus a `generationtime`). For a stored card only the CVC
is encrypted (`encryptedSecurityCode`) alongside `storedPaymentMethodId`.

Prefer supplying the CVC via `GAILS_CVC` (env) rather than the `cvc` argument so
it never appears in tool-call arguments.

## 3-D Secure: why it's hard, and the hybrid solution

The official checkout runs Adyen Web on `gails.vmos.io`. Two browser-security
mechanisms stop us reproducing that on any other origin (e.g. an ngrok tunnel):

1. **Adyen client-key origin allowlist (CORS).** Adyen Web's own XHRs to
   `checkoutshopper` are pre-flighted; Adyen returns **403** for origins not in
   the client key's allowlist (only `gails.vmos.io` is). Seen as
   `OPTIONS … 403` on `submitThreeDS2Fingerprint`.
2. **`X-Frame-Options: SAMEORIGIN`** on the bank's ACS (`secure5.arcot.com`) —
   the challenge can't be **iframed** cross-origin ("refused to connect").

### The hybrid flow (`hybrid_3ds: true`)

`pay_with_stored_card` with `hybrid_3ds: true` returns a `pay_url` (served by the
embedded tunnel — see below) that splits the work so each call lands where it's
allowed:

1. **3DS Method** — browser runs it as a **hidden-iframe form POST** to the ACS
   (`threeDSMethodUrl`). Form POST ⇒ no CORS preflight; it establishes the ACS's
   device context (the step whose absence left the challenge blank).
2. **Fingerprint** — submitted **server-side** (`threeDSCompInd:Y`) → no CORS.
   Returns the challenge action with an `authorisationToken`.
3. **Challenge** — rendered in an **iframe via form POST** of the `CReq` to the
   ACS. Form POST ⇒ no CORS preflight; a *valid* challenge frames fine (only
   arcot's error pages send `SAMEORIGIN`). The shopper approves (often via a
   banking-app push).
4. **Finalise** — **auto-detected**: the page listens for the completion
   `postMessage` from the ACS/Adyen origin (no button) and calls the server,
   which assembles `threeDSResult = base64({transStatus:"Y", authorisationToken})`
   and calls `/confirm`.

Net: every **Adyen API** call is server-side (dodges the CORS allowlist); every
**ACS** call is a browser form-POST (dodges `X-Frame-Options`). Verified live
with a real £1.65 order (`HTTP 201`, paid).

### Findings worth remembering

- `submitThreeDS2Challenge` is **not** available on this account (`HTTP 400 —
  "Service … not present"`). Do **not** rely on it; assemble the `threeDSResult`
  from `{transStatus, authorisationToken}` instead.
- Headless VMOS calls need a **browser `User-Agent`** or Cloudflare blocks them
  with `403 "error code: 1010"`. (The Go client already sets one; any ad-hoc
  helper must too.)
- There is **no** client-selectable redirect flow: dropping `browserInfo` to
  force a redirect action fails with `failureCode 15_002 — browserInfo missing`.
- A failed initiate returns `status: "payment_failed"` with the `failureMessage`
  (never mislabel a missing action as authorised).

## The embedded 3DS challenge server + ngrok

The challenge/finalise endpoints run **inside the MCP process** (`internal/tunnel`)
and are exposed via ngrok automatically: it reuses a running ngrok agent's
tunnel if there is one, otherwise spawns `ngrok http` itself. Requires the
`ngrok` CLI installed + authenticated (a free account is fine). Override the
public URL/server with `GAILS_3DS_SERVER` if you prefer an external one.

Because a long-lived MCP process serves the `pay_url`, run the hybrid flow from
your persistent Claude Code MCP (not a one-shot), so the link stays alive while
the shopper approves.

## Environment variables

| Var | Purpose |
|-----|---------|
| `GAILS_EMAIL` / `GAILS_PASSWORD` | VMOS login (bearer token, cached). |
| `GAILS_CVC` | CVC for `pay_with_stored_card` (keeps it out of tool args). |
| `GAILS_ADYEN_CLIENT_KEY` | Override the (public) Adyen client key. |
| `GAILS_3DS_SERVER` | Use an external 3DS challenge server instead of the embedded one. |
