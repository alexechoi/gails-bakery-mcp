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
		Description: "Find Gail's Bakery stores near a postcode or lat/long, sorted nearest-first. No authentication required. Each store includes a computed `distanceKm`, `distanceMiles` and `walkMinutes` (the API's own `distance` field is always null, so it's calculated from coordinates). The nearest store is the first result. Note: `hours` is always null here — for open-now use `status` + `appStatus.online == \"on\"`, and for opening times call `store_hours`.",
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
		Name:        "list_products",
		Description: "List menu products (bundles) with names and prices, sorted cheapest first. Use this to browse items or find the cheapest/priciest — get_menu only returns category names, not products. Optionally scope to one category by name substring (e.g. \"pastries\") or UUID. Prices are the takeaway 'from' price in GBP. No authentication required.",
		InputSchema: objSchema(map[string]any{
			"category": strSchema("Optional category name substring (e.g. 'hot drinks') or UUID. Omit to list the whole menu."),
			"store":    strSchema("Store UUID. Defaults to the standard store."),
			"menu":     strSchema("Menu UUID. Defaults to the standard menu."),
			"limit":    intSchema("Max products to return (after sorting cheapest first). 0 = all."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.ListProductsInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.ListProducts(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_menu",
		Description: "Get the menu structure for a Gail's store: this returns CATEGORY NAMES ONLY (e.g. PASTRIES, LUNCH) plus opening hours — NOT products or prices. To list items with prices use `list_products`; for one item's full detail use `get_product`. No authentication required. Defaults to the standard Click & Collect menu/store.",
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
		Name:        "store_hours",
		Description: "Get a store's opening hours: today's hours (currentDayWorkHours), all 7 weekdays (availableHours), and a computed openNow flag (Europe/London). No authentication required. The store finder does not return hours; use this instead.",
		InputSchema: objSchema(map[string]any{
			"store": strSchema("Store UUID (from find_stores). Defaults to the standard store."),
			"menu":  strSchema("Menu UUID. Defaults to the standard shared menu."),
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.StoreHoursInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.GetStoreHours(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "get_product",
		Description: "Get full detail for one product/bundle (modifiers, options, nutrition, allergens) by its bundle UUID — get the UUID from `list_products`. Note: these are CUSTOMISED bundles, so the top-level price is 0; the real takeaway price lives at items[].customizations[].variations[].price (list_products extracts it for you). No authentication required.",
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
		Name:        "create_order",
		Description: "Create an order from a basket payload (step 1 of checkout — does not charge). Requires authentication. `body` is the full order payload (bundles, timeSlot, customers, payment, user, device). Returns the created order incl. its UUID.",
		InputSchema: objSchema(map[string]any{
			"body": map[string]any{"type": "object", "description": "The full order/basket payload."},
		}, "body"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.CreateOrderInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.CreateOrder(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "place_order",
		Description: "Assemble a complete basket for a single product and create the order. Use this instead of create_order — it builds the full VMOS basket (user, customers, grouped itemTypes/items, prices) from the bundle_id + timeslot, so you don't hand-craft it. Set dry_run=true to inspect the exact payload and the amount that would be charged WITHOUT creating anything. Without dry_run it creates a real (unpaid) order; pay separately with pay_with_stored_card. Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"bundle_id": strSchema("The product/bundle UUID to order (from list_products)."),
			"timeslot":  map[string]any{"type": "object", "description": "A collection slot object from get_timeslots (the whole object, e.g. {uuid, slot, weekday, timezone, ...})."},
			"date_ms":   intSchema("Collection date as epoch milliseconds (same dateSlot used for get_timeslots)."),
			"store":     strSchema("Store UUID. Defaults to the standard store."),
			"menu":      strSchema("Menu UUID. Defaults to the standard menu."),
			"eat_in":    boolSchema("Eat-in pricing instead of takeaway. Defaults to false (takeaway)."),
			"dry_run":   boolSchema("If true, return the assembled payload + amount without creating the order."),
		}, "bundle_id", "timeslot"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.PlaceOrderInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.PlaceOrder(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "initiate_payment",
		Description: "Initiate payment for an order (step 2 of checkout). Requires authentication. `body` carries providers[] with an Adyen-encrypted paymentMethod (encrypted card/CVC blobs produced client-side by Adyen Web), browserInfo, riskData, and order:{uuid,amount}. Returns an Adyen 3DS action whose result feeds confirm_payment.",
		InputSchema: objSchema(map[string]any{
			"body": map[string]any{"type": "object", "description": "The payment payload incl. providers[] and order."},
		}, "body"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.InitiatePaymentInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.InitiatePayment(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "prepare_3ds_challenge",
		Description: "When pay_with_stored_card returns a 3-D Secure `action` (bank verification required), pass that action here to get a clickable URL. The shopper opens it, completes the bank check (e.g. approves in their banking app), and the order is confirmed automatically. Extract `action`, and the transaction UUID, from the pay_with_stored_card response. Requires the companion challenge server to be running (GAILS_3DS_SERVER).",
		InputSchema: objSchema(map[string]any{
			"action":           map[string]any{"type": "object", "description": "The 3DS `action` object returned by pay_with_stored_card."},
			"order_uuid":       strSchema("The order UUID being paid for."),
			"transaction_uuid": strSchema("The payment transaction UUID (from the pay_with_stored_card response)."),
			"amount":           numberSchema("Order amount, for display on the verification page."),
			"store":            strSchema("Store UUID. Defaults to the standard store."),
		}, "action", "order_uuid", "transaction_uuid"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.Prepare3DSInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.Prepare3DS(ctx, in)
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

	server.AddTool(mcp.Tool{
		Name:        "adyen_encrypt",
		Description: "Adyen client-side encryption of a single card field into a JWE blob (RSA-OAEP + A256CBC-HS512), matching the web checkout. No authentication required. Use for 'cvc' (default), 'number', 'expiryMonth', 'expiryYear'.",
		InputSchema: objSchema(map[string]any{
			"field": strSchema("Card field name: cvc (default), number, expiryMonth, expiryYear."),
			"value": strSchema("The plaintext value to encrypt."),
		}, "value"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.AdyenEncryptInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.AdyenEncrypt(ctx, in)
		},
	})

	server.AddTool(mcp.Tool{
		Name:        "pay_with_stored_card",
		Description: "Pay for an order with a saved card, frictionless-first. CSE-encrypts the CVC, initiates payment, then auto-completes the 3-D Secure device fingerprint server-side. Returns a `status`: 'paid' (frictionless — done), '3ds_challenge_required' (a `challenge.pay_url` the shopper opens to approve with their bank), 'authorised_or_no_action', or 'advanced'. WARNING: this attempts a real charge and, when frictionless, places a paid order. Set manual_only=true to just initiate and return the raw action. CVC via `cvc` or the GAILS_CVC env var (preferred). Requires authentication.",
		InputSchema: objSchema(map[string]any{
			"order_uuid":               strSchema("The order UUID to pay for (from create_order)."),
			"amount":                   numberSchema("Order amount to charge."),
			"stored_payment_method_id": strSchema("Saved card id (see get_payment_methods.storedPaymentMethods[].id)."),
			"brand":                    strSchema("Card brand, e.g. 'mc' or 'visa'."),
			"cvc":                      strSchema("Card security code. Prefer the GAILS_CVC env var so it is not passed as a tool argument."),
			"holder_name":              strSchema("Optional cardholder name."),
			"store":                    strSchema("Store UUID. Defaults to the standard store."),
			"risk_client_data":         strSchema("Optional Adyen device-fingerprint clientData (base64)."),
			"redirect_url":             strSchema("Optional redirect URL; defaults to the order-confirmation URL."),
			"browser_info":             map[string]any{"type": "object", "description": "Optional Adyen browserInfo object; a sensible default is used if omitted."},
			"manual_only":              boolSchema("If true, only initiate and return the raw 3DS action without auto-completing the fingerprint. Default false (frictionless-first)."),
			"redirect_3ds":             boolSchema("Sets our own returnUrl for a redirect-style 3DS. NOTE: this tenant's Adyen does not offer a client-selectable redirect flow (browserInfo is mandatory → native 3DS2), so a challenged payment must be completed on gails.vmos.io. Leaving this false is recommended."),
		}, "order_uuid", "amount", "stored_payment_method_id"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in app.PayWithStoredCardInput
			if err := decode(raw, &in); err != nil {
				return nil, err
			}
			return service.PayWithStoredCard(ctx, in)
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
