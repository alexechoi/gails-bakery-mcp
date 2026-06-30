package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	_ "time/tzdata"

	"github.com/alexechoi/gails-bakery-mcp/internal/app"
	"github.com/alexechoi/gails-bakery-mcp/internal/gails"
	"github.com/alexechoi/gails-bakery-mcp/internal/mcp"
)

func main() {
	client := gails.New(os.Getenv("GAILS_EMAIL"), os.Getenv("GAILS_PASSWORD"))
	service := app.NewService(client)
	server := mcp.NewServer("gails-bakery", "0.1.0")

	// --- Public catalog tools (no auth) ---

	server.AddTool(mcp.Tool{
		Name:        "find_stores",
		Description: "Find Gail's Bakery stores near a postcode or lat/long. No authentication required.",
		InputSchema: objSchema(map[string]any{
			"postcode": strSchema("UK postcode to search near, e.g. 'EC4V 6BJ'."),
			"lat":      numberSchema("Latitude to search near (optional, used with long)."),
			"long":     numberSchema("Longitude to search near (optional, used with lat)."),
			"limit":    intSchema("Max number of stores to return. Defaults to 15."),
			"offset":   intSchema("Pagination offset. Defaults to 0."),
			"weekday":  intSchema("ISO weekday for opening hours (1=Mon..7=Sun). Defaults to today."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.FindStoresInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.FindStores(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_menu",
		Description: "Get the product menu for a Gail's store. No authentication required. Defaults to the standard Click & Collect menu/store if not specified.",
		InputSchema: objSchema(map[string]any{
			"store":  strSchema("Store UUID. Defaults to the standard store."),
			"menu":   strSchema("Menu UUID. Defaults to the standard menu."),
			"locale": strSchema("Locale, e.g. 'en-GB'."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.GetMenuInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetMenu(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_product",
		Description: "Get full detail for a product/bundle (including modifiers and options) by its bundle UUID. No authentication required.",
		InputSchema: objSchema(map[string]any{
			"bundle_id": strSchema("The bundle/product UUID to fetch."),
			"store":     strSchema("Store UUID. Defaults to the standard store."),
			"menu":      strSchema("Menu UUID. Defaults to the standard menu."),
			"locale":    strSchema("Locale, e.g. 'en-GB'."),
		}, "bundle_id"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.GetProductInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetProduct(ctx, in)
		},
	})

	// --- Authenticated user tools (require GAILS_EMAIL / GAILS_PASSWORD) ---

	server.AddTool(mcp.Tool{
		Name:        "get_profile",
		Description: "Get the signed-in user's Gail's profile (name, phone, address, member number). Requires authentication.",
		InputSchema: objSchema(map[string]any{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return service.GetProfile(ctx)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "update_address",
		Description: "Update the signed-in user's delivery address and/or postcode on their profile (PATCH). Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"address":             strSchema("Street address to save."),
			"postcode":            strSchema("Postcode to save."),
			"address_coordinates": map[string]any{"description": "Optional address coordinates object to save."},
			"raw":                 map[string]any{"type": "object", "description": "Optional raw profile patch body merged into the request."},
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.UpdateAddressInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.UpdateAddress(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_subscriptions",
		Description: "Get the signed-in user's notification/subscription settings. Requires authentication.",
		InputSchema: objSchema(map[string]any{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return service.GetSubscriptions(ctx)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_loyalty_points",
		Description: "Get the signed-in user's loyalty points balance and rewards. Requires authentication.",
		InputSchema: objSchema(map[string]any{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return service.GetLoyaltyPoints(ctx)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_referrer_code",
		Description: "Get the signed-in user's referral code. Requires authentication.",
		InputSchema: objSchema(map[string]any{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return service.GetReferrerCode(ctx)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "order_history",
		Description: "Get the signed-in user's order history. Requires authentication. The exact upstream path is tenant-specific; pass `path` captured from the network tab (e.g. /order/v1/<segment>/user-history).",
		InputSchema: objSchema(map[string]any{
			"path":   strSchema("Full request path for user order history, e.g. /order/v1/<segment>/user-history."),
			"limit":  intSchema("Max number of orders. Defaults to 15."),
			"offset": intSchema("Pagination offset. Defaults to 0."),
			"store":  strSchema("Store UUID header. Defaults to the standard store."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.OrderHistoryInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.OrderHistory(ctx, in)
		},
	})

	// --- Ordering & payment tools ---

	server.AddTool(mcp.Tool{
		Name:        "get_timeslots",
		Description: "List collection time slots for a date and basket amount. No authentication required.",
		InputSchema: objSchema(map[string]any{
			"date":          strSchema("Collection date in YYYY-MM-DD format (Europe/London)."),
			"date_ms":       intSchema("Alternatively, the date as epoch milliseconds."),
			"basket_amount": numberSchema("Basket total used to determine slot availability. Defaults to 0."),
			"store":         strSchema("Store UUID. Defaults to the standard store."),
			"menu":          strSchema("Menu UUID. Defaults to the standard menu."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.TimeslotsInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetTimeslots(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_payment_methods",
		Description: "List available and stored payment methods for the signed-in user. Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"provider_uuid": strSchema("Payment provider UUID. Defaults to the standard Adyen provider."),
			"store":         strSchema("Store UUID. Defaults to the standard store."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.PaymentMethodsInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetPaymentMethods(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_user_promotions",
		Description: "Get promotions and rewards applicable to a basket. Requires authentication. `body` is the full basket payload (products, promotions, payment).",
		InputSchema: objSchema(map[string]any{
			"body": map[string]any{"type": "object", "description": "The basket payload to evaluate promotions against."},
		}, "body"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.UserPromotionsInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetUserPromotions(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "apply_promotion",
		Description: "Apply a promotion to a basket and return the adjusted basket. Requires authentication. `body` is the basket payload including the promotion to apply.",
		InputSchema: objSchema(map[string]any{
			"promotion_id": strSchema("The promotion ID (e.g. voucher/promo id) to apply."),
			"body":         map[string]any{"type": "object", "description": "The basket payload the promotion is applied to."},
		}, "promotion_id", "body"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.ApplyPromotionInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.ApplyPromotion(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_transactions",
		Description: "Fetch payment transaction details for one or more order UUIDs. Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"orders":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of order UUIDs."},
			"details": boolSchema("If true, include full transaction details."),
		}, "orders"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.GetTransactionsInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetTransactions(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "confirm_payment",
		Description: "Confirm/finalise a payment for an order (e.g. submitting a 3DS result). WARNING: this places a real, paid order. Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"order_uuid":       strSchema("The order UUID being paid for."),
			"transaction_uuid": strSchema("The payment transaction UUID to confirm."),
			"details":          map[string]any{"type": "object", "description": "Payment confirmation details (e.g. threeDSResult)."},
		}, "order_uuid", "transaction_uuid"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.ConfirmPaymentInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.ConfirmPayment(ctx, in)
		},
	})

	if err := mcp.Run(context.Background(), server); err != nil {
		log.Fatal(err)
	}
}

func decode[T any](raw json.RawMessage, out *T) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func objSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func strSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func numberSchema(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

func intSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}
