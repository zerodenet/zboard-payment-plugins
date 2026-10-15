package runtime

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestNativeSDKPrepareRestartAndConcurrentNotification(t *testing.T) {
	const principal = "order-bound-authority-42"
	const secret = "independent-notification-secret-for-native-test-42"
	var mu sync.Mutex
	events := map[string]string{}
	var settled bool
	storageHost(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call pluginv1.HostCallRequest
		if json.NewDecoder(r.Body).Decode(&call) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		respond := func(data any, code string) {
			if code != "" {
				_ = json.NewEncoder(w).Encode(pluginv1.HostCallResponse{Error: &pluginv1.HostCallError{Code: code}})
			} else {
				raw, _ := json.Marshal(data)
				_ = json.NewEncoder(w).Encode(pluginv1.HostCallResponse{Data: raw})
			}
		}
		switch call.Operation {
		case "commerce.orders.list":
			var in pluginv1.CommerceOrdersRequest
			_ = json.Unmarshal(call.Payload, &in)
			if call.Capability != pluginv1.CommerceOrdersReadCapability || in.ID != 42 || (in.PrincipalID != principal && in.PrincipalID != "account-authority") {
				respond(nil, "forbidden")
				return
			}
			status := "pending"
			if settled {
				status = "paid"
			}
			respond(pluginv1.CommerceOrderPage{Items: []pluginv1.CommerceOrder{{ID: 42, AccountID: 1, TradeNo: "zb-42", PayableAmount: 1050, Currency: "USD", Status: status}}}, "")
		case "commerce.payments.record":
			var in pluginv1.PaymentFactRequest
			_ = json.Unmarshal(call.Payload, &in)
			if call.Capability != pluginv1.CommercePaymentsWriteCapability || in.PrincipalID != principal || in.OrderID != 42 {
				respond(nil, "forbidden")
				return
			}
			if in.AmountMinor != 1050 || in.Currency != "USD" || in.Status != "paid" {
				respond(nil, "conflict")
				return
			}
			raw := string(call.Payload)
			if previous := events[in.ProviderEventID]; previous != "" && previous != raw {
				respond(nil, "conflict")
				return
			}
			first := events[in.ProviderEventID] == ""
			events[in.ProviderEventID] = raw
			settled = true
			respond(pluginv1.PaymentFactResult{OrderID: 42, Status: "paid", Fulfilled: first}, "")
		default:
			respond(nil, "forbidden")
		}
	}))
	var paid atomic.Bool
	var remoteState atomic.Value
	remoteState.Store("pending")
	var lookups atomic.Int32
	var creates atomic.Int32
	occurred := time.Now().UTC().Truncate(time.Second)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant-token", "expires_in": 3600})
		case "/openapi/v1/merchant-payment-channels":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"payment_no": "test-usd", "currency": "USD", "currency_decimals": 2}}})
		case "/openapi/v1/merchant-payments":
			var in struct {
				ReturnURL string  `json:"return_url"`
				Amount    string  `json:"amount"`
				Minor     *int64  `json:"amount_minor"`
				Method    *string `json:"payment_no"`
				Reference string  `json:"external_reference"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Amount != "10.50" || in.Minor != nil || in.Method != nil || in.Reference != "zb-42" || in.ReturnURL != "https://board.example/account/orders?payment_order=42" {
				t.Errorf("untrusted creation values: %+v", in)
			}
			creates.Add(1)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "test-usd", "return_url": in.ReturnURL, "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "pending", "checkout_url": "https://pay.example/42", "expires_at": time.Now().Add(time.Hour)})
		case "/openapi/v1/merchant-payments/mp-42":
			lookups.Add(1)
			status := remoteState.Load().(string)
			if paid.Load() {
				status = "paid"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "test-usd", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": status, "event_id": "paid-event-42", "provider_trade_no": "trade-42", "occurred_at": occurred})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	open := func() *Server {
		s, err := Open()
		if err != nil {
			t.Fatal(err)
		}
		s.httpClient = provider.Client()
		t.Cleanup(s.Close)
		return s
	}
	s := open()
	enabled := true
	settings, _ := json.Marshal(tdcloud.Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "merchant-secret", PaymentNo: "test-usd", NotifyURL: "https://hooks.example" + tdcloud.NotificationPath, NotifySecret: secret, NotifyEnabled: &enabled})
	cfg, _ := json.Marshal(config{Channels: []channelConfig{{ID: "tdcloud-main", Name: "TDCloud", Type: "tdcloud", Settings: settings, Enabled: true}}})
	if _, err := s.ApplyConfig(t.Context(), &pluginv1.ConfigRequest{ConfigJson: cfg}); err != nil {
		t.Fatal(err)
	}
	hostContext := pluginv1.CheckoutContext{OrderID: 42, AccountID: 1, TradeNo: "zb-42", PayableAmount: 1050, Currency: "USD", CurrencyPrecision: 2, Status: "pending", ReturnURL: "https://board.example/account/orders?payment_order=42"}
	contextJSON, _ := json.Marshal(hostContext)
	request := func(action string) *pluginv1.PageActionRequest {
		return &pluginv1.PageActionRequest{ActorId: principal, Surface: "account", Purpose: "slot", Slot: pluginv1.CheckoutPaymentSlot, SlotId: "payment-methods", Action: action, CheckoutJson: contextJSON, PayloadJson: []byte(`{"option_id":"tdcloud-main"}`)}
	}
	prepared, err := s.HandlePageAction(t.Context(), request("payments.prepare"))
	if err != nil || !strings.Contains(string(prepared.ResultJson), `"checkout_url":"https://pay.example/42"`) || strings.Contains(string(prepared.ResultJson), principal) {
		t.Fatalf("host prepare: %+v %v", prepared, err)
	}
	bad := request("payments.prepare")
	bad.PayloadJson = []byte(`{"option_id":"tdcloud-main","amount_minor":1}`)
	if _, err := s.HandlePageAction(t.Context(), bad); err == nil {
		t.Fatal("browser amount accepted")
	}
	bad = request("payments.prepare")
	bad.ActorId = "forged"
	if _, err := s.HandlePageAction(t.Context(), bad); err == nil {
		t.Fatal("forged authority accepted")
	}
	if creates.Load() != 1 {
		t.Fatal("denied request created payment")
	}
	inspect := func(actor, payload string) actionResult {
		in := request("payments.inspect")
		in.ActorId, in.PayloadJson = actor, []byte(payload)
		response, err := s.HandlePageAction(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		var out actionResult
		if json.Unmarshal(response.ResultJson, &out) != nil {
			t.Fatal("invalid inspection")
		}
		return out
	}
	beforeLookup := lookups.Load()
	for _, item := range []struct{ actor, payload string }{
		{"forged", `{"order_id":"42"}`}, {"account-authority", `{"order_id":"99"}`},
		{"account-authority", `{"order_id":"42","account_id":"forged"}`},
		{"account-authority", `{"order_id":"042"}`},
	} {
		if result := inspect(item.actor, item.payload); result.Error == nil {
			t.Fatalf("unauthorized inspection accepted: %+v", item)
		}
	}
	if lookups.Load() != beforeLookup {
		t.Fatal("unauthorized inspection contacted provider")
	}
	remoteState.Store("review")
	inspected := inspect("account-authority", `{"order_id":"42"}`)
	if inspected.Error != nil || inspected.Data.(map[string]any)["provider_state"] != "review" ||
		inspected.Data.(map[string]any)["state"] != "unknown" || creates.Load() != 1 || settled || len(events) != 0 {
		t.Fatalf("inspection lost remote status or wrote payment: %+v", inspected)
	}
	remoteState.Store("pending")
	pending, err := s.HandlePageAction(t.Context(), request("payments.reconcile"))
	if err != nil || !strings.Contains(string(pending.ResultJson), `"state":"pending"`) {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	s.Close()
	s = open() // configuration removed; saved provider snapshot must survive
	if _, err := s.HandlePageAction(t.Context(), request("payments.prepare")); err != nil || creates.Load() != 1 {
		t.Fatalf("restart checkout: %v", err)
	}
	historical := action(t, s, "account-authority", "payments.methods", `{"currency":"USD","order_id":"42"}`)
	if historical.Error != nil || !historical.Data.([]any)[0].(map[string]any)["disabled"].(bool) {
		t.Fatalf("historical option: %+v", historical)
	}
	expired, err := s.checkout.ports.Attempts.Get(t.Context(), principal, 42)
	if err != nil {
		t.Fatal(err)
	}
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := s.checkout.ports.Attempts.Put(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	deadlineMethods := action(t, s, "account-authority", "payments.methods", `{"currency":"USD","order_id":"42"}`)
	if deadlineMethods.Error != nil || deadlineMethods.Data.([]any)[0].(map[string]any)["checkout_expires_at"] == nil {
		t.Fatalf("slot did not receive the owned checkout deadline: %+v", deadlineMethods)
	}
	for _, action := range []string{"payments.prepare", "payments.reconcile"} {
		response, err := s.HandlePageAction(t.Context(), request(action))
		public, ok := pluginv1.ParseActionError(err)
		if !ok || public.Code != "checkout_expiry_pending" || !public.Retryable || response != nil || creates.Load() != 1 {
			t.Fatalf("expired unresolved SDK checkout must not reopen or recreate: %+v %v", response, err)
		}
	}
	paid.Store(true)
	body, _ := json.Marshal(map[string]any{"id": "notification-42", "type": "merchant_payment.status_changed", "api_version": 1, "created_at": occurred, "data": map[string]any{"order_no": "mp-42", "external_reference": "zb-42", "payment_no": "test-usd", "amount": "10.50", "amount_minor": 1050, "currency": "USD", "currency_decimals": 2, "status": "paid", "status_version": 2, "event_id": "paid-event-42"}})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	notify := httptest.NewRequest("POST", "https://hooks.example"+tdcloud.NotificationPath, bytes.NewReader(body))
	notify.Header.Set("Content-Type", "application/json")
	notify.Header.Set("X-TDCloud-Event-ID", "notification-42")
	notify.Header.Set("X-TDCloud-Timestamp", timestamp)
	notify.Header.Set("X-TDCloud-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	forwarded := pluginv1.NewHTTPRequest(notify, body)
	forwarded.RouteId = tdcloudNotificationRouteID
	failures := make(chan error, 3)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.HandlePageAction(t.Context(), request("payments.reconcile"))
			failures <- err
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		response, err := s.HandleHTTP(t.Context(), forwarded)
		if err == nil && response.Status != 200 {
			err = payment.ErrPending
		}
		failures <- err
	}()
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !settled || len(events) != 1 {
		t.Fatalf("settlement replayed: %v %v", settled, events)
	}
}
