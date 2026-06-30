// Package app contains the tool-facing service methods. Each method maps one
// MCP tool to one or more Gail's backend calls and returns decoded JSON.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/60xAI/gails-bakery-mcp/internal/gails"
)

type Service struct {
	client *gails.Client
}

func NewService(client *gails.Client) *Service {
	return &Service{client: client}
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
	return s.client.GetJSON(ctx, "/tenant/v1/stores/tenant", q, nil)
}

type GetMenuInput struct {
	Store  string `json:"store"`
	Menu   string `json:"menu"`
	Locale string `json:"locale"`
}

func (s *Service) GetMenu(ctx context.Context, in GetMenuInput) (any, error) {
	return s.client.GetJSON(ctx, "/catalog/v2/menu", nil, catalogHeaders(in.Store, in.Menu, in.Locale))
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

// geocodePostcode resolves a UK postcode to lat/long via the free postcodes.io
// API. The Gail's store finder requires coordinates even when a postcode is
// given, so this fills them in transparently.
func geocodePostcode(ctx context.Context, postcode string) (lat, long float64, err error) {
	pc := url.PathEscape(strings.ReplaceAll(postcode, " ", ""))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.postcodes.io/postcodes/"+pc, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
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
