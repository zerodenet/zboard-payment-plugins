package runtime

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

const tdcloudNotificationRouteID = "tdcloud-notification"
const tdcloudNotificationPath = tdcloud.NotificationPath

func publicJSON(status uint32, retryAfter int64) *pluginv1.HTTPResponse {
	return &pluginv1.HTTPResponse{Status: status, ContentType: "application/json", Body: []byte(`{}`), RetryAfter: retryAfter}
}

// HandleHTTP owns the registered TDCloud route after ZBoard's route admission.
// Callback data only locates an attempt; its saved channel authenticates the
// notification before the coordinator queries TDCloud and records a fact.
func (s *Server) HandleHTTP(ctx context.Context, request *pluginv1.HTTPRequest) (*pluginv1.HTTPResponse, error) {
	if request == nil || request.RouteId != tdcloudNotificationRouteID || request.Method != http.MethodPost || request.Path != tdcloudNotificationPath {
		return publicJSON(http.StatusNotFound, 0), nil
	}
	if request.RawQuery != "" || request.ForceQuery {
		return publicJSON(http.StatusBadRequest, 0), nil
	}
	mediaType, _, err := mime.ParseMediaType(request.ContentType)
	if err != nil || mediaType != "application/json" {
		return publicJSON(http.StatusBadRequest, 0), nil
	}
	headers := make(http.Header)
	for name, field := range request.Headers {
		if field == nil {
			continue
		}
		for _, value := range field.Values {
			headers.Add(name, string(value))
		}
	}
	for _, name := range []string{"X-TDCloud-Event-ID", "X-TDCloud-Timestamp", "X-TDCloud-Signature"} {
		if len(headers.Values(name)) != 1 || strings.TrimSpace(headers.Get(name)) == "" {
			return publicJSON(http.StatusBadRequest, 0), nil
		}
	}
	event, err := tdcloud.ParseNotification(request.Body)
	if err != nil {
		return publicJSON(http.StatusBadRequest, 0), nil
	}
	s.mu.RLock()
	checkout := s.checkout
	s.mu.RUnlock()
	if checkout == nil {
		return publicJSON(http.StatusServiceUnavailable, 30), nil
	}
	locator, ok := checkout.ports.Attempts.(payment.NotificationAttemptLocator)
	if !ok {
		return publicJSON(http.StatusServiceUnavailable, 30), nil
	}
	attempt, err := locator.FindByProviderOrder(ctx, "tdcloud", event.Data.OrderNo)
	if errors.Is(err, payment.ErrNotFound) {
		return publicJSON(http.StatusBadRequest, 0), nil
	}
	if err != nil {
		return publicJSON(http.StatusServiceUnavailable, 30), nil
	}
	_, err = checkout.confirmAttemptNotification(ctx, attempt, request.Body, headers, time.Now())
	switch {
	case err == nil:
		return publicJSON(http.StatusOK, 0), nil
	case errors.Is(err, tdcloud.ErrInvalidNotification), errors.Is(err, payment.ErrConflict):
		return publicJSON(http.StatusBadRequest, 0), nil
	case errors.Is(err, payment.ErrNotFound):
		return publicJSON(http.StatusBadRequest, 0), nil
	default:
		return publicJSON(http.StatusServiceUnavailable, 30), nil
	}
}
