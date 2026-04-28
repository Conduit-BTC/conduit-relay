package conduitl2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

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

type productEnvelope struct {
	Address    string
	Event      nostr.Event
	UpdatedAt  nostr.Timestamp
	Price      float64
	Currency   string
	HasPrice   bool
	SearchText string
	Tombstoned bool
}

type searchPlan struct {
	Sort    ProductSort
	Text    string
	Cursor  string
	Partial bool
}

type cursorState struct {
	ID string `json:"id"`
}

type pricePolicy struct {
	Allowed         bool
	PartialCurrency string
	Reason          string
}

type scope2State struct {
	mu       sync.RWMutex
	products map[string]productEnvelope
}

var (
	scope2RegistryMu sync.RWMutex
	scope2Registry   = map[uintptr]*scope2State{}
)

func ConfigureRelay(relay *khatru.Relay, opts Scope2Options) {
	withDefaults(&opts)

	prevQuery := relay.QueryStored
	state := newScope2State(prevQuery)
	registerScope2State(prevQuery, state)

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

	prevSaved := relay.OnEventSaved
	relay.OnEventSaved = func(ctx context.Context, event nostr.Event) {
		state.applyEvent(event)
		if prevSaved != nil {
			prevSaved(ctx, event)
		}
	}

	prevDeleted := relay.OnEventDeleted
	relay.OnEventDeleted = func(ctx context.Context, deleted nostr.Event) {
		state.applyDeletedEvent(deleted)
		if prevDeleted != nil {
			prevDeleted(ctx, deleted)
		}
	}

	if prevQuery != nil {
		relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
			applyRequestBounds(&filter, opts)
			return prevQuery(ctx, filter)
		}
		registerScope2State(relay.QueryStored, state)
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

		plan, hasPlan := parseSearchPlan(filter.Search)
		if hasPlan && isProductQuery(filter) {
			filter = filter.Clone()
			applyRequestBounds(&filter, opts)
			if _, reason := state.evaluate(filter, plan, opts); reason != "" {
				return true, reason
			}
		}

		return false, ""
	}
}

func WrapProductQueries(baseQuery StoreQuery, opts Scope2Options) StoreQuery {
	withDefaults(&opts)

	state := lookupScope2State(baseQuery)
	if state == nil {
		state = newScope2State(baseQuery)
	}

	return func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		plan, hasPlan := parseSearchPlan(filter.Search)
		if !hasPlan || !isProductQuery(filter) {
			return baseQuery(ctx, filter)
		}

		filter = filter.Clone()
		applyRequestBounds(&filter, opts)

		products, reason := state.evaluate(filter, plan, opts)
		if reason != "" {
			return emptySeq()
		}

		return func(yield func(nostr.Event) bool) {
			for _, pe := range products {
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

func newScope2State(baseQuery StoreQuery) *scope2State {
	state := &scope2State{
		products: make(map[string]productEnvelope),
	}
	state.seed(baseQuery)
	return state
}

func registerScope2State(query StoreQuery, state *scope2State) {
	if query == nil || state == nil {
		return
	}

	scope2RegistryMu.Lock()
	scope2Registry[queryKey(query)] = state
	scope2RegistryMu.Unlock()
}

func lookupScope2State(query StoreQuery) *scope2State {
	if query == nil {
		return nil
	}

	scope2RegistryMu.RLock()
	state := scope2Registry[queryKey(query)]
	scope2RegistryMu.RUnlock()
	return state
}

func queryKey(query StoreQuery) uintptr {
	return reflect.ValueOf(query).Pointer()
}

func (s *scope2State) seed(baseQuery StoreQuery) {
	if baseQuery == nil {
		return
	}

	events := make([]nostr.Event, 0, 128)
	for evt := range baseQuery(context.Background(), nostr.Filter{Kinds: []nostr.Kind{30402, 5}}) {
		if evt.Kind == 30402 || evt.Kind == 5 {
			events = append(events, evt)
		}
	}

	slices.SortFunc(events, compareSeedEvents)
	for _, evt := range events {
		s.applyEvent(evt)
	}
}

func compareSeedEvents(a, b nostr.Event) int {
	if a.CreatedAt < b.CreatedAt {
		return -1
	}
	if a.CreatedAt > b.CreatedAt {
		return 1
	}
	if a.Kind != b.Kind {
		if a.Kind == 30402 {
			return -1
		}
		if b.Kind == 30402 {
			return 1
		}
	}
	return strings.Compare(a.ID.Hex(), b.ID.Hex())
}

func (s *scope2State) applyEvent(evt nostr.Event) {
	switch evt.Kind {
	case 30402:
		s.upsertProduct(evt)
	case 5:
		s.applyDeleteEvent(evt)
	}
}

func (s *scope2State) upsertProduct(evt nostr.Event) {
	pe := buildProductEnvelope(evt)

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.products[pe.Address]
	if ok && !eventBeatsCurrent(pe.Event, current.Event) {
		return
	}

	pe.Tombstoned = false
	s.products[pe.Address] = pe
}

func (s *scope2State) applyDeleteEvent(evt nostr.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, tag := range evt.Tags {
		if len(tag) < 2 {
			continue
		}

		switch tag[0] {
		case "e":
			id, err := nostr.IDFromHex(tag[1])
			if err != nil {
				continue
			}
			for address, current := range s.products {
				if current.Event.ID == id {
					current.Tombstoned = true
					s.products[address] = current
				}
			}
		case "a":
			address, ok := canonicalAddress(tag[1])
			if !ok {
				continue
			}
			current, ok := s.products[address]
			if !ok {
				continue
			}
			if current.Event.CreatedAt <= evt.CreatedAt {
				current.Tombstoned = true
				s.products[address] = current
			}
		}
	}
}

func (s *scope2State) applyDeletedEvent(evt nostr.Event) {
	if evt.Kind != 30402 {
		return
	}

	address := productAddress(evt)

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.products[address]
	if !ok || current.Event.ID != evt.ID {
		return
	}

	current.Tombstoned = true
	s.products[address] = current
}

func (s *scope2State) evaluate(filter nostr.Filter, plan searchPlan, opts Scope2Options) ([]productEnvelope, string) {
	products := s.matchProducts(filter, plan.Text)

	policy := evaluatePricePolicy(products, plan, opts)
	if !policy.Allowed {
		return nil, policy.Reason
	}

	if policy.PartialCurrency != "" {
		products = keepCurrencyCohort(products, policy.PartialCurrency)
	}

	slices.SortFunc(products, func(a, b productEnvelope) int {
		return compareProducts(a, b, plan.Sort)
	})

	start := 0
	if plan.Cursor != "" {
		start = findCursorStart(products, plan.Cursor)
	}
	if start >= len(products) {
		return nil, ""
	}

	end := start + resolvedLimit(filter, opts)
	if end > len(products) {
		end = len(products)
	}
	return products[start:end], ""
}

func (s *scope2State) matchProducts(filter nostr.Filter, text string) []productEnvelope {
	base := filter.Clone()
	base.Search = ""

	needle := strings.ToLower(strings.TrimSpace(text))

	s.mu.RLock()
	defer s.mu.RUnlock()

	products := make([]productEnvelope, 0, len(s.products))
	for _, current := range s.products {
		if current.Tombstoned {
			continue
		}
		if !base.Matches(current.Event) {
			continue
		}
		if needle != "" && !strings.Contains(current.SearchText, needle) {
			continue
		}
		products = append(products, current)
	}
	return products
}

func buildProductEnvelope(evt nostr.Event) productEnvelope {
	title, summary := extractProductText(evt)
	price, currency, hasPrice := extractPrice(evt)

	return productEnvelope{
		Address:    productAddress(evt),
		Event:      evt,
		UpdatedAt:  extractUpdatedAt(evt),
		Price:      price,
		Currency:   currency,
		HasPrice:   hasPrice,
		SearchText: strings.ToLower(strings.TrimSpace(title + "\n" + summary)),
	}
}

func eventBeatsCurrent(next, current nostr.Event) bool {
	if next.CreatedAt > current.CreatedAt {
		return true
	}
	if next.CreatedAt < current.CreatedAt {
		return false
	}
	return strings.Compare(next.ID.Hex(), current.ID.Hex()) < 0
}

func productAddress(evt nostr.Event) string {
	if dTag := evt.Tags.GetD(); dTag != "" {
		return fmt.Sprintf("%d:%s:%s", evt.Kind, evt.PubKey, dTag)
	}
	return evt.ID.Hex()
}

func canonicalAddress(raw string) (string, bool) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return "", false
	}
	kind, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", false
	}
	pubkey, err := nostr.PubKeyFromHex(parts[1])
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d:%s:%s", kind, pubkey, parts[2]), true
}

func evaluatePricePolicy(products []productEnvelope, plan searchPlan, opts Scope2Options) pricePolicy {
	if plan.Sort != SortPriceAsc && plan.Sort != SortPriceDesc {
		return pricePolicy{Allowed: true}
	}
	if opts.AllowMixedCurrencySort {
		return pricePolicy{Allowed: true}
	}

	counts := make(map[string]int)
	for _, pe := range products {
		if pe.HasPrice {
			counts[pe.Currency]++
		}
	}
	if len(counts) <= 1 {
		return pricePolicy{Allowed: true}
	}
	if !plan.Partial {
		return pricePolicy{
			Reason: "blocked: mixed-currency price sort requires partial=1 or trusted normalization",
		}
	}

	bestCurrency := ""
	bestCount := -1
	for currency, count := range counts {
		if count > bestCount || (count == bestCount && strings.Compare(currency, bestCurrency) < 0) {
			bestCurrency = currency
			bestCount = count
		}
	}

	return pricePolicy{
		Allowed:         true,
		PartialCurrency: bestCurrency,
	}
}

func keepCurrencyCohort(products []productEnvelope, currency string) []productEnvelope {
	trimmed := make([]productEnvelope, 0, len(products))
	for _, pe := range products {
		if pe.HasPrice && pe.Currency == currency {
			trimmed = append(trimmed, pe)
		}
	}
	return trimmed
}

func compareProducts(a, b productEnvelope, mode ProductSort) int {
	switch mode {
	case SortPriceAsc, SortPriceDesc:
		if a.HasPrice != b.HasPrice {
			if a.HasPrice {
				return -1
			}
			return 1
		}
		if a.HasPrice && b.HasPrice && a.Price != b.Price {
			if a.Price < b.Price {
				if mode == SortPriceAsc {
					return -1
				}
				return 1
			}
			if mode == SortPriceAsc {
				return 1
			}
			return -1
		}
	case SortUpdatedAtDesc:
		if a.UpdatedAt > b.UpdatedAt {
			return -1
		}
		if a.UpdatedAt < b.UpdatedAt {
			return 1
		}
	case SortNewest:
		if a.Event.CreatedAt > b.Event.CreatedAt {
			return -1
		}
		if a.Event.CreatedAt < b.Event.CreatedAt {
			return 1
		}
	}

	if a.UpdatedAt > b.UpdatedAt {
		return -1
	}
	if a.UpdatedAt < b.UpdatedAt {
		return 1
	}
	if a.Event.CreatedAt > b.Event.CreatedAt {
		return -1
	}
	if a.Event.CreatedAt < b.Event.CreatedAt {
		return 1
	}
	return strings.Compare(a.Event.ID.Hex(), b.Event.ID.Hex())
}

func appendUnique(target []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(target, value) {
			target = append(target, value)
		}
	}
	return target
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

func extractProductText(evt nostr.Event) (string, string) {
	title := firstTagValue(evt.Tags, "title")
	summary := firstTagValue(evt.Tags, "summary")
	if title != "" || summary != "" {
		fallbackTitle, fallbackSummary := extractProductTextFromContent(evt.Content)
		if title == "" {
			title = fallbackTitle
		}
		if summary == "" {
			summary = fallbackSummary
		}
		return title, summary
	}

	return extractProductTextFromContent(evt.Content)
}

func extractProductTextFromContent(content string) (string, string) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", ""
	}

	title, _ := payload["title"].(string)
	summary, _ := payload["summary"].(string)
	return title, summary
}

func extractPrice(evt nostr.Event) (float64, string, bool) {
	if price, currency, ok := extractPriceFromTags(evt.Tags); ok {
		return price, currency, true
	}

	return extractPriceFromContent(evt.Content)
}

func extractPriceFromTags(tags nostr.Tags) (float64, string, bool) {
	for _, tag := range tags {
		if len(tag) < 2 || tag[0] != "price" {
			continue
		}

		price, ok := toFloat(tag[1])
		if !ok {
			continue
		}

		currency := ""
		if len(tag) >= 3 {
			currency = tag[2]
		} else {
			currency = firstTagValue(tags, "currency")
		}

		return price, strings.ToUpper(strings.TrimSpace(currency)), true
	}

	return 0, "", false
}

func extractPriceFromContent(content string) (float64, string, bool) {
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
	if ts, ok := extractTimestampFromTags(evt.Tags, "updated_at", "updatedAt", "published_at", "publishedAt"); ok {
		return ts
	}

	if ts, ok := extractUpdatedAtFromContent(evt.Content); ok {
		return ts
	}

	return evt.CreatedAt
}

func extractUpdatedAtFromContent(content string) (nostr.Timestamp, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return 0, false
	}

	for _, key := range []string{"updatedAt", "updated_at", "publishedAt", "published_at"} {
		if ts, ok := timestampFromValue(payload[key]); ok {
			return ts, true
		}
	}

	return 0, false
}

func extractTimestampFromTags(tags nostr.Tags, names ...string) (nostr.Timestamp, bool) {
	for _, name := range names {
		value := firstTagValue(tags, name)
		if ts, ok := timestampFromValue(value); ok {
			return ts, true
		}
	}

	return 0, false
}

func timestampFromValue(raw any) (nostr.Timestamp, bool) {
	if f, ok := toFloat(raw); ok {
		return nostr.Timestamp(int64(f)), true
	}
	if s, ok := raw.(string); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return nostr.Timestamp(n), true
		}
	}
	return 0, false
}

func firstTagValue(tags nostr.Tags, names ...string) string {
	for _, tag := range tags {
		if len(tag) < 2 {
			continue
		}
		for _, name := range names {
			if tag[0] == name {
				return strings.TrimSpace(tag[1])
			}
		}
	}

	return ""
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
