package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestHistoricalSnapshotsReceiptsAndAdminReviewSurviveStorageSDKRestart(t *testing.T) {
	storageHost(t)
	store, err := openStorage()
	if err != nil {
		t.Fatal(err)
	}
	oldSnapshot := []byte(`{"type":"tdcloud","settings":{"client_secret":"old-private-secret"}}`)
	old, err := store.Put(t.Context(), payment.Attempt{AccountID: "private-order-authority", OrderID: 42, ProviderOrderID: "mp-old", ChannelID: "tdcloud", ChannelSnapshot: oldSnapshot, State: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	current := old
	old.Revision = 0
	current.History = []payment.Attempt{old}
	current.PreviousOrders = []string{"mp-old"}
	current.ProviderOrderID = "mp-new"
	current.ChannelSnapshot = []byte(`{"type":"tdcloud","settings":{"client_secret":"new-private-secret"}}`)
	current.State, current.FactState, current.ReviewReason = "paid", "review", "additional_payment"
	current.VerifiedFact = &payment.PaymentFact{OrderID: 42, ProviderEventID: "tdcloud:event-new", ProviderTradeNo: "trade-new", Status: "paid", AmountMinor: 1050, Currency: "USD", OccurredAt: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC)}
	if _, err := store.Put(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	store.Close()
	restarted, err := openStorage()
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	historical, err := restarted.FindByProviderOrder(t.Context(), "tdcloud", "mp-old")
	if err != nil || string(historical.ChannelSnapshot) != string(oldSnapshot) || historical.ProviderOrderID != "mp-old" {
		t.Fatalf("historical index lost after restart: %+v %v", historical, err)
	}
	server := &Server{attempts: restarted}
	request := &pluginv1.PageActionRequest{Surface: "admin", ActorAdmin: true, Purpose: "business", PageId: "payment-reviews", ActorId: "admin", Action: "payments.reviews", PayloadJson: []byte(`{"offset":0,"limit":20}`)}
	out, err := server.HandlePageAction(t.Context(), request)
	if err != nil || !strings.Contains(string(out.ResultJson), "event-new") || strings.Contains(string(out.ResultJson), "private-secret") || strings.Contains(string(out.ResultJson), "private-order-authority") {
		t.Fatalf("admin receipt projection lost or leaked authority: %s %v", out.ResultJson, err)
	}
	for _, surface := range []string{"account", "public"} {
		request.Surface = surface
		out, _ = server.HandlePageAction(t.Context(), request)
		if !strings.Contains(string(out.ResultJson), "permission_denied") {
			t.Fatal("non-admin financial worklist access allowed")
		}
	}
	items, next, err := restarted.scanAttempts(t.Context(), recoveryCursor{}, 1)
	if err != nil || len(items) != 1 {
		t.Fatal("paged recovery scan missing order")
	}
	items, _, err = restarted.scanAttempts(t.Context(), next, 1)
	if err != nil || len(items) > 1 {
		t.Fatal("recovery scan exceeded page size")
	}
	// A pre-fix record containing only old IDs must remain an explicit case.
	loaded, _ := restarted.Get(t.Context(), current.AccountID, 42)
	loaded.History = nil
	if _, err := restarted.Put(t.Context(), loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.FindByProviderOrder(t.Context(), "tdcloud", "mp-old"); !errors.Is(err, payment.ErrHistoricalUnavailable) {
		t.Fatal("guessed missing historical credentials")
	}
	reviews, err := restarted.paymentReviews(t.Context())
	if err != nil || len(reviews) != 2 || reviews[1].Reason != "historical_credentials_missing" {
		t.Fatalf("legacy loss hidden: %+v %v", reviews, err)
	}
}

func TestRegisteredRecoveryTaskFindsLatePaymentWithoutNotification(t *testing.T) {
	storageHost(t)
	store, err := openStorage()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var lookups atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 3600})
			return
		}
		lookups.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"order_no": "mp-old", "external_reference": "zb-42", "amount": "10.50", "currency": "USD", "status": "paid", "status_version": 3, "event_id": "event-old", "provider_trade_no": "trade-old", "occurred_at": "2026-10-14T01:00:00Z"})
	}))
	defer provider.Close()
	settings, _ := json.Marshal(tdcloud.Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "private", AmountMetadata: &tdcloud.AmountMetadata{Version: 2, Currency: "USD"}})
	snapshot, _ := snapshotChannel(channelConfig{Type: "tdcloud", Settings: settings})
	if _, err := store.Put(t.Context(), payment.Attempt{AccountID: "buyer", OrderID: 42, OrderTradeNo: "zb-42", AmountMinor: 1050, Currency: "USD", ProviderOrderID: "mp-old", ChannelID: "tdcloud", ChannelSnapshot: snapshot, State: "failed", ProviderVersion: 2}); err != nil {
		t.Fatal(err)
	}
	facts := &checkoutFacts{events: map[string]bool{}}
	server := &Server{attempts: store, httpClient: provider.Client()}
	if err := server.BindCommerce(CommercePorts{Orders: checkoutOrder{}, Facts: facts, OrderReturnURL: checkoutReturnURL}); err != nil {
		t.Fatal(err)
	}
	if out, err := server.RunTask(t.Context(), &pluginv1.TaskRunRequest{TaskId: recoveryTaskID}); err != nil || !out.Succeeded {
		t.Fatalf("scheduled recovery failed: %+v %v", out, err)
	}
	a, _ := store.Get(t.Context(), "buyer", 42)
	if a.FactState != "recorded" || a.ProviderVersion != 3 || len(facts.events) != 1 {
		t.Fatal("task dropped late receipt")
	}
	if out, err := server.RunTask(t.Context(), &pluginv1.TaskRunRequest{TaskId: recoveryTaskID}); err != nil || !out.Succeeded || lookups.Load() != 1 || len(facts.events) != 1 {
		t.Fatal("periodic task replayed settled receipt")
	}
	if _, err := server.RunTask(t.Context(), &pluginv1.TaskRunRequest{TaskId: "other"}); err == nil {
		t.Fatal("unknown task executed")
	}
}
