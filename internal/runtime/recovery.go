package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const recoveryTaskID = "reconcile-payments"

type recoveryCursor struct {
	Bucket int    `json:"bucket"`
	After  string `json:"after"`
}

// The cursor is durable, and each run visits a bounded page of private orders.
// An empty or failing account cannot starve later orders after a restart.
func (s *storage) scanAttempts(ctx context.Context, cursor recoveryCursor, limit int) ([]payment.Attempt, recoveryCursor, error) {
	if cursor.Bucket < 0 || cursor.Bucket > 15 {
		cursor = recoveryCursor{}
	}
	var out []payment.Attempt
	for visited := 0; visited < 16; visited++ {
		value, err := s.getValue(ctx, "attempts."+strconv.FormatInt(int64(cursor.Bucket), 16))
		if err != nil {
			return out, cursor, err
		}
		entries := map[string]payment.Attempt{}
		if value.Found && json.Unmarshal(value.Value, &entries) != nil {
			return out, cursor, payment.ErrConflict
		}
		keys := make([]string, 0, len(entries))
		for key := range entries {
			if key > cursor.After {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			out = append(out, entries[key])
			cursor.After = key
			if len(out) == limit {
				return out, cursor, nil
			}
		}
		cursor.Bucket, cursor.After = (cursor.Bucket+1)%16, ""
	}
	return out, cursor, nil
}

// RunTask uses the existing durable host scheduler; no private polling goroutine
// runs outside plugin lifecycle admission. The internal deadline leaves time to
// return a definite result before the host's 45-second task deadline.
func (s *Server) RunTask(ctx context.Context, r *pluginv1.TaskRunRequest) (*pluginv1.TaskRunResult, error) {
	if r == nil || r.TaskId != recoveryTaskID {
		return nil, status.Error(codes.InvalidArgument, "unknown payment task")
	}
	s.mu.RLock()
	coordinator := s.checkout
	s.mu.RUnlock()
	if s.attempts == nil || coordinator == nil {
		return &pluginv1.TaskRunResult{Succeeded: false}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	value, err := s.attempts.getValue(ctx, "reconciliation.cursor")
	if err != nil {
		return &pluginv1.TaskRunResult{Succeeded: false}, nil
	}
	var cursor recoveryCursor
	if value.Found && json.Unmarshal(value.Value, &cursor) != nil {
		return &pluginv1.TaskRunResult{Succeeded: false}, nil
	}
	orders, next, err := s.attempts.scanAttempts(ctx, cursor, 20)
	if err != nil {
		return &pluginv1.TaskRunResult{Succeeded: false}, nil
	}
	service := payment.Service{Orders: coordinator.ports.Orders, Facts: coordinator.ports.Facts, Attempts: coordinator.ports.Attempts, ResolveChannel: coordinator.resolveChannel}
	ok := true
	progress := cursor
	processed := 0
	for _, order := range orders {
		// Leave enough budget for a durable cursor checkpoint. A slow provider
		// cannot trap every future run on the same first order.
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline && time.Until(deadline) < 6*time.Second {
			ok = false
			break
		}
		orderCtx, orderCancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := service.ReconcileDue(orderCtx, order.AccountID, order.OrderID, time.Now())
		orderCancel()
		if err != nil && !errors.Is(err, payment.ErrCheckoutExpiryPending) && !errors.Is(err, payment.ErrPending) {
			ok = false
		}
		if ctx.Err() != nil {
			return &pluginv1.TaskRunResult{Succeeded: false}, nil
		}
		bucket, _ := strconv.ParseInt(attemptBucket(order.AccountID)[9:], 16, 32)
		progress = recoveryCursor{Bucket: int(bucket), After: attemptKey(order.AccountID, order.OrderID)}
		processed++
	}
	if processed == len(orders) {
		progress = next
	}
	raw, _ := json.Marshal(progress)
	// Storage revisions can cover the entire plugin namespace. Reconciliation
	// changed that revision, so reload the cursor before checkpointing progress.
	fresh, err := s.attempts.getValue(ctx, "reconciliation.cursor")
	if err != nil {
		return &pluginv1.TaskRunResult{Succeeded: false}, nil
	}
	if _, err := s.attempts.putValue(ctx, "reconciliation.cursor", fresh.Revision, raw); err != nil {
		ok = false
	}
	return &pluginv1.TaskRunResult{Succeeded: ok}, nil
}

type paymentReview struct {
	OrderID         uint64               `json:"order_id"`
	ProviderOrderID string               `json:"provider_order_id"`
	Reason          string               `json:"reason"`
	Fact            *payment.PaymentFact `json:"fact,omitempty"`
}

// Review projection exposes verified financial facts to administrators, never
// account authorities, snapshot credentials, notification secrets or URLs.
func (s *storage) paymentReviews(ctx context.Context) ([]paymentReview, error) {
	reviews := []paymentReview{}
	for bucket := 0; bucket < 16; bucket++ {
		value, err := s.getValue(ctx, "attempts."+strconv.FormatInt(int64(bucket), 16))
		if err != nil {
			return nil, err
		}
		entries := map[string]payment.Attempt{}
		if value.Found && json.Unmarshal(value.Value, &entries) != nil {
			return nil, payment.ErrConflict
		}
		for _, root := range entries {
			for _, a := range append(append([]payment.Attempt(nil), root.History...), root) {
				if a.ReviewReason != "" || a.FactState == "pending" || (a.ProviderOrderID != "" && len(a.ChannelSnapshot) == 0) {
					reason := a.ReviewReason
					if reason == "" && len(a.ChannelSnapshot) == 0 {
						reason = "historical_credentials_missing"
					}
					if reason == "" {
						reason = "settlement_pending"
					}
					reviews = append(reviews, paymentReview{root.OrderID, a.ProviderOrderID, reason, a.VerifiedFact})
				}
			}
			for _, previous := range root.PreviousOrders {
				found := false
				for _, a := range root.History {
					if a.ProviderOrderID == previous {
						found = true
						break
					}
				}
				if previous != "" && !found {
					reviews = append(reviews, paymentReview{OrderID: root.OrderID, ProviderOrderID: previous, Reason: "historical_credentials_missing"})
				}
			}
		}
	}
	sort.Slice(reviews, func(i, j int) bool {
		if reviews[i].OrderID != reviews[j].OrderID {
			return reviews[i].OrderID < reviews[j].OrderID
		}
		return reviews[i].ProviderOrderID < reviews[j].ProviderOrderID
	})
	return reviews, nil
}
