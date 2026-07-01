// Package app contains the tool-facing service methods. Each method maps one
// MCP tool to one or more Gail's backend calls and returns decoded JSON.
package app

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexechoi/gails-bakery-mcp/internal/adyen"
	"github.com/alexechoi/gails-bakery-mcp/internal/gails"
	"github.com/alexechoi/gails-bakery-mcp/internal/tunnel"
)

// extHTTP is used for auxiliary calls (Adyen checkoutshopper, postcodes.io, the
// external 3DS server). It has a bounded timeout so a slow/half-open upstream
// can't hang a tool call for minutes.
var extHTTP = &http.Client{Timeout: 15 * time.Second}

type Service struct {
	client *gails.Client

	encMu sync.Mutex
	enc   *adyen.Encryptor

	tunMu sync.Mutex
	tun   *tunnel.Manager
}

func NewService(client *gails.Client) *Service {
	return &Service{client: client}
}

// tunnelMgr lazily builds the embedded 3DS challenge server/tunnel manager,
// wiring its confirm callback to the authenticated client.
func (s *Service) tunnelMgr() *tunnel.Manager {
	s.tunMu.Lock()
	defer s.tunMu.Unlock()
	if s.tun == nil {
		clientKey := os.Getenv("GAILS_ADYEN_CLIENT_KEY")
		if clientKey == "" {
			clientKey = adyen.DefaultClientKey
		}
		s.tun = tunnel.New(clientKey, func(ctx context.Context, order, txn, store string, details map[string]any) (any, error) {
			return s.confirmOrder(ctx, order, txn, store, details)
		})
	}
	return s.tun
}

// confirmOrder finalises a payment: POST /payment/v2/transactions/order/{order}
// /confirm?transactionUUID={txn} with the given details.
func (s *Service) confirmOrder(ctx context.Context, order, txn, store string, details map[string]any) (any, error) {
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	q := url.Values{}
	q.Set("transactionUUID", txn)
	path := "/payment/v2/transactions/order/" + url.PathEscape(order) + "/confirm"
	return s.client.JSONAuth(ctx, http.MethodPost, path, q, map[string]any{"details": details}, map[string]string{"store": store})
}

// encryptor lazily fetches the Adyen public key (once) and caches an Encryptor.
// The client key may be overridden via GAILS_ADYEN_CLIENT_KEY.
func (s *Service) encryptor(ctx context.Context) (*adyen.Encryptor, error) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if s.enc != nil {
		return s.enc, nil
	}
	e, err := adyen.FetchEncryptor(ctx, extHTTP, os.Getenv("GAILS_ADYEN_CLIENT_KEY"))
	if err != nil {
		return nil, err
	}
	s.enc = e
	return e, nil
}

// catalogHeaders builds the store/menu/locale headers used by catalog calls.
func catalogHeaders(store, menu, locale string) map[string]string {
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	if menu == "" {
		menu = gails.DefaultMenuUUID
	}
	if locale == "" {
		locale = gails.DefaultLocale
	}
	return map[string]string{"store": store, "menu": menu, "locale": locale}
}

// --- Public catalog tools -------------------------------------------------

type FindStoresInput struct {
	Postcode string  `json:"postcode"`
	Lat      float64 `json:"lat"`
	Long     float64 `json:"long"`
	Limit    int     `json:"limit"`
	Offset   int     `json:"offset"`
	Weekday  int     `json:"weekday"`
}

func (s *Service) FindStores(ctx context.Context, in FindStoresInput) (any, error) {
	if in.Postcode == "" && in.Lat == 0 && in.Long == 0 {
		return nil, fmt.Errorf("provide a postcode, or lat and long")
	}
	// The store finder requires lat/long. If only a postcode was supplied,
	// geocode it (postcodes.io) to obtain coordinates.
	lat, long := in.Lat, in.Long
	if (lat == 0 || long == 0) && in.Postcode != "" {
		glat, glong, err := geocodePostcode(ctx, in.Postcode)
		if err != nil {
			return nil, fmt.Errorf("could not geocode postcode %q: %w (pass lat and long instead)", in.Postcode, err)
		}
		lat, long = glat, glong
	}
	if lat == 0 || long == 0 {
		return nil, fmt.Errorf("lat and long are required (geocoding the postcode did not yield coordinates)")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 15
	}
	q := url.Values{}
	q.Set("offset", strconv.Itoa(in.Offset))
	q.Set("limit", strconv.Itoa(limit))
	if in.Postcode != "" {
		q.Set("postcode", in.Postcode)
	}
	q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("long", strconv.FormatFloat(long, 'f', -1, 64))
	q.Add("sortBy[]", "name")
	q.Add("sortBy[]", "sortOrder")
	q.Add("sortBy[]", "distance")
	q.Set("sortDir", "ASC")
	q.Set("status", "1")
	// weekday is required by the API (ISO 1=Mon..7=Sun); default to today.
	weekday := in.Weekday
	if weekday < 1 || weekday > 7 {
		if wd := int(time.Now().Weekday()); wd == 0 {
			weekday = 7 // Go Sunday=0 -> ISO 7
		} else {
			weekday = wd
		}
	}
	q.Set("weekday", strconv.Itoa(weekday))
	raw, err := s.client.GetJSON(ctx, "/tenant/v1/stores/tenant", q, nil)
	if err != nil {
		return nil, err
	}
	// The API's own `distance` field is always null, so compute it from each
	// store's lat/long against the search point (haversine), then sort
	// nearest-first and add a rough walk-time estimate.
	stores, ok := raw.([]any)
	if !ok {
		return raw, nil
	}
	for _, st := range stores {
		sm, ok := st.(map[string]any)
		if !ok {
			continue
		}
		slat, e1 := strconv.ParseFloat(asStr(sm["lat"]), 64)
		slong, e2 := strconv.ParseFloat(asStr(sm["long"]), 64)
		if e1 != nil || e2 != nil {
			continue
		}
		km := haversineKm(lat, long, slat, slong)
		sm["distanceKm"] = math.Round(km*100) / 100
		sm["distanceMiles"] = math.Round(km*62.1371) / 100
		sm["walkMinutes"] = int(math.Round(km / 4.8 * 60)) // ~4.8 km/h walking
	}
	sort.SliceStable(stores, func(i, j int) bool {
		return storeDist(stores[i]) < storeDist(stores[j])
	})
	return stores, nil
}

func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// storeDist returns a store's computed distanceKm, or +Inf if it couldn't be
// computed (so those sort last).
func storeDist(st any) float64 {
	if sm, ok := st.(map[string]any); ok {
		if d, ok := sm["distanceKm"].(float64); ok {
			return d
		}
	}
	return math.Inf(1)
}

// haversineKm returns the great-circle distance in kilometres.
func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371.0
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := rad(lat2 - lat1)
	dLon := rad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

type GetMenuInput struct {
	Store  string `json:"store"`
	Menu   string `json:"menu"`
	Locale string `json:"locale"`
}

func (s *Service) GetMenu(ctx context.Context, in GetMenuInput) (any, error) {
	return s.client.GetJSON(ctx, "/catalog/v2/menu", nil, catalogHeaders(in.Store, in.Menu, in.Locale))
}

type StoreHoursInput struct {
	Store string `json:"store"`
	Menu  string `json:"menu"`
}

// GetStoreHours returns a store's opening hours. The store finder does not
// populate per-store hours (its `hours` field is null); the hours live on the
// menu endpoint as currentDayWorkHours (today) and availableHours (all 7 days),
// keyed by the store header. openNow is computed against Europe/London time.
// No authentication required.
func (s *Service) GetStoreHours(ctx context.Context, in StoreHoursInput) (any, error) {
	raw, err := s.client.GetJSON(ctx, "/catalog/v2/menu", nil, catalogHeaders(in.Store, in.Menu, ""))
	if err != nil {
		return nil, err
	}
	menus, ok := raw.([]any)
	if !ok || len(menus) == 0 {
		return nil, fmt.Errorf("no menu/hours found for this store")
	}
	menu, _ := menus[0].(map[string]any)
	today, _ := menu["currentDayWorkHours"].(map[string]any)
	weekly := menu["availableHours"]

	openNow := false
	if today != nil {
		from, _ := today["from"].(string)
		to, _ := today["to"].(string)
		loc, err := time.LoadLocation("Europe/London")
		if err != nil {
			loc = time.UTC
		}
		hm := time.Now().In(loc).Format("15:04")
		// HH:MM strings compare lexicographically in chronological order.
		if from != "" && to != "" && hm >= from && hm < to {
			openNow = true
		}
	}

	store := in.Store
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	return map[string]any{
		"store":       store,
		"openNow":     openNow,
		"today":       today,
		"weeklyHours": weekly,
	}, nil
}

type GetProductInput struct {
	BundleID string `json:"bundle_id"`
	Store    string `json:"store"`
	Menu     string `json:"menu"`
	Locale   string `json:"locale"`
}

func (s *Service) GetProduct(ctx context.Context, in GetProductInput) (any, error) {
	if in.BundleID == "" {
		return nil, fmt.Errorf("bundle_id is required")
	}
	q := url.Values{}
	q.Set("selectAllBundleItems", "1")
	q.Set("provideIsInLayoutStatus", "1")
	q.Set("forceStockStatus", "1")
	return s.client.GetJSON(ctx, "/catalog/bundles/"+url.PathEscape(in.BundleID), q, catalogHeaders(in.Store, in.Menu, in.Locale))
}

type ListProductsInput struct {
	Category string `json:"category"`
	Store    string `json:"store"`
	Menu     string `json:"menu"`
	Limit    int    `json:"limit"`
}

type productEntry struct {
	UUID     string   `json:"uuid"`
	Name     string   `json:"name,omitempty"`
	Category string   `json:"category"`
	Price    *float64 `json:"price,omitempty"`
}

// ListProducts lists the products (bundles) in the menu with names and prices.
// get_menu only returns category names, so this is how you enumerate items.
// It reads the menu's categories, then fetches each category's bundles via
// /catalog/categories/{uuid}/bundles (one call per category, concurrently),
// extracting each bundle's effective takeaway price. Results are sorted
// cheapest first, which answers questions like "the cheapest item". Pass
// `category` (name substring or UUID) to scope to one category. No auth.
func (s *Service) ListProducts(ctx context.Context, in ListProductsInput) (any, error) {
	headers := catalogHeaders(in.Store, in.Menu, "")
	raw, err := s.client.GetJSON(ctx, "/catalog/v2/menu", nil, headers)
	if err != nil {
		return nil, err
	}
	menus, _ := raw.([]any)
	if len(menus) == 0 {
		return nil, fmt.Errorf("no menu found for this store")
	}
	menu, _ := menus[0].(map[string]any)

	filter := strings.ToLower(strings.TrimSpace(in.Category))
	type catRef struct{ uuid, name string }
	var selected []catRef
	for _, c := range asSlice(menu["categories"]) {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		uuid, _ := cm["uuid"].(string)
		name, _ := cm["name"].(string)
		if uuid == "" {
			continue
		}
		if filter == "" || strings.Contains(strings.ToLower(name), filter) || strings.EqualFold(uuid, filter) {
			selected = append(selected, catRef{uuid, name})
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no matching category for %q", in.Category)
	}

	// One bundles call per category, concurrently.
	bundlesByCat := make([][]any, len(selected))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, cr := range selected {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, cr catRef) {
			defer wg.Done()
			defer func() { <-sem }()
			q := url.Values{}
			q.Set("forceStockStatus", "0")
			b, err := s.client.GetJSON(ctx, "/catalog/categories/"+url.PathEscape(cr.uuid)+"/bundles", q, headers)
			if err != nil {
				return
			}
			if bm, ok := b.(map[string]any); ok {
				bundlesByCat[i] = asSlice(bm["bundles"])
			}
		}(i, cr)
	}
	wg.Wait()

	seen := map[string]bool{}
	var entries []*productEntry
	for i, cr := range selected {
		for _, bd := range bundlesByCat[i] {
			bm, ok := bd.(map[string]any)
			if !ok {
				continue
			}
			uuid, _ := bm["uuid"].(string)
			if uuid == "" || seen[uuid] {
				continue
			}
			seen[uuid] = true
			name, _ := bm["name"].(string)
			entries = append(entries, &productEntry{UUID: uuid, Name: name, Category: cr.name, Price: effectivePrice(bm)})
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		pi, pj := entries[i].Price, entries[j].Price
		if pi == nil && pj == nil {
			return entries[i].Name < entries[j].Name
		}
		if pi == nil {
			return false
		}
		if pj == nil {
			return true
		}
		return *pi < *pj
	})
	total := len(entries)
	if in.Limit > 0 && len(entries) > in.Limit {
		entries = entries[:in.Limit]
	}
	return map[string]any{"count": total, "returned": len(entries), "products": entries}, nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asNum(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// effectivePrice derives a bundle's display (takeaway "from") price. Simple
// bundles carry it in price/priceFrom; CUSTOMISED bundles keep it on the base
// item's variations, so fall back to the lowest positive variation price among
// the base items.
func effectivePrice(b map[string]any) *float64 {
	if p := asNum(b["price"]); p > 0 {
		return &p
	}
	if p := asNum(b["priceFrom"]); p > 0 {
		return &p
	}
	items := asSlice(b["items"])
	// Prefer the base item(s); fall back to all items if none are tagged.
	var bases []any
	for _, it := range items {
		if im, ok := it.(map[string]any); ok {
			if t, _ := im["type"].(string); t == "BUNDLE_BASE" {
				bases = append(bases, it)
			}
		}
	}
	if len(bases) == 0 {
		bases = items
	}
	var min *float64
	consider := func(p float64) {
		if p > 0 && (min == nil || p < *min) {
			v := p
			min = &v
		}
	}
	for _, it := range bases {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		consider(asNum(im["price"]))
		for _, c := range asSlice(im["customizations"]) {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			for _, v := range asSlice(cm["variations"]) {
				if vm, ok := v.(map[string]any); ok {
					consider(asNum(vm["price"]))
				}
			}
		}
	}
	return min
}

// --- Authenticated user tools ---------------------------------------------

func (s *Service) GetProfile(ctx context.Context) (any, error) {
	return s.client.GetJSONAuth(ctx, "/user/v1/user/profile", nil, nil)
}

type UpdateAddressInput struct {
	Address            string `json:"address"`
	Postcode           string `json:"postcode"`
	AddressCoordinates any    `json:"address_coordinates"`
	// Raw lets callers pass an arbitrary profile patch body if needed.
	Raw map[string]any `json:"raw"`
}

func (s *Service) UpdateAddress(ctx context.Context, in UpdateAddressInput) (any, error) {
	body := map[string]any{}
	for k, v := range in.Raw {
		body[k] = v
	}
	if in.Address != "" {
		body["address"] = in.Address
	}
	if in.Postcode != "" {
		body["postcode"] = in.Postcode
	}
	if in.AddressCoordinates != nil {
		body["addressCoordinates"] = in.AddressCoordinates
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("provide address, postcode, address_coordinates, or raw")
	}
	return s.client.JSONAuth(ctx, http.MethodPatch, "/user/v1/user/profile", nil, body, nil)
}

func (s *Service) GetSubscriptions(ctx context.Context) (any, error) {
	return s.client.GetJSONAuth(ctx, "/user/v1/user/subscriptions", nil, nil)
}

func (s *Service) GetLoyaltyPoints(ctx context.Context) (any, error) {
	return s.client.GetJSONAuth(ctx, "/loyalty/v2/points", nil, nil)
}

func (s *Service) GetReferrerCode(ctx context.Context) (any, error) {
	return s.client.GetJSONAuth(ctx, "/user/v1/user/profile/referrer-code", nil, nil)
}

type OrderHistoryInput struct {
	// Path overrides the request path. The exact upstream path segment for
	// order history was not confirmed; capture it from the network tab and
	// pass it here, e.g. "/order/v1/<segment>/user-history".
	Path   string `json:"path"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Store  string `json:"store"`
}

func (s *Service) OrderHistory(ctx context.Context, in OrderHistoryInput) (any, error) {
	path := in.Path
	if path == "" {
		return nil, fmt.Errorf("order history path is not yet confirmed for this tenant; pass `path` (e.g. /order/v1/<segment>/user-history) captured from the browser network tab")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 15
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(in.Offset))
	store := in.Store
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	return s.client.GetJSONAuth(ctx, path, q, map[string]string{"store": store})
}

// --- Ordering & payment tools ---------------------------------------------

type TimeslotsInput struct {
	Date         string  `json:"date"`
	DateMs       int64   `json:"date_ms"`
	BasketAmount float64 `json:"basket_amount"`
	Store        string  `json:"store"`
	Menu         string  `json:"menu"`
}

// GetTimeslots lists collection time slots for a date and basket amount. No
// authentication required.
func (s *Service) GetTimeslots(ctx context.Context, in TimeslotsInput) (any, error) {
	dateMs := in.DateMs
	if dateMs == 0 && in.Date != "" {
		loc, err := time.LoadLocation("Europe/London")
		if err != nil {
			loc = time.UTC
		}
		t, err := time.ParseInLocation("2006-01-02", in.Date, loc)
		if err != nil {
			return nil, fmt.Errorf("invalid date %q (use YYYY-MM-DD): %w", in.Date, err)
		}
		dateMs = t.UnixMilli()
	}
	if dateMs == 0 {
		return nil, fmt.Errorf("provide date (YYYY-MM-DD) or date_ms (epoch milliseconds)")
	}
	q := url.Values{}
	q.Set("dateSlot", strconv.FormatInt(dateMs, 10))
	q.Set("basketAmount", strconv.FormatFloat(in.BasketAmount, 'f', -1, 64))
	return s.client.GetJSON(ctx, "/tenant/v1/timeslots", q, catalogHeaders(in.Store, in.Menu, ""))
}

type PaymentMethodsInput struct {
	ProviderUUID string `json:"provider_uuid"`
	Store        string `json:"store"`
}

// GetPaymentMethods lists available and stored payment methods. Requires auth.
func (s *Service) GetPaymentMethods(ctx context.Context, in PaymentMethodsInput) (any, error) {
	provider := in.ProviderUUID
	if provider == "" {
		provider = gails.DefaultPaymentProviderUUID
	}
	q := url.Values{}
	q.Set("providerUUID", provider)
	store := in.Store
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	return s.client.GetJSONAuth(ctx, "/payment/v2/payment-methods", q, map[string]string{"store": store})
}

type UserPromotionsInput struct {
	Body map[string]any `json:"body"`
}

// GetUserPromotions returns promotions/rewards applicable to a basket. Requires
// auth. body is the basket payload (products, promotions, payment, ...).
func (s *Service) GetUserPromotions(ctx context.Context, in UserPromotionsInput) (any, error) {
	if len(in.Body) == 0 {
		return nil, fmt.Errorf("body (basket payload) is required")
	}
	return s.client.JSONAuth(ctx, http.MethodPost, "/loyalty/promotions/user", nil, in.Body,
		map[string]string{"locale": gails.DefaultLocale, "platform": "web"})
}

type ApplyPromotionInput struct {
	PromotionID string         `json:"promotion_id"`
	Body        map[string]any `json:"body"`
}

// ApplyPromotion applies a promotion to a basket and returns the adjusted
// basket. Requires auth.
func (s *Service) ApplyPromotion(ctx context.Context, in ApplyPromotionInput) (any, error) {
	if in.PromotionID == "" {
		return nil, fmt.Errorf("promotion_id is required")
	}
	if len(in.Body) == 0 {
		return nil, fmt.Errorf("body (basket payload) is required")
	}
	path := "/loyalty/promotions/v2/" + url.PathEscape(in.PromotionID) + "/apply"
	return s.client.JSONAuth(ctx, http.MethodPost, path, nil, in.Body, map[string]string{"platform": "web"})
}

type GetTransactionsInput struct {
	Orders  []string `json:"orders"`
	Details bool     `json:"details"`
}

// GetTransactions fetches payment transaction details for the given order
// UUIDs. Requires auth.
func (s *Service) GetTransactions(ctx context.Context, in GetTransactionsInput) (any, error) {
	if len(in.Orders) == 0 {
		return nil, fmt.Errorf("orders (list of order UUIDs) is required")
	}
	q := url.Values{}
	if in.Details {
		q.Set("details", "true")
	}
	return s.client.JSONAuth(ctx, http.MethodPost, "/payment/v2/transactions", q,
		map[string]any{"orders": in.Orders}, nil)
}

type CreateOrderInput struct {
	Body map[string]any `json:"body"`
}

// CreateOrder creates an order from a basket payload (bundles, timeSlot,
// customers, payment, user, device) and returns the created order (incl. its
// UUID). This is step 1 of checkout; it does not charge the customer. Requires
// auth.
func (s *Service) CreateOrder(ctx context.Context, in CreateOrderInput) (any, error) {
	if len(in.Body) == 0 {
		return nil, fmt.Errorf("body (order payload) is required")
	}
	return s.client.JSONAuth(ctx, http.MethodPost, "/order/v1/commands/create", nil, in.Body,
		map[string]string{"store": gails.DefaultStoreUUID, "menu": gails.DefaultMenuUUID})
}

type PlaceOrderInput struct {
	BundleID string         `json:"bundle_id"`
	Store    string         `json:"store"`
	Menu     string         `json:"menu"`
	TimeSlot map[string]any `json:"timeslot"`
	DateMs   int64          `json:"date_ms"`
	EatIn    bool           `json:"eat_in"`
	DryRun   bool           `json:"dry_run"`
}

// PlaceOrder assembles a complete basket for a single bundle and creates the
// order. get_order/create_order need the full VMOS basket shape (user,
// customers, and each bundle grouped into itemTypes[].items[] with
// finalPrice/subtotals); this builds that from the bundle detail + the
// signed-in user, so callers don't hand-craft it. With dry_run=true it returns
// the assembled payload WITHOUT creating an order (no charge, nothing placed)
// so it can be inspected first. Requires auth.
func (s *Service) PlaceOrder(ctx context.Context, in PlaceOrderInput) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if in.BundleID == "" {
		return nil, fmt.Errorf("bundle_id is required")
	}
	if len(in.TimeSlot) == 0 {
		return nil, fmt.Errorf("timeslot is required (pass a slot object from get_timeslots)")
	}
	store := in.Store
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	menu := in.Menu
	if menu == "" {
		menu = gails.DefaultMenuUUID
	}
	headers := catalogHeaders(store, menu, "")

	user, err := s.client.UserInfo(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("selectAllBundleItems", "1")
	q.Set("provideIsInLayoutStatus", "1")
	q.Set("forceStockStatus", "1")
	rawBundle, err := s.client.GetJSON(ctx, "/catalog/bundles/"+url.PathEscape(in.BundleID), q, headers)
	if err != nil {
		return nil, err
	}
	bundle, ok := rawBundle.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected bundle response")
	}

	basket, price, err := buildBasketBundle(bundle, store, menu, in.EatIn)
	if err != nil {
		return nil, err
	}

	name := user.FirstName
	if name == "" {
		name = "Customer"
	}
	customer := map[string]any{
		"uuid":         user.UUID,
		"email":        user.Email,
		"name":         name,
		"memberNumber": user.MemberNumber,
	}
	payload := map[string]any{
		"timeSlot":                       in.TimeSlot,
		"takeaway":                       !in.EatIn,
		"note":                           "",
		"accessories":                    []any{},
		"bundles":                        []any{basket},
		"customers":                      []any{customer},
		"dateSlot":                       in.DateMs,
		"isAdditionalInformationEnabled": false,
		"isAsap":                         false,
		"isDelivery":                     false,
		"isOpat":                         false,
		"locale":                         gails.DefaultLocale,
		"orderType":                      "takeaway",
		"payment": map[string]any{
			"totalAmount":             price,
			"subtotalAmount":          price,
			"serviceCharge":           0,
			"serviceChargePercentage": nil,
			"price":                   price,
			"discount":                0,
		},
		"promotions": []any{},
		"toggleCard": nil,
		"user": map[string]any{
			"name":               name,
			"email":              user.Email,
			"extUserUUID":        user.UUID,
			"acteolMemberNumber": user.ActeolMemberNum,
			"address":            map[string]any{"phoneNumber": user.Phone},
		},
		"device": map[string]any{
			"appVersion":     nil,
			"deviceType":     nil,
			"platform":       "web",
			"productVersion": "2.1605.1",
		},
	}

	if in.DryRun {
		return map[string]any{
			"dryRun":      true,
			"wouldCharge": price,
			"payload":     payload,
		}, nil
	}
	return s.client.JSONAuth(ctx, http.MethodPost, "/order/v1/commands/create", nil, payload,
		map[string]string{"store": store, "menu": menu})
}

// buildBasketBundle converts a /catalog/bundles detail object into the basket
// bundle shape the create endpoint expects: items grouped under
// itemTypes[].items[], with the base item's default variation selected and
// finalPrice/subtotal fields populated. Returns the basket bundle and its
// effective price.
func buildBasketBundle(b map[string]any, store, menu string, eatIn bool) (map[string]any, float64, error) {
	items := asSlice(b["items"])
	if len(items) == 0 {
		return nil, 0, fmt.Errorf("bundle has no items")
	}

	priceKey := "price"
	if eatIn {
		priceKey = "priceEatIn"
	}

	// Determine the base item's selected-variation price.
	var price float64
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		if t, _ := im["type"].(string); t != "BUNDLE_BASE" {
			continue
		}
		for _, c := range asSlice(im["customizations"]) {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			vars := asSlice(cm["variations"])
			def, _ := (cm["meta"].(map[string]any))["defaultValue"].(string)
			for _, v := range vars {
				vm, _ := v.(map[string]any)
				if vm == nil {
					continue
				}
				vp := asNum(vm[priceKey])
				if vp == 0 {
					vp = asNum(vm["price"])
				}
				if def != "" {
					if id, _ := vm["uuid"].(string); id == def && vp > 0 {
						price = vp
					}
				} else if vp > 0 && price == 0 {
					price = vp
				}
			}
		}
		if im["finalPrice"] == nil {
			im["finalPrice"] = price
		}
	}
	if price == 0 {
		if p := effectivePrice(b); p != nil {
			price = *p
		}
	}

	// Group items under their itemType.
	order := []string{}
	byType := map[string][]any{}
	typeObj := map[string]map[string]any{}
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		itp, _ := im["itemType"].(map[string]any)
		if itp == nil {
			continue
		}
		id, _ := itp["uuid"].(string)
		if _, seen := typeObj[id]; !seen {
			typeObj[id] = itp
			order = append(order, id)
		}
		byType[id] = append(byType[id], im)
	}
	var itemTypes []any
	for _, id := range order {
		t := map[string]any{}
		for k, v := range typeObj[id] {
			t[k] = v
		}
		t["items"] = byType[id]
		itemTypes = append(itemTypes, t)
	}

	// defaultItems: the base item(s) with their current size selection.
	var defaultItems []any
	for _, it := range items {
		im, _ := it.(map[string]any)
		if im == nil {
			continue
		}
		if t, _ := im["type"].(string); t != "BUNDLE_BASE" {
			continue
		}
		defaultItems = append(defaultItems, map[string]any{
			"itemUUID":       im["itemUUID"],
			"name":           im["name"],
			"customizations": []any{map[string]any{"current": 0, "type": "size"}},
		})
	}

	basket := map[string]any{}
	for k, v := range b {
		basket[k] = v
	}
	basket["itemTypes"] = itemTypes
	basket["basketUUID"] = randomID()
	basket["finalPrice"] = price
	basket["price"] = 0
	basket["priceEatIn"] = 0
	basket["subtotalAmount"] = price
	basket["subtotalAmountIncludingTax"] = price
	basket["storeUUID"] = store
	basket["menuUUID"] = menu
	basket["defaultItems"] = defaultItems
	basket["isRecommendation"] = false
	return basket, price, nil
}

// randomID returns a short nanoid-like token for basketUUID. It avoids
// crypto/rand-vs-determinism concerns; uniqueness within an order is enough.
func randomID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 21)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("basket-%d", len(alphabet))
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

type InitiatePaymentInput struct {
	Body map[string]any `json:"body"`
}

// InitiatePayment starts payment for an order (step 2 of checkout). The body
// carries providers[] including an Adyen-encrypted paymentMethod (the
// encrypted card / CVC blobs produced client-side by Adyen Web), browserInfo,
// riskData, and order:{uuid,amount}. It returns an Adyen 3DS action whose
// result feeds confirm_payment. Requires auth.
func (s *Service) InitiatePayment(ctx context.Context, in InitiatePaymentInput) (any, error) {
	if len(in.Body) == 0 {
		return nil, fmt.Errorf("body (payment payload incl. providers[] and order) is required")
	}
	return s.client.JSONAuth(ctx, http.MethodPost, "/payment/v2/transactions/order", nil, in.Body, nil)
}

type Decode3DSActionInput struct {
	Token string `json:"token"`
}

// Decode3DSAction decodes a 3DS2 action token to reveal its contents
// (threeDSMethodURL / acsURL / CReq etc.), useful for building a challenge URL.
// No authentication required.
func (s *Service) Decode3DSAction(ctx context.Context, in Decode3DSActionInput) (any, error) {
	if in.Token == "" {
		return nil, fmt.Errorf("token is required (the action.token from initiate_payment)")
	}
	return adyen.DecodeToken(in.Token)
}

type Submit3DSFingerprintInput struct {
	PaymentData    string `json:"payment_data"`
	ThreeDSCompInd string `json:"three_ds_comp_ind"`
	ClientKey      string `json:"client_key"`
}

// SubmitFingerprint runs the 3DS2 fingerprint step server-side. For a saved
// low-value card this often resolves frictionlessly (Authorised); otherwise it
// returns a challenge action. Requires no auth (talks directly to Adyen).
func (s *Service) SubmitFingerprint(ctx context.Context, in Submit3DSFingerprintInput) (any, error) {
	raw, err := adyen.SubmitFingerprint(ctx, extHTTP, in.ClientKey, in.PaymentData, in.ThreeDSCompInd)
	if err != nil {
		return nil, err
	}
	return decodeRaw(raw), nil
}

type Submit3DSChallengeInput struct {
	PaymentData string `json:"payment_data"`
	TransStatus string `json:"trans_status"`
	ClientKey   string `json:"client_key"`
}

// SubmitChallenge submits the 3DS2 challenge result after the shopper has
// authenticated with their bank.
func (s *Service) SubmitChallenge(ctx context.Context, in Submit3DSChallengeInput) (any, error) {
	raw, err := adyen.SubmitChallenge(ctx, extHTTP, in.ClientKey, in.PaymentData, in.TransStatus)
	if err != nil {
		return nil, err
	}
	return decodeRaw(raw), nil
}

func decodeRaw(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return v
}

type Prepare3DSInput struct {
	Action          map[string]any `json:"action"`
	OrderUUID       string         `json:"order_uuid"`
	TransactionUUID string         `json:"transaction_uuid"`
	Amount          float64        `json:"amount"`
	Store           string         `json:"store"`
}

// Prepare3DS hands a 3-D Secure action (returned by pay_with_stored_card when
// the bank requires verification) to the companion challenge server, which
// returns a clickable URL. The shopper opens it, completes the bank's check
// (e.g. approves in their banking app), and the companion server calls
// /confirm to finalise the order. The companion server URL is configurable via
// GAILS_3DS_SERVER.
func (s *Service) Prepare3DS(ctx context.Context, in Prepare3DSInput) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if len(in.Action) == 0 {
		return nil, fmt.Errorf("action is required (the 3DS `action` object from pay_with_stored_card)")
	}
	if in.OrderUUID == "" || in.TransactionUUID == "" {
		return nil, fmt.Errorf("order_uuid and transaction_uuid are required")
	}

	// Default path: embedded challenge server + auto ngrok tunnel (no external
	// server needed). Set GAILS_3DS_SERVER to use an external companion server.
	server := os.Getenv("GAILS_3DS_SERVER")
	if server == "" {
		out, err := s.tunnelMgr().Prepare(ctx, tunnel.RecordInput{
			Action:          in.Action,
			OrderUUID:       in.OrderUUID,
			TransactionUUID: in.TransactionUUID,
			Store:           in.Store,
			Amount:          in.Amount,
		})
		if err != nil {
			return nil, err
		}
		out["instructions"] = "Open pay_url in a browser and complete the bank's verification; the order confirms automatically. Poll status_url (or get_transactions) to check completion."
		return out, nil
	}

	body := map[string]any{
		"action":          in.Action,
		"orderUUID":       in.OrderUUID,
		"transactionUUID": in.TransactionUUID,
		"amount":          in.Amount,
	}
	if in.Store != "" {
		body["store"] = in.Store
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/prepare", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := extHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("challenge server unreachable at %s: %w", server, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("challenge server returned non-JSON (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("challenge server error (HTTP %d): %v", resp.StatusCode, out)
	}
	out["instructions"] = "Open pay_url in a browser and complete the bank's verification; the order confirms automatically. Poll status_url (or get_transactions) to check completion."
	return out, nil
}

type ConfirmPaymentInput struct {
	OrderUUID       string         `json:"order_uuid"`
	TransactionUUID string         `json:"transaction_uuid"`
	Details         map[string]any `json:"details"`
}

// ConfirmPayment confirms (finalises) a payment for an order — e.g. submitting
// a 3DS result. This places a real, paid order; use with care. Requires auth.
func (s *Service) ConfirmPayment(ctx context.Context, in ConfirmPaymentInput) (any, error) {
	if in.OrderUUID == "" || in.TransactionUUID == "" {
		return nil, fmt.Errorf("order_uuid and transaction_uuid are required")
	}
	q := url.Values{}
	q.Set("transactionUUID", in.TransactionUUID)
	path := "/payment/v2/transactions/order/" + url.PathEscape(in.OrderUUID) + "/confirm"
	return s.client.JSONAuth(ctx, http.MethodPost, path, q, map[string]any{"details": in.Details}, nil)
}

// --- Adyen client-side encryption ----------------------------------------

type AdyenEncryptInput struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// AdyenEncrypt encrypts a single card field (e.g. "cvc", "number",
// "expiryMonth", "expiryYear") into an Adyen JWE blob, matching the blobs the
// web checkout produces. No authentication required.
func (s *Service) AdyenEncrypt(ctx context.Context, in AdyenEncryptInput) (any, error) {
	field := in.Field
	if field == "" {
		field = "cvc"
	}
	if in.Value == "" {
		return nil, fmt.Errorf("value is required")
	}
	e, err := s.encryptor(ctx)
	if err != nil {
		return nil, err
	}
	blob, err := e.EncryptField(field, in.Value, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]string{"field": field, "encrypted": blob}, nil
}

type PayWithStoredCardInput struct {
	OrderUUID             string         `json:"order_uuid"`
	Amount                float64        `json:"amount"`
	StoredPaymentMethodID string         `json:"stored_payment_method_id"`
	Brand                 string         `json:"brand"`
	CVC                   string         `json:"cvc"`
	HolderName            string         `json:"holder_name"`
	Store                 string         `json:"store"`
	RiskClientData        string         `json:"risk_client_data"`
	RedirectURL           string         `json:"redirect_url"`
	BrowserInfo           map[string]any `json:"browser_info"`
	ManualOnly            bool           `json:"manual_only"`
}

// PayWithStoredCard initiates payment for an order using a saved card. It
// CSE-encrypts the CVC and assembles the providers[] payload, then calls
// /payment/v2/transactions/order. WARNING: this attempts a real charge and,
// once any required 3DS completes, places a paid order. Requires auth.
//
// The CVC may be supplied via the cvc argument or the GAILS_CVC environment
// variable (preferred, so it never appears in tool arguments).
func (s *Service) PayWithStoredCard(ctx context.Context, in PayWithStoredCardInput) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if in.OrderUUID == "" {
		return nil, fmt.Errorf("order_uuid is required")
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("amount must be greater than 0")
	}
	if in.StoredPaymentMethodID == "" {
		return nil, fmt.Errorf("stored_payment_method_id is required (see get_payment_methods)")
	}
	cvc := in.CVC
	if cvc == "" {
		cvc = os.Getenv("GAILS_CVC")
	}
	if cvc == "" {
		return nil, fmt.Errorf("cvc is required (pass cvc or set GAILS_CVC)")
	}

	e, err := s.encryptor(ctx)
	if err != nil {
		return nil, err
	}
	encryptedCVC, err := e.EncryptField("cvc", cvc, time.Now())
	if err != nil {
		return nil, err
	}

	store := in.Store
	if store == "" {
		store = gails.DefaultStoreUUID
	}
	redirectURL := in.RedirectURL
	if redirectURL == "" {
		redirectURL = fmt.Sprintf("https://gails.vmos.io/store/%s/order-confirmation/%s", store, in.OrderUUID)
	}
	browserInfo := in.BrowserInfo
	if browserInfo == nil {
		browserInfo = map[string]any{
			"acceptHeader":   "*/*",
			"colorDepth":     30,
			"language":       "en-GB",
			"javaEnabled":    false,
			"screenHeight":   982,
			"screenWidth":    1512,
			"userAgent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
			"timeZoneOffset": -60,
		}
	}

	meta := map[string]any{
		"storeUUID":                store,
		"returnUrl":                nil,
		"redirectUrl":              redirectURL,
		"origin":                   "https://gails.vmos.io",
		"browserInfo":              browserInfo,
		"redirectFromIssuerMethod": "POST",
		"redirectToIssuerMethod":   "GET",
		"channel":                  nil,
	}
	if in.RiskClientData != "" {
		meta["riskData"] = map[string]any{"clientData": in.RiskClientData}
	}

	paymentMethod := map[string]any{
		"type":                  "scheme",
		"holderName":            in.HolderName,
		"encryptedSecurityCode": encryptedCVC,
		"storedPaymentMethodId": in.StoredPaymentMethodID,
	}
	if in.Brand != "" {
		paymentMethod["brand"] = in.Brand
	}

	body := map[string]any{
		"providers": []any{map[string]any{
			"amount":        in.Amount,
			"meta":          meta,
			"paymentMethod": paymentMethod,
			"uuid":          gails.DefaultPaymentProviderUUID,
		}},
		"order": map[string]any{"uuid": in.OrderUUID, "amount": in.Amount},
	}
	initResp, err := s.client.JSONAuth(ctx, http.MethodPost, "/payment/v2/transactions/order", nil, body,
		map[string]string{"store": store})
	if err != nil {
		return nil, err
	}

	out := map[string]any{"initiate": initResp}
	resultCode := deepFindString(initResp, "resultCode")
	txn := deepFindString(initResp, "transactionUUID", "transactionUuid")
	if txn == "" {
		txn = deepFindString(deepFind(initResp, "transaction", "transactions"), "uuid")
	}
	out["resultCode"] = resultCode
	out["transactionUUID"] = txn

	action, _ := deepFind(initResp, "action").(map[string]any)
	if action == nil {
		// No 3DS action — either authorised outright or nothing to advance.
		out["status"] = "authorised_or_no_action"
		return out, nil
	}
	if in.ManualOnly {
		out["status"] = "action_required"
		out["action"] = action
		return out, nil
	}

	// Frictionless-first: complete the device fingerprint server-side.
	pd := deepFindString(action, "paymentData")
	if pd == "" {
		pd = deepFindString(initResp, "paymentData")
	}
	fpResp, ferr := s.submitFingerprint(ctx, pd, "N")
	if ferr != nil {
		out["status"] = "action_required"
		out["fingerprintError"] = ferr.Error()
		out["action"] = action
		return out, nil
	}
	out["fingerprint"] = fpResp

	// Escalated to an interactive challenge? Hand the ORIGINAL action to the
	// browser so Adyen Web runs the full 3DS in-browser (fingerprint +
	// challenge) with real device data — the bank's challenge page only frames
	// correctly when the browser did the fingerprint. Passing the server-side
	// challenge action instead makes the ACS serve an error page (which is
	// X-Frame-Options: SAMEORIGIN → "refused to connect").
	if ch, _ := deepFind(fpResp, "action").(map[string]any); ch != nil && deepFindString(ch, "subtype") == "challenge" {
		out["status"] = "3ds_challenge_required"
		if prep, perr := s.Prepare3DS(ctx, Prepare3DSInput{Action: action, OrderUUID: in.OrderUUID, TransactionUUID: txn, Amount: in.Amount, Store: store}); perr != nil {
			out["challengeError"] = perr.Error()
			out["challengeAction"] = action
		} else {
			out["challenge"] = prep
		}
		return out, nil
	}

	// Frictionless: confirm with the threeDSResult if present.
	if tds := deepFindString(fpResp, "threeDSResult"); tds != "" && txn != "" {
		confRes, cerr := s.confirmOrder(ctx, in.OrderUUID, txn, store, map[string]any{"threeDSResult": tds})
		if cerr != nil {
			out["status"] = "fingerprint_done_confirm_failed"
			out["confirmError"] = cerr.Error()
			return out, nil
		}
		out["confirm"] = confRes
		out["status"] = "paid"
		return out, nil
	}

	if rc := deepFindString(fpResp, "resultCode"); rc != "" {
		out["fingerprintResultCode"] = rc
	}
	out["status"] = "advanced"
	return out, nil
}

// --- 3-D Secure helpers (frictionless-first) ------------------------------

// deepFind returns the first value found under any of the given keys, searching
// maps and slices recursively. Used to read fields out of Adyen/VMOS responses
// whose exact nesting we don't want to hard-code.
func deepFind(node any, keys ...string) any {
	switch v := node.(type) {
	case map[string]any:
		for _, k := range keys {
			if val, ok := v[k]; ok && val != nil {
				return val
			}
		}
		for _, val := range v {
			if got := deepFind(val, keys...); got != nil {
				return got
			}
		}
	case []any:
		for _, item := range v {
			if got := deepFind(item, keys...); got != nil {
				return got
			}
		}
	}
	return nil
}

func deepFindString(node any, keys ...string) string {
	if s, ok := deepFind(node, keys...).(string); ok {
		return s
	}
	return ""
}

// submitFingerprint completes the 3DS2 device-fingerprint step server-side by
// reporting threeDSCompInd (default "N" — no browser ran the method). Adyen
// then either authorises (frictionless) or returns a challenge action.
func (s *Service) submitFingerprint(ctx context.Context, paymentData, compInd string) (any, error) {
	if compInd == "" {
		compInd = "N"
	}
	clientKey := os.Getenv("GAILS_ADYEN_CLIENT_KEY")
	if clientKey == "" {
		clientKey = adyen.DefaultClientKey
	}
	fp := base64.StdEncoding.EncodeToString([]byte(`{"threeDSCompInd":"` + compInd + `"}`))
	body, _ := json.Marshal(map[string]any{"fingerprintResult": fp, "paymentData": paymentData})
	u := "https://checkoutshopper-live.adyen.com/checkoutshopper/v1/submitThreeDS2Fingerprint?token=" + url.QueryEscape(clientKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("origin", "https://gails.vmos.io")
	req.Header.Set("referer", "https://gails.vmos.io/")
	resp, err := extHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("submitThreeDS2Fingerprint failed: %w", err)
	}
	defer resp.Body.Close()
	var out any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("submitThreeDS2Fingerprint: bad response (HTTP %d)", resp.StatusCode)
	}
	return out, nil
}

// geocodePostcode resolves a UK postcode to lat/long via the free postcodes.io
// API. The Gail's store finder requires coordinates even when a postcode is
// given, so this fills them in transparently.
func geocodePostcode(ctx context.Context, postcode string) (lat, long float64, err error) {
	pc := url.PathEscape(strings.ReplaceAll(postcode, " ", ""))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.postcodes.io/postcodes/"+pc, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := extHTTP.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("postcodes.io returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Result struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, 0, err
	}
	if out.Result.Latitude == 0 && out.Result.Longitude == 0 {
		return 0, 0, fmt.Errorf("no coordinates returned")
	}
	return out.Result.Latitude, out.Result.Longitude, nil
}
