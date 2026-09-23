package service

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/wishwall/wishwall/internal/constants"
	"github.com/wishwall/wishwall/internal/dto"
	"github.com/wishwall/wishwall/internal/model"
	"github.com/wishwall/wishwall/internal/util"
)

func TestWishClaimService_Claim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		wishFn      func(tx *gorm.DB, id uint64) (*model.Wish, error)
		createFn    func(tx *gorm.DB, claim *model.WishClaim) error
		wantErrCode int
	}{
		{
			name: "claim success",
			wishFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
				return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusPending}, nil
			},
			createFn: func(tx *gorm.DB, claim *model.WishClaim) error {
				claim.ID = 7
				return nil
			},
			wantErrCode: 0,
		},
		{
			name: "wish already claimed",
			wishFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
				return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusClaimed}, nil
			},
			createFn:    func(tx *gorm.DB, claim *model.WishClaim) error { return nil },
			wantErrCode: constants.CodeWishAlreadyClaimed,
		},
		{
			name: "self claim forbidden",
			wishFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
				return &model.Wish{ID: id, UserID: 2, Status: constants.WishStatusPending}, nil
			},
			createFn:    func(tx *gorm.DB, claim *model.WishClaim) error { return nil },
			wantErrCode: constants.CodeWishStatusInvalid,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			wishRepo := &mockWishRepo{
				findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
					// 转换为真实签名：mock 忽略 tx
					return tt.wishFn(nil, id)
				},
				updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { return nil },
			}
			claimRepo := &mockClaimRepo{
				createWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error {
					return tt.createFn(nil, claim)
				},
			}
			badge := &mockBadge{grantFirstClaimFn: func(userID uint64) error { return nil }}
			svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())
			claim, err := svc.Claim(context.Background(), 2, 5, "127.0.0.1", "req-5")
			if tt.wantErrCode == 0 {
				if err != nil {
					t.Fatalf("claim should succeed, got %v", err)
				}
				if claim.WishID != 5 {
					t.Fatalf("unexpected wish id %d", claim.WishID)
				}
				return
			}
			var appErr *util.AppError
			if !errors.As(err, &appErr) || appErr.Code != tt.wantErrCode {
				t.Fatalf("expected code %d, got %v", tt.wantErrCode, err)
			}
		})
	}
}

// 进度保存到 100% 也不自动完成，只进入圆梦中。
func TestWishClaimService_UpdateProgress_NotAutoComplete(t *testing.T) {
	t.Parallel()
	wishRepo := &mockWishRepo{
		findByIDFn: func(id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusClaimed}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { return nil },
	}
	claimRepo := &mockClaimRepo{
		findByIDFn: func(id uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: id, WishID: 5, UserID: 2, Progress: 40, Status: constants.WishStatusInProgress}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { return nil },
	}
	badge := &mockBadge{}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())

	claim, err := svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 100, Note: "做到了！", IsMilestone: true}, "127.0.0.1", "req-6")
	if err != nil {
		t.Fatalf("update progress: %v", err)
	}
	if claim.Status != constants.WishStatusInProgress {
		t.Fatalf("expected in_progress, got %s", claim.Status)
	}
	if claim.Progress != 100 {
		t.Fatalf("expected progress 100, got %d", claim.Progress)
	}
	if claim.MilestoneCount != 1 {
		t.Fatalf("expected milestone count 1, got %d", claim.MilestoneCount)
	}
}

// 待确认期间不允许修改进度。
func TestWishClaimService_UpdateProgress_BlockedWhilePending(t *testing.T) {
	t.Parallel()
	claimRepo := &mockClaimRepo{
		findByIDFn: func(id uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: id, WishID: 5, UserID: 2, Progress: 100, Status: constants.WishStatusPendingConfirmation}, nil
		},
	}
	svc := NewWishClaimService(&mockTx{}, &mockWishRepo{}, claimRepo, &mockUserRepo{}, &mockBadge{}, &mockAudit{}, testLogger())
	_, err := svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 90}, "127.0.0.1", "req-6")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeClaimStatusInvalid {
		t.Fatalf("expected claim status invalid, got %v", err)
	}
}

// 提交完成：进入待确认，进度保留 100%，不发放完成徽章。
func TestWishClaimService_Complete_Submit(t *testing.T) {
	t.Parallel()
	var savedClaim *model.WishClaim
	var savedWish *model.Wish
	wishRepo := &mockWishRepo{
		findByIDFn: func(id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusInProgress}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { savedWish = wish; return nil },
	}
	claimRepo := &mockClaimRepo{
		findByIDFn: func(id uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: id, WishID: 5, UserID: 2, Progress: 80, Status: constants.WishStatusInProgress, RejectReason: "旧原因"}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { savedClaim = claim; return nil },
	}
	badge := &mockBadge{grantCompletionFn: func(userID uint64) error {
		t.Fatalf("completion badges must not be granted before approval")
		return nil
	}}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())

	claim, err := svc.Complete(context.Background(), 2, 9, dto.CompleteClaimRequest{Note: "完成啦"}, "127.0.0.1", "req-7")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if claim.Status != constants.WishStatusPendingConfirmation {
		t.Fatalf("expected pending_confirmation, got %s", claim.Status)
	}
	if claim.Progress != 100 {
		t.Fatalf("expected progress 100, got %d", claim.Progress)
	}
	if claim.RejectReason != "" {
		t.Fatalf("reject reason should be cleared on resubmit")
	}
	if savedClaim == nil || savedClaim.RejectReason != "" {
		t.Fatalf("persisted claim reject reason should be cleared")
	}
	if savedWish == nil || savedWish.Status != constants.WishStatusPendingConfirmation {
		t.Fatalf("wish should be pending_confirmation, got %+v", savedWish)
	}
}

// 待确认期间重复提交：保持原状态，幂等返回。
func TestWishClaimService_Complete_DuplicateIdempotent(t *testing.T) {
	t.Parallel()
	updated := false
	wishRepo := &mockWishRepo{
		findByIDFn: func(id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { updated = true; return nil },
	}
	claimRepo := &mockClaimRepo{
		findByIDFn: func(id uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: id, WishID: 5, UserID: 2, Progress: 100, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { updated = true; return nil },
	}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, &mockBadge{}, &mockAudit{}, testLogger())
	claim, err := svc.Complete(context.Background(), 2, 9, dto.CompleteClaimRequest{Note: "再次提交"}, "127.0.0.1", "req-8")
	if err != nil {
		t.Fatalf("duplicate complete should be idempotent, got %v", err)
	}
	if claim.Status != constants.WishStatusPendingConfirmation {
		t.Fatalf("expected pending_confirmation, got %s", claim.Status)
	}
	if updated {
		t.Fatalf("duplicate submit must not write again")
	}
}

// 发布者验收通过：转为已完成并触发完成徽章与排行榜缓存失效。
func TestWishClaimService_Review_Approve(t *testing.T) {
	t.Parallel()
	var savedClaim *model.WishClaim
	badged := false
	invalidated := false
	wishRepo := &mockWishRepo{
		findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { return nil },
	}
	claimRepo := &mockClaimRepo{
		findByWishFn: func(wishID uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: 9, WishID: wishID, UserID: 2, Progress: 100, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { savedClaim = claim; return nil },
	}
	badge := &mockBadge{
		grantCompletionFn: func(userID uint64) error { badged = true; return nil },
		invalidateBoardFn: func(ctx context.Context) error { invalidated = true; return nil },
	}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())
	claim, err := svc.Review(context.Background(), 1, 5, dto.ReviewWishRequest{Approved: true}, "127.0.0.1", "req-9")
	if err != nil {
		t.Fatalf("review approve: %v", err)
	}
	if claim.Status != constants.WishStatusCompleted {
		t.Fatalf("expected completed, got %s", claim.Status)
	}
	if savedClaim == nil || savedClaim.RejectReason != "" {
		t.Fatalf("approved claim should be saved without reject reason")
	}
	if !badged {
		t.Fatalf("completion badges should be granted on approval")
	}
	if !invalidated {
		t.Fatalf("leaderboard cache should be invalidated on approval")
	}
}

// 发布者退回：必须填写原因，恢复圆梦中，进度保留，等待圆梦人调整后再提交。
func TestWishClaimService_Review_Reject(t *testing.T) {
	t.Parallel()
	var savedClaim *model.WishClaim
	var savedWish *model.Wish
	wishRepo := &mockWishRepo{
		findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, wish *model.Wish) error { savedWish = wish; return nil },
	}
	claimRepo := &mockClaimRepo{
		findByWishFn: func(wishID uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: 9, WishID: wishID, UserID: 2, Progress: 100, Status: constants.WishStatusPendingConfirmation}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { savedClaim = claim; return nil },
	}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, &mockBadge{}, &mockAudit{}, testLogger())

	// 未填原因应被拒绝。
	_, err := svc.Review(context.Background(), 1, 5, dto.ReviewWishRequest{Approved: false}, "127.0.0.1", "req-10")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeValidationFailed {
		t.Fatalf("expected validation error for empty reason, got %v", err)
	}

	claim, err := svc.Review(context.Background(), 1, 5, dto.ReviewWishRequest{Approved: false, Reason: "还差验收材料"}, "127.0.0.1", "req-10")
	if err != nil {
		t.Fatalf("review reject: %v", err)
	}
	if claim.Status != constants.WishStatusInProgress {
		t.Fatalf("expected in_progress after reject, got %s", claim.Status)
	}
	if claim.Progress != 100 {
		t.Fatalf("progress should be kept at 100, got %d", claim.Progress)
	}
	if savedClaim == nil || savedClaim.RejectReason != "还差验收材料" {
		t.Fatalf("reject reason not saved: %+v", savedClaim)
	}
	if savedWish == nil || savedWish.Status != constants.WishStatusInProgress {
		t.Fatalf("wish should return to in_progress, got %+v", savedWish)
	}
}

// 非发布者不能验收；非待确认状态不能验收。
func TestWishClaimService_Review_Forbidden(t *testing.T) {
	t.Parallel()
	wishRepo := &mockWishRepo{
		findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: constants.WishStatusInProgress}, nil
		},
	}
	svc := NewWishClaimService(&mockTx{}, wishRepo, &mockClaimRepo{}, &mockUserRepo{}, &mockBadge{}, &mockAudit{}, testLogger())

	// 非发布者。
	_, err := svc.Review(context.Background(), 3, 5, dto.ReviewWishRequest{Approved: true}, "127.0.0.1", "req-11")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeWishNotOwner {
		t.Fatalf("expected wish not owner, got %v", err)
	}

	// 发布者但状态不是待确认。
	_, err = svc.Review(context.Background(), 1, 5, dto.ReviewWishRequest{Approved: true}, "127.0.0.1", "req-11")
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeClaimStatusInvalid {
		t.Fatalf("expected claim status invalid, got %v", err)
	}
}
