package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentAPIAdapterFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Key") != "ak_test" {
			t.Errorf("missing X-Agent-Key: %q", r.Header.Get("X-Agent-Key"))
		}
		switch r.URL.Path {
		case "/balance":
			_, _ = w.Write([]byte(`{"data":{"balance":"12.34","currency":"CNY","plans":[{"plan_code":"pro","name":"Pro 20X","price":"8.50","available":true},{"plan_code":"pro5x","name":"Pro 5X","price_cents":300,"stock":2}]}}`))
		case "/keys":
			if r.Method != http.MethodPost {
				t.Fatalf("keys method=%s", r.Method)
			}
			if r.Header.Get("Idempotency-Key") != "dedupe-12345678" {
				t.Fatalf("idempotency=%q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["plan_code"] != "pro" || int(body["count"].(float64)) != 2 {
				t.Fatalf("unexpected body: %#v", body)
			}
			_, _ = w.Write([]byte(`{"status":"processing","order_no":"AG-100"}`))
		case "/orders/AG-100":
			_, _ = w.Write([]byte(`{"data":{"status":"success","order_no":"AG-100","amount":"17.00","keys":[{"card_key":"CARD-1"},{"card_key":"CARD-2"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &agentAPIAdapter{
		protocol: "agent_api",
		creds:    Credentials{APIKey: "ak_test"},
		t:        newTransportWithClient(srv.URL, nil, nil, srv.Client()),
	}
	ctx := context.Background()

	ping, err := a.Ping(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ping.Balance != 1234 || ping.Currency != "CNY" {
		t.Fatalf("ping=%+v", ping)
	}

	list, err := a.ListProducts(ctx, 1, 50, false)
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Items) != 2 {
		t.Fatalf("list=%+v", list)
	}
	if list.Items[0].ID != "pro" || list.Items[0].Price != 850 || list.Items[0].Stock != -1 {
		t.Fatalf("first product=%+v", list.Items[0])
	}
	if list.Items[1].ID != "pro5x" || list.Items[1].Price != 300 || list.Items[1].Stock != 2 {
		t.Fatalf("second product=%+v", list.Items[1])
	}

	stock, err := a.GetStock(ctx, "pro5x", "")
	if err != nil || stock != 2 {
		t.Fatalf("stock=%d err=%v", stock, err)
	}

	created, err := a.CreateOrder(ctx, CreateOrderReq{
		ProductCode:       "pro",
		Quantity:          2,
		DownstreamOrderNo: "dedupe-12345678",
		TraceID:           "trace-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.UpstreamOrderID != "AG-100" || created.Status != "pending" {
		t.Fatalf("created=%+v", created)
	}

	order, err := a.GetOrder(ctx, "AG-100")
	if err != nil {
		t.Fatal(err)
	}
	if order.Status != "delivered" || order.Amount != 1700 || len(order.Cards) != 2 || order.Cards[0] != "CARD-1" {
		t.Fatalf("order=%+v", order)
	}
}

func TestAgentAPIIdempotencyConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error_code":"idempotency_conflict","error_message":"幂等键冲突"}`))
	}))
	defer srv.Close()
	a := &agentAPIAdapter{
		protocol: "agent_api",
		creds:    Credentials{APIKey: "ak_test"},
		t:        newTransportWithClient(srv.URL, nil, nil, srv.Client()),
	}
	_, err := a.CreateOrder(context.Background(), CreateOrderReq{
		ProductCode:       "pro",
		Quantity:          1,
		DownstreamOrderNo: "dedupe-12345678",
	})
	if !errors.Is(err, ErrDuplicateSubmit) {
		t.Fatalf("want ErrDuplicateSubmit, got %v", err)
	}
}

func TestAgentAPIWrappedPlanMap(t *testing.T) {
	root, err := decodeAgentJSON([]byte(`{"result":{"available_plans":{"prolite":{"title":"Pro 5X","unit_price":"1.25","enabled":true},"pro":{"title":"Pro 20X","unit_price_cents":999,"enabled":false}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := agentProducts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans=%+v", plans)
	}
	byCode := map[string]Product{}
	for _, p := range plans {
		byCode[p.ID] = p
	}
	if byCode["prolite"].Price != 125 || !byCode["prolite"].IsActive {
		t.Fatalf("prolite=%+v", byCode["prolite"])
	}
	if byCode["pro"].Price != 999 || byCode["pro"].IsActive {
		t.Fatalf("pro=%+v", byCode["pro"])
	}
}


func TestAgentAPIRealBalancePricesShape(t *testing.T) {
	root, err := decodeAgentJSON([]byte(`{
		"data": {
			"currency": "CNY",
			"prices": [
				{"plan_code":"go","label":"Go","price":35.0},
				{"plan_code":"plus","label":"Plus","price":109.0},
				{"plan_code":"prolite","label":"Pro Lite","price":640.0},
				{"plan_code":"credit250","label":"额度 250","price":72.0},
				{"plan_code":"credit500","label":"额度 500","price":145.0},
				{"plan_code":"credit1000","label":"额度 1000","price":268.0},
				{"plan_code":"pro","label":"Pro","price":1000.0},
				{"plan_code":"pro5x","label":"智利 Pro 5X","price":628.0}
			],
			"agent": "test-agent",
			"balance": 0.0
		},
		"code":"ok",
		"ok":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	plans, err := agentProducts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 8 {
		t.Fatalf("want 8 plans, got %d: %+v", len(plans), plans)
	}
	if plans[0].ID != "go" || plans[0].Name != "Go" || plans[0].Price != 3500 || plans[0].FactoryPrice != 3500 {
		t.Fatalf("first plan=%+v", plans[0])
	}
	if plans[7].ID != "pro5x" || plans[7].Name != "智利 Pro 5X" || plans[7].Price != 62800 {
		t.Fatalf("last plan=%+v", plans[7])
	}
}


func TestAgentAPIPlanNotFoundResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"code":"plan_not_found","msg":"所选会员档不存在，请刷新后重新选择"}`))
	}))
	defer srv.Close()

	a := &agentAPIAdapter{
		protocol: "agent_api",
		creds:    Credentials{APIKey: "ak_test"},
		t:        newTransportWithClient(srv.URL, nil, nil, srv.Client()),
	}
	_, err := a.CreateOrder(context.Background(), CreateOrderReq{
		ProductCode:       "__not_exist_test__",
		Quantity:          1,
		DownstreamOrderNo: "dedupe-12345678",
	})
	if !errors.Is(err, ErrProductUnavailable) {
		t.Fatalf("want ErrProductUnavailable, got %v", err)
	}
}
