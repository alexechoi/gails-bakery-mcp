# gails-bakery-mcp

An MCP (Model Context Protocol) server for **Gail's Bakery** online ordering,
modelled on [`dvcrn/mcp-server-wework`](https://github.com/dvcrn/mcp-server-wework).

It exposes the Gail's ordering backend (the VMOS / Vita Mojo platform behind
`order.gailsbread.co.uk`) as MCP tools over stdio. Public catalog tools work
with no credentials; user tools sign in with an email/password and reuse a
cached bearer token.

## Build

```bash
go build -o gails-mcp ./cmd/mcp-server-gails
```

## Tools

### Public (no authentication)

| Tool          | Description                                                        |
|---------------|--------------------------------------------------------------------|
| `find_stores`   | Find stores near a postcode (auto-geocoded) or lat/long.         |
| `get_menu`      | Get the menu for a store (defaults to the standard Click&Collect).|
| `get_product`   | Get full product/bundle detail incl. modifiers, by bundle UUID.  |
| `get_timeslots` | List collection time slots for a date and basket amount.         |

### Authenticated (require `GAILS_EMAIL` / `GAILS_PASSWORD`)

| Tool                 | Description                                            |
|----------------------|--------------------------------------------------------|
| `get_profile`        | The signed-in user's profile.                          |
| `update_address`     | Update delivery address / postcode (PATCH profile).    |
| `get_subscriptions`  | Notification/subscription settings.                    |
| `get_loyalty_points` | Loyalty points balance and rewards.                    |
| `get_referrer_code`  | The user's referral code.                              |
| `order_history`      | Past orders (see note below).                          |
| `get_payment_methods`| Available and stored payment methods.                  |
| `get_user_promotions`| Promotions/rewards applicable to a basket.             |
| `apply_promotion`    | Apply a promotion to a basket; returns adjusted basket.|
| `get_transactions`   | Payment transaction details for given order UUIDs.     |
| `create_order`       | Create an order from a basket (checkout step 1; no charge). |
| `initiate_payment`   | Start payment for an order (checkout step 2; returns 3DS action). |
| `confirm_payment`    | Confirm/finalise payment for an order. ⚠️ places a real, paid order. |

## Checkout flow

The full ordering sequence maps to three tools:

1. **`create_order`** → `POST /order/v1/commands/create` — builds the order from the
   basket and returns its UUID. No card data, no charge.
2. **`initiate_payment`** → `POST /payment/v2/transactions/order` — submits the
   Adyen-encrypted `paymentMethod` (incl. the encrypted CVC) plus `browserInfo`
   and `order:{uuid,amount}`; returns an Adyen 3DS action.
3. **`confirm_payment`** → `POST …/order/{uuid}/confirm?transactionUUID=…` with
   `{"details":{"threeDSResult":"…"}}` — finalises the paid order.

> **Card data / CVC:** card number, expiry and CVC are never sent in plaintext.
> They must be encrypted client-side by Adyen Web (`adyen.js`) into the
> `encryptedCardNumber` / `encryptedSecurityCode` blobs before being passed to
> `initiate_payment`. This server does not perform Adyen client-side encryption;
> supply the already-encrypted `paymentMethod` object in the `body`.

> **Note:** the exact upstream path for order history is tenant-specific and
> was not confirmed during development. `order_history` therefore takes a
> `path` argument — capture the real request from the browser network tab
> (e.g. `/order/v1/<segment>/user-history`) and pass it in.

## Configuration

Authenticated tools read credentials from the environment:

```bash
export GAILS_EMAIL="you@example.com"
export GAILS_PASSWORD="••••••••"
```

### Claude Desktop / MCP client config

```json
{
  "mcpServers": {
    "gails-bakery": {
      "command": "/absolute/path/to/gails-mcp",
      "env": {
        "GAILS_EMAIL": "you@example.com",
        "GAILS_PASSWORD": "••••••••"
      }
    }
  }
}
```

## Constants

The Gail's tenant and sensible store/menu defaults are baked in
(`internal/gails/client.go`):

- Tenant: `1e2d00fd-212c-4556-8763-3f614fe9f2fa`
- Default store: `9b607569-f04d-11eb-9486-0676c9cc7839`
- Default menu: `b8e28ac3-4d9a-48ea-969a-03702840c5cd`

## Quick test over stdio

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find_stores","arguments":{"postcode":"EC4V 6BJ","limit":3}}}' \
  | ./gails-mcp
```

## Layout

```
cmd/mcp-server-gails/main.go   # tool registration + entry point
internal/mcp/                  # minimal JSON-RPC MCP server + stdio transport
internal/gails/client.go       # HTTP client, tenant headers, auth/token cache
internal/app/service.go        # one method per tool
```
