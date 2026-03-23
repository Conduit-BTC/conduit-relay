package conduitl2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip11"
)

type ProductSort string

const (
	SortNewest        ProductSort = "newest"
	SortPriceAsc      ProductSort = "price_asc"
	SortPriceDesc     ProductSort = "price_desc"
	SortUpdatedAtDesc ProductSort = "updated_at_desc"
)

type Scope2Options struct {
	MaxQueryLimit          int
	DefaultQueryLimit      int
	MaxProjectionScan      int
	AllowMixedCurrencySort bool
	EnableNIP50            bool
}

type StoreQuery func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event]

func ConfigureRelay(relay *khatru.Relay, opts Scope2Options) {
	withDefaults(&opts)

	relay.Info.AddSupportedNIPs([]int{17, 59})
	if opts.EnableNIP50 {
		relay.Info.AddSupportedNIP(50)
	}

	prevInfo := relay.OverwriteRelayInformation
	relay.OverwriteRelayInformation = func(ctx context.Context, r *http.Request, info nip11.RelayInformationDocument) nip11.RelayInformationDocument {
		if prevInfo != nil {
			info = prevInfo(ctx, r, info)
		}

		info.Tags = appendUnique(info.Tags,
			"conduit_l2",
			"scope2-mvp",
			"marketplace_product_browse",
			"merchant_storefront_browse",
			"product_detail_resolution",
			"profile_decoration_lookup",
			"sort:newest",
			"sort:price_asc",
			"sort:price_desc",
			"sort:updated_at_desc",
			"cursor:conduit-l2-v1",
			"search_scope:bounded_commerce_search",
			"protected_kind:1059",
		)

		return info
	}

	prevQuery := relay.QueryStored
	if prevQuery != nil {
		relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
			applyRequestBounds(&filter, opts)
			return prevQuery(ctx, filter)
		}
	}

	prevRequest := relay.OnRequest
	relay.OnRequest = func(ctx context.Context, filter nostr.Filter) (bool, string) {
		if prevRequest != nil {
			if reject, msg := prevRequest(ctx, filter); reject {
				return true, msg
			}
		}

		if requestsProtectedGiftWraps(filter) {
			if _, authed := khatru.GetAuthed(ctx); !authed {
				return true, "auth-required: kind 1059 requests require NIP-42 authentication"
			}
		}

		if requestsOutOfScopeSearch(filter) {
			return true, "blocked: use conduit-l2 capability search format"
		}

		return false, ""
	}
}

func appendUnique(target []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(target, value) {
			target = append(target, value)
		}
	}
	return target
}

func WrapProductQueries(baseQuery StoreQuery, opts Scope2Options) StoreQuery {
	withDefaults(&opts)

	return func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		plan, hasPlan := parseSearchPlan(filter.Search)
		if !hasPlan || !isProductQuery(filter) {
			return baseQuery(ctx, filter)
		}

		base := filter.Clone()
		base.Search = ""
		base.LimitZero = false
		base.Limit = opts.MaxProjectionScan

		raw := make([]nostr.Event, 0, opts.MaxProjectionScan)
		for evt := range baseQuery(ctx, base) {
			if evt.Kind != 30402 {
				continue
			}
			if plan.Text != "" {
				title, summary := extractProductText(evt.Content)
				needle := strings.ToLower(plan.Text)
				if !strings.Contains(strings.ToLower(title), needle) && !strings.Contains(strings.ToLower(summary), needle) {
					continue
				}
			}
			raw = append(raw, evt)
		}

		products := dedupeLatestProducts(raw)
		products = sortProducts(products, plan.Sort, opts.AllowMixedCurrencySort, plan.Partial)

		start := 0
		if plan.Cursor != "" {
			start = findCursorStart(products, plan.Cursor)
		}

		if start >= len(products) {
			return emptySeq()
		}

		limit := resolvedLimit(filter, opts)
		end := start + limit
		if end > len(products) {
			end = len(products)
		}
		page := products[start:end]

		return func(yield func(nostr.Event) bool) {
			for _, pe := range page {
				if !yield(pe.Event) {
					return
				}
			}
		}
	}
}

func BuildNextCursor(last nostr.Event) string {
	b, _ := json.Marshal(cursorState{ID: last.ID.Hex()})
	return base64.RawURLEncoding.EncodeToString(b)
}

func emptySeq() iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {}
}

type productEnvelope struct {
	Event     nostr.Event
	UpdatedAt nostr.Timestamp
	Price     float64
	Currency  string
	HasPrice  bool
}

type searchPlan struct {
	Sort    ProductSort
	Text    string
	Cursor  string
	Partial bool
}

func withDefaults(opts *Scope2Options) {
	if opts.MaxQueryLimit <= 0 {
		opts.MaxQueryLimit = 100
	}
	if opts.DefaultQueryLimit <= 0 {
		opts.DefaultQueryLimit = 50
	}
	if opts.MaxProjectionScan <= 0 {
		opts.MaxProjectionScan = 2000
	}
}

func applyRequestBounds(filter *nostr.Filter, opts Scope2Options) {
	if filter.LimitZero {
		return
	}
	if filter.Limit <= 0 {
		filter.Limit = opts.DefaultQueryLimit
	}
	if filter.Limit > opts.MaxQueryLimit {
		filter.Limit = opts.MaxQueryLimit
	}
}

func requestsProtectedGiftWraps(filter nostr.Filter) bool {
	for _, kind := range filter.Kinds {
		if kind == 1059 {
			return true
		}
	}
	return false
}

func requestsOutOfScopeSearch(filter nostr.Filter) bool {
	if filter.Search == "" {
		return false
	}
	_, ok := parseSearchPlan(filter.Search)
	return !ok
}

func parseSearchPlan(raw string) (searchPlan, bool) {
	plan := searchPlan{Sort: SortNewest}
	if raw == "" || !strings.HasPrefix(raw, "conduit-l2:") {
		return plan, false
	}

	body := strings.TrimPrefix(raw, "conduit-l2:")
	for _, token := range strings.Split(body, ";") {
		k, v, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)

		switch k {
		case "q":
			plan.Text = v
		case "sort":
			sort := ProductSort(v)
			if sort == SortNewest || sort == SortPriceAsc || sort == SortPriceDesc || sort == SortUpdatedAtDesc {
				plan.Sort = sort
			}
		case "cursor":
			plan.Cursor = v
		case "partial":
			plan.Partial = v == "1" || strings.EqualFold(v, "true")
		}
	}

	return plan, true
}

func isProductQuery(filter nostr.Filter) bool {
	for _, kind := range filter.Kinds {
		if kind == 30402 {
			return true
		}
	}
	return false
}

func dedupeLatestProducts(events []nostr.Event) []productEnvelope {
	byAddress := make(map[string]productEnvelope, len(events))
	for _, evt := range events {
		address := fmt.Sprintf("%d:%s:%s", evt.Kind, evt.PubKey, evt.Tags.GetD())
		if evt.Tags.GetD() == "" {
			address = evt.ID.Hex()
		}

		if current, ok := byAddress[address]; ok {
			if evt.CreatedAt < current.Event.CreatedAt {
				continue
			}
			if evt.CreatedAt == current.Event.CreatedAt && evt.ID.Hex() > current.Event.ID.Hex() {
				continue
			}
		}

		price, currency, hasPrice := extractPrice(evt.Content)
		byAddress[address] = productEnvelope{
			Event:     evt,
			UpdatedAt: extractUpdatedAt(evt),
			Price:     price,
			Currency:  currency,
			HasPrice:  hasPrice,
		}
	}

	out := make([]productEnvelope, 0, len(byAddress))
	for _, pe := range byAddress {
		out = append(out, pe)
	}
	return out
}

func sortProducts(events []productEnvelope, mode ProductSort, allowMixedCurrency bool, partial bool) []productEnvelope {
	if mode == SortPriceAsc || mode == SortPriceDesc {
		currencies := map[string]struct{}{}
		for _, pe := range events {
			if pe.HasPrice && pe.Currency != "" {
				currencies[pe.Currency] = struct{}{}
			}
		}

		if len(currencies) > 1 && !allowMixedCurrency && partial {
			seed := ""
			for c := range currencies {
				seed = c
				break
			}
			trimmed := make([]productEnvelope, 0, len(events))
			for _, pe := range events {
				if pe.HasPrice && pe.Currency == seed {
					trimmed = append(trimmed, pe)
				}
			}
			events = trimmed
		}
	}

	slices.SortFunc(events, func(a, b productEnvelope) int {
		if mode == SortPriceAsc || mode == SortPriceDesc {
			if a.HasPrice != b.HasPrice {
				if a.HasPrice {
					return -1
				}
				return 1
			}
			if a.HasPrice && b.HasPrice {
				if a.Price < b.Price {
					if mode == SortPriceAsc {
						return -1
					}
					return 1
				}
				if a.Price > b.Price {
					if mode == SortPriceAsc {
						return 1
					}
					return -1
				}
			}
		}

		if mode == SortUpdatedAtDesc {
			if a.UpdatedAt > b.UpdatedAt {
				return -1
			}
			if a.UpdatedAt < b.UpdatedAt {
				return 1
			}
		}

		if a.Event.CreatedAt > b.Event.CreatedAt {
			return -1
		}
		if a.Event.CreatedAt < b.Event.CreatedAt {
			return 1
		}
		if a.Event.ID.Hex() < b.Event.ID.Hex() {
			return -1
		}
		if a.Event.ID.Hex() > b.Event.ID.Hex() {
			return 1
		}
		return 0
	})

	return events
}

type cursorState struct {
	ID string `json:"id"`
}

func findCursorStart(products []productEnvelope, rawCursor string) int {
	b, err := base64.RawURLEncoding.DecodeString(rawCursor)
	if err != nil {
		return 0
	}

	var state cursorState
	if err := json.Unmarshal(b, &state); err != nil {
		return 0
	}

	for i, p := range products {
		if p.Event.ID.Hex() == state.ID {
			return i + 1
		}
	}

	return 0
}

func resolvedLimit(filter nostr.Filter, opts Scope2Options) int {
	if filter.Limit <= 0 {
		return opts.DefaultQueryLimit
	}
	if filter.Limit > opts.MaxQueryLimit {
		return opts.MaxQueryLimit
	}
	return filter.Limit
}

func extractProductText(content string) (string, string) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", ""
	}

	title, _ := payload["title"].(string)
	summary, _ := payload["summary"].(string)
	return title, summary
}

func extractPrice(content string) (float64, string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return 0, "", false
	}

	raw, ok := payload["price"]
	if !ok {
		return 0, "", false
	}
	price, ok := toFloat(raw)
	if !ok {
		return 0, "", false
	}

	currency, _ := payload["currency"].(string)
	return price, strings.ToUpper(strings.TrimSpace(currency)), true
}

func extractUpdatedAt(evt nostr.Event) nostr.Timestamp {
	var payload map[string]any
	if err := json.Unmarshal([]byte(evt.Content), &payload); err == nil {
		if raw, ok := payload["updatedAt"]; ok {
			if f, ok := toFloat(raw); ok {
				return nostr.Timestamp(int64(f))
			}
			if s, ok := raw.(string); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					return nostr.Timestamp(n)
				}
			}
		}
	}

	return evt.CreatedAt
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}
