package runtime

import (
	"context"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func (s *Server) reviewAction(ctx context.Context, r *pluginv1.PageActionRequest) (*pluginv1.PageActionResponse, error) {
	if r.Surface != "admin" || !r.ActorAdmin || r.PageId != "payment-reviews" || r.Purpose != "business" || r.ActorId == "" {
		return actionResponse(nil, "permission_denied")
	}
	var in struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if !decodeAction(r.PayloadJson, &in) || in.Offset < 0 || in.Offset > 1000000 || in.Limit < 1 || in.Limit > 20 {
		return actionResponse(nil, "invalid_argument")
	}
	if s.attempts == nil {
		return actionResponse(nil, "temporarily_unavailable")
	}
	items, err := s.attempts.paymentReviews(ctx)
	if err != nil {
		return actionResponse(nil, "temporarily_unavailable")
	}
	start := min(in.Offset, len(items))
	end := min(start+in.Limit, len(items))
	return actionResponse(struct {
		Items  []paymentReview `json:"items"`
		Total  int             `json:"total"`
		Offset int             `json:"offset"`
	}{items[start:end], len(items), start}, "")
}
