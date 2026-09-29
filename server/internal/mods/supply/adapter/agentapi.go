package adapter

// agentapi.go — 代理 API v1 协议。
// 基础地址示例：https://host/api/open/agent/v1
// 认证：X-Agent-Key（兼容服务端同时支持的 Bearer 方式时仍固定使用更明确的专用头）。
//
// 协议与传统发卡货源不同：/balance 同时承担余额与可售套餐目录，POST /keys
// 生成卡密并用 Idempotency-Key 防重，异步结果通过 /orders/{order_no} 轮询。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const agentPlanCategory = "agent_plans"

type agentAPIAdapter struct {
	protocol string
	creds    Credentials
	t        *transport
}

func newAgentAPI(baseURL string, creds Credentials, retryIntervals []int) (Adapter, error) {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil, ErrCredentialsInvalid
	}
	t, err := newTransport(strings.TrimRight(baseURL, "/"), retryIntervals, slog.Default())
	if err != nil {
		return nil, err
	}
	return &agentAPIAdapter{protocol: "agent_api", creds: creds, t: t}, nil
}

func (a *agentAPIAdapter) Protocol() string { return a.protocol }

func (a *agentAPIAdapter) request(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string) ([]byte, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("adapter.agentapi: 序列化请求失败: %w", err)
		}
	}
	headers := map[string]string{
		"X-Agent-Key": a.creds.APIKey,
		"Accept":      "application/json",
	}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	return a.t.do(ctx, method, path, query, headers, raw)
}

func (a *agentAPIAdapter) Ping(ctx context.Context) (*PingResult, error) {
	raw, err := a.request(ctx, http.MethodGet, "/balance", nil, nil, "")
	if err != nil {
		return nil, err
	}
	root, err := decodeAgentJSON(raw)
	if err != nil {
		return nil, err
	}
	balance := int64(-1)
	if v, key, ok := findAgentField(root, "balance_cents", "balance_amount_cents", "balance"); ok {
		if n, ok := agentMoney(v, strings.Contains(key, "cents")); ok {
			balance = n
		}
	}
	currency := agentStringField(root, "currency", "balance_currency")
	name := agentStringField(root, "site_name", "site", "agent_name", "agent", "name")
	if name == "" {
		name = "Agent API"
	}
	return &PingResult{
		SiteName:        name,
		ProtocolVersion: "agent/v1",
		Balance:         balance,
		Currency:        currency,
	}, nil
}

func (a *agentAPIAdapter) ListCategories(context.Context) ([]Category, error) {
	return []Category{{ID: agentPlanCategory, Name: "代理套餐"}}, nil
}

func (a *agentAPIAdapter) ListProducts(ctx context.Context, page, pageSize int, _ bool) (*ProductList, error) {
	raw, err := a.request(WithCatalogRead(ctx), http.MethodGet, "/balance", nil, nil, "")
	if err != nil {
		return nil, err
	}
	root, err := decodeAgentJSON(raw)
	if err != nil {
		return nil, err
	}
	plans, err := agentProducts(root)
	if err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	start := (page - 1) * pageSize
	if start > len(plans) {
		start = len(plans)
	}
	end := start + pageSize
	if end > len(plans) {
		end = len(plans)
	}
	return &ProductList{
		Total:            len(plans),
		Items:            append([]Product(nil), plans[start:end]...),
		HasMore:          end < len(plans),
		Categories:       []Category{{ID: agentPlanCategory, Name: "代理套餐"}},
		IncludesInactive: false, // /balance 只描述“可售套餐”，未提供下架全集。
	}, nil
}

func (a *agentAPIAdapter) GetStock(ctx context.Context, productCode, _ string) (int32, error) {
	raw, err := a.request(stockReadContext(ctx), http.MethodGet, "/balance", nil, nil, "")
	if err != nil {
		return 0, err
	}
	root, err := decodeAgentJSON(raw)
	if err != nil {
		return 0, err
	}
	plans, err := agentProducts(root)
	if err != nil {
		return 0, err
	}
	for _, p := range plans {
		if p.ID != productCode {
			continue
		}
		if !p.IsActive {
			return 0, ErrProductUnavailable
		}
		return p.Stock, nil
	}
	return 0, ErrProductUnavailable
}

func (a *agentAPIAdapter) CreateOrder(ctx context.Context, req CreateOrderReq) (*CreateOrderResult, error) {
	code := strings.TrimSpace(req.UpstreamSKU)
	if code == "" {
		code = strings.TrimSpace(req.ProductCode)
	}
	if code == "" {
		return nil, errors.New("adapter.agentapi: plan_code 不能为空")
	}
	if req.Quantity <= 0 {
		return nil, errors.New("adapter.agentapi: count 必须大于 0")
	}
	idem := strings.TrimSpace(req.DownstreamOrderNo)
	if len(idem) < 8 || len(idem) > 128 {
		return nil, errors.New("adapter.agentapi: Idempotency-Key 必须为 8-128 个字符")
	}
	note := req.TraceID
	if note == "" {
		note = req.DownstreamOrderNo
	}
	body := map[string]any{
		"count":     req.Quantity,
		"plan_code": code,
		"note":      note,
	}
	raw, err := a.request(ctx, http.MethodPost, "/keys", nil, body, idem)
	if err != nil {
		return nil, agentHTTPError(err)
	}
	parsed, err := parseAgentOrder(raw, "")
	if err != nil {
		return nil, err
	}
	if parsed.Status == "failed" {
		return nil, classifyAgentBusinessError(0, parsed.ErrorCode, parsed.ErrorMessage)
	}
	if parsed.Status == "pending" && parsed.UpstreamOrderID == "" {
		return nil, errors.New("adapter.agentapi: processing 响应缺少 order_no/order_id，无法轮询")
	}
	return &CreateOrderResult{
		UpstreamOrderID: parsed.UpstreamOrderID,
		Status:          parsed.Status,
		Amount:          parsed.Amount,
		Cards:           parsed.Cards,
	}, nil
}

func (a *agentAPIAdapter) GetOrder(ctx context.Context, upstreamOrderID string) (*OrderDetail, error) {
	if strings.TrimSpace(upstreamOrderID) == "" {
		return nil, errors.New("adapter.agentapi: order_no 不能为空")
	}
	raw, err := a.request(ctx, http.MethodGet, "/orders/"+url.PathEscape(upstreamOrderID), nil, nil, "")
	if err != nil {
		return nil, agentHTTPError(err)
	}
	parsed, err := parseAgentOrder(raw, upstreamOrderID)
	if err != nil {
		return nil, err
	}
	return &OrderDetail{
		UpstreamOrderID: parsed.UpstreamOrderID,
		Status:          parsed.Status,
		Amount:          parsed.Amount,
		Cards:           parsed.Cards,
	}, nil
}

func (a *agentAPIAdapter) RefundOrder(context.Context, string) error {
	return ErrNotSupported
}

type agentParsedOrder struct {
	UpstreamOrderID string
	Status          string
	Amount          int64
	Cards           []string
	ErrorCode       string
	ErrorMessage    string
}

func parseAgentOrder(raw []byte, fallbackID string) (*agentParsedOrder, error) {
	root, err := decodeAgentJSON(raw)
	if err != nil {
		return nil, err
	}
	out := &agentParsedOrder{UpstreamOrderID: fallbackID}
	out.UpstreamOrderID = firstAgentString(root, out.UpstreamOrderID, "order_no", "order_id", "id", "trade_no")
	out.ErrorCode = agentStringField(root, "error_code", "code")
	out.ErrorMessage = agentStringField(root, "error_message", "message", "msg", "reason")

	if v, key, ok := findAgentField(root, "amount_cents", "total_amount_cents", "amount", "total_amount"); ok {
		if n, ok := agentMoney(v, strings.Contains(key, "cents")); ok {
			out.Amount = n
		}
	}
	out.Cards = agentCards(root)
	status := strings.ToLower(strings.TrimSpace(agentStringField(root, "status", "state")))
	switch status {
	case "failed", "failure", "error", "rejected", "cancelled", "canceled":
		out.Status = "failed"
	case "processing", "pending", "queued", "accepted", "running", "submitted":
		out.Status = "pending"
	case "success", "succeeded", "completed", "complete", "done", "finished", "fulfilled", "delivered", "issued":
		if len(out.Cards) > 0 {
			out.Status = "delivered"
		} else {
			// 写接口可能先返回业务成功但卡密仍需从订单详情获取。
			out.Status = "pending"
		}
	default:
		if len(out.Cards) > 0 {
			out.Status = "delivered"
		} else {
			out.Status = "pending"
		}
	}

	if b, ok := agentBoolField(root, "ok", "success"); ok && !b {
		out.Status = "failed"
	}
	return out, nil
}

func agentHTTPError(err error) error {
	var he *httpError
	if !errors.As(err, &he) {
		return err
	}
	return classifyAgentBusinessError(he.Status, he.Code, he.Message)
}

func classifyAgentBusinessError(status int, code, message string) error {
	codeNorm := strings.ToLower(strings.TrimSpace(code))
	s := strings.ToLower(strings.TrimSpace(code + " " + message))
	switch {
	case codeNorm == "plan_not_found" || codeNorm == "invalid_plan" || codeNorm == "plan_unavailable":
		return ErrProductUnavailable
	case status == http.StatusConflict || strings.Contains(s, "idempot") || strings.Contains(s, "幂等"):
		return ErrDuplicateSubmit
	case strings.Contains(s, "balance") || strings.Contains(s, "余额"):
		return ErrInsufficientBalance
	case strings.Contains(s, "stock") || strings.Contains(s, "库存") || strings.Contains(s, "sold out"):
		return ErrNoStock
	case strings.Contains(s, "plan") && (strings.Contains(s, "not found") || strings.Contains(s, "unavailable") || strings.Contains(s, "invalid")):
		return ErrProductUnavailable
	case strings.Contains(s, "套餐") && (strings.Contains(s, "不存在") || strings.Contains(s, "不可") || strings.Contains(s, "无效")):
		return ErrProductUnavailable
	default:
		if status > 0 {
			return &httpError{Status: status, Code: code, Message: message}
		}
		if message != "" || code != "" {
			return fmt.Errorf("adapter.agentapi: 上游拒绝 (%s): %s", code, message)
		}
		return errors.New("adapter.agentapi: 上游返回失败状态")
	}
}

func decodeAgentJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("adapter.agentapi: 解析响应失败: %w", err)
	}
	return root, nil
}

func agentProducts(root any) ([]Product, error) {
	container, ok := findAgentPlanContainer(root, 0)
	if !ok {
		return nil, errors.New("adapter.agentapi: /balance 响应未找到可售套餐字段（支持 prices/plans/available_plans/available_packages/packages/products/items 等）")
	}
	out := make([]Product, 0)
	switch v := container.(type) {
	case []any:
		for _, row := range v {
			if p, ok := agentProduct("", row); ok {
				out = append(out, p)
			}
		}
	case map[string]any:
		for code, row := range v {
			if p, ok := agentProduct(code, row); ok {
				out = append(out, p)
			}
		}
	default:
		return nil, errors.New("adapter.agentapi: 可售套餐字段必须是数组或对象")
	}
	if len(out) == 0 {
		return nil, errors.New("adapter.agentapi: /balance 未返回可解析的可售套餐")
	}
	return out, nil
}

func findAgentPlanContainer(v any, depth int) (any, bool) {
	if depth > 4 {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, key := range []string{"prices", "plans", "available_plans", "available_packages", "sellable_plans", "plan_list", "packages", "products", "items"} {
		if x, exists := m[key]; exists {
			switch x.(type) {
			case []any, map[string]any:
				return x, true
			}
		}
	}
	for _, key := range []string{"data", "result", "payload", "account", "balance"} {
		if x, exists := m[key]; exists {
			if found, ok := findAgentPlanContainer(x, depth+1); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func agentProduct(codeHint string, row any) (Product, bool) {
	if s, ok := row.(string); ok {
		code := strings.TrimSpace(s)
		if code == "" {
			code = strings.TrimSpace(codeHint)
		}
		if code == "" {
			return Product{}, false
		}
		return Product{ID: code, Name: code, CategoryID: agentPlanCategory, FactoryPrice: -1, IsActive: true, Stock: -1}, true
	}
	m, ok := row.(map[string]any)
	if !ok {
		if codeHint == "" {
			return Product{}, false
		}
		return Product{ID: codeHint, Name: codeHint, CategoryID: agentPlanCategory, FactoryPrice: -1, IsActive: true, Stock: -1}, true
	}
	code := firstMapString(m, "plan_code", "code", "slug", "id")
	if code == "" {
		code = codeHint
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Product{}, false
	}
	name := firstMapString(m, "name", "title", "display_name", "label")
	if name == "" {
		name = code
	}
	price := int64(0)
	factory := int64(-1)
	for _, key := range []string{"price_cents", "unit_price_cents", "amount_cents", "price", "unit_price", "amount", "cost"} {
		if v, exists := m[key]; exists {
			if n, ok := agentMoney(v, strings.Contains(key, "cents")); ok {
				price, factory = n, n
				break
			}
		}
	}
	active := true
	if b, ok := mapBool(m, "is_active", "enabled", "available"); ok {
		active = b
	} else if status := strings.ToLower(firstMapString(m, "status", "state")); status != "" {
		switch status {
		case "inactive", "disabled", "unavailable", "sold_out", "offline":
			active = false
		}
	}
	stock := int32(-1)
	for _, key := range []string{"stock", "stock_quantity", "quantity", "available_count"} {
		if v, exists := m[key]; exists {
			if n, ok := agentInt64(v); ok {
				if n > int64(^uint32(0)>>1) {
					n = int64(^uint32(0) >> 1)
				}
				stock = int32(n)
				break
			}
		}
	}
	if !active && stock < 0 {
		stock = 0
	}
	return Product{
		ID:           code,
		Name:         name,
		CategoryID:   agentPlanCategory,
		Price:        price,
		FactoryPrice: factory,
		IsActive:     active,
		Stock:        stock,
		UpstreamExtra: map[string]any{
			"plan_code": code,
		},
	}, true
}

func agentCards(root any) []string {
	var out []string
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 5 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for _, key := range []string{"card_key", "key"} {
				if s, ok := x[key].(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, splitCards(s)...)
					return
				}
			}
			for _, key := range []string{"keys", "cards", "card_keys", "codes", "items", "data", "result", "payload", "order"} {
				if child, ok := x[key]; ok {
					walk(child, depth+1)
				}
			}
		case []any:
			for _, item := range x {
				walk(item, depth+1)
			}
		case string:
			if strings.TrimSpace(x) != "" {
				out = append(out, splitCards(x)...)
			}
		}
	}
	walk(root, 0)
	return uniqueStrings(out)
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func findAgentField(root any, keys ...string) (any, string, bool) {
	var walk func(any, int) (any, string, bool)
	walk = func(v any, depth int) (any, string, bool) {
		if depth > 4 {
			return nil, "", false
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "", false
		}
		for _, key := range keys {
			if x, exists := m[key]; exists {
				return x, key, true
			}
		}
		for _, key := range []string{"data", "result", "payload", "order", "account"} {
			if x, exists := m[key]; exists {
				if value, foundKey, ok := walk(x, depth+1); ok {
					return value, foundKey, true
				}
			}
		}
		return nil, "", false
	}
	return walk(root, 0)
}

func agentStringField(root any, keys ...string) string {
	if v, _, ok := findAgentField(root, keys...); ok {
		return scalarString(v)
	}
	return ""
}

func firstAgentString(root any, fallback string, keys ...string) string {
	if s := agentStringField(root, keys...); s != "" {
		return s
	}
	return fallback
}

func agentBoolField(root any, keys ...string) (bool, bool) {
	if v, _, ok := findAgentField(root, keys...); ok {
		return scalarBool(v)
	}
	return false, false
}

func firstMapString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s := scalarString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func mapBool(m map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return scalarBool(v)
		}
	}
	return false, false
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}

func scalarBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "true", "yes", "active", "enabled", "available":
			return true, true
		case "0", "false", "no", "inactive", "disabled", "unavailable":
			return false, true
		}
	case json.Number:
		n, err := x.Int64()
		if err == nil {
			return n != 0, true
		}
	}
	return false, false
}

func agentInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

func agentMoney(v any, alreadyCents bool) (int64, bool) {
	s := scalarString(v)
	if s == "" {
		return 0, false
	}
	if alreadyCents {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return n, err == nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	if f < 0 {
		return int64(f*100 - 0.5), true
	}
	return int64(f*100 + 0.5), true
}
