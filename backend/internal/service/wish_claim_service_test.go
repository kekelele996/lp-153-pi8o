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

// newConfirmService 构造圆梦人 2 / 发布者 1 / 心愿 5 的认领服务（可注入认领初始状态）。
func newConfirmService(t *testing.T, claimStatus string, progress int, grantCompletion func(userID uint64) error) (WishClaimService, *mockWishRepo, *mockClaimRepo) {
	t.Helper()
	wishRepo := &mockWishRepo{
		findByIDFn: func(id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: claimStatus}, nil
		},
		findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) {
			return &model.Wish{ID: id, UserID: 1, Status: claimStatus}, nil
		},
		updateWithTxFn:   func(tx *gorm.DB, wish *model.Wish) error { return nil },
		countCompletedFn: func(userID uint64) (int64, error) { return 11, nil },
	}
	claimRepo := &mockClaimRepo{
		findByIDFn: func(id uint64) (*model.WishClaim, error) {
			return &model.WishClaim{ID: id, WishID: 5, UserID: 2, Progress: progress, Status: claimStatus, LatestNote: "做到了！"}, nil
		},
		updateWithTxFn: func(tx *gorm.DB, claim *model.WishClaim) error { return nil },
	}
	badge := &mockBadge{
		grantCompletionFn: grantCompletion,
	}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())
	return svc, wishRepo, claimRepo
}

func TestWishClaimService_UpdateProgress_SubmitPendingConfirm(t *testing.T) {
	t.Parallel()
	completionBadgeCalled := false
	svc, _, _ := newConfirmService(t, constants.WishStatusInProgress, 40, func(userID uint64) error {
		completionBadgeCalled = true
		return nil
	})

	// 进度 100：只进入待确认，不完成、不发放完成徽章。
	claim, err := svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 100, Note: "做到了！", IsMilestone: true}, "127.0.0.1", "req-6")
	if err != nil {
		t.Fatalf("update progress: %v", err)
	}
	if claim.Status != constants.WishStatusPendingConfirm {
		t.Fatalf("expected pending_confirm, got %s", claim.Status)
	}
	if claim.Progress != 100 {
		t.Fatalf("expected progress 100, got %d", claim.Progress)
	}
	if claim.MilestoneCount != 1 {
		t.Fatalf("expected milestone count 1, got %d", claim.MilestoneCount)
	}
	if completionBadgeCalled {
		t.Fatal("completion badges must not be granted before publisher approval")
	}
}

func TestWishClaimService_UpdateProgress_ResubmitKeepsState(t *testing.T) {
	t.Parallel()
	svc, _, claimRepo := newConfirmService(t, constants.WishStatusPendingConfirm, 100, func(userID uint64) error {
		t.Fatal("completion badge must not be granted on resubmit")
		return nil
	})
	updated := false
	claimRepo.updateWithTxFn = func(tx *gorm.DB, claim *model.WishClaim) error {
		updated = true
		return nil
	}

	// 待确认期间重复提交 100%：保持原状态，不落库更新。
	claim, err := svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 100, IsMilestone: true}, "127.0.0.1", "req-7")
	if err != nil {
		t.Fatalf("resubmit should be idempotent, got %v", err)
	}
	if claim.Status != constants.WishStatusPendingConfirm {
		t.Fatalf("expected pending_confirm, got %s", claim.Status)
	}
	if updated {
		t.Fatal("claim should not be persisted again during pending confirmation")
	}

	// 待确认期间下调进度：拒绝。
	_, err = svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 80}, "127.0.0.1", "req-7")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeClaimStatusInvalid {
		t.Fatalf("expected CodeClaimStatusInvalid, got %v", err)
	}
}

func TestWishClaimService_Approve(t *testing.T) {
	t.Parallel()
	grantedTo := uint64(0)
	svc, wishRepo, _ := newConfirmService(t, constants.WishStatusPendingConfirm, 100, func(userID uint64) error {
		grantedTo = userID
		return nil
	})
	savedWishStatus := ""
	wishRepo.updateWithTxFn = func(tx *gorm.DB, wish *model.Wish) error {
		savedWishStatus = wish.Status
		return nil
	}

	// 发布者验收通过：心愿与认领均完成，完成徽章发给圆梦人。
	claim, err := svc.Approve(context.Background(), 1, 9, "127.0.0.1", "req-8")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if claim.Status != constants.WishStatusCompleted {
		t.Fatalf("expected completed claim, got %s", claim.Status)
	}
	if savedWishStatus != constants.WishStatusCompleted {
		t.Fatalf("expected completed wish, got %s", savedWishStatus)
	}
	if grantedTo != 2 {
		t.Fatalf("completion badge should be granted to fulfiller 2, got %d", grantedTo)
	}

	// 非发布者不能验收。
	_, err = svc.Approve(context.Background(), 3, 9, "127.0.0.1", "req-8")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeWishNotOwner {
		t.Fatalf("expected CodeWishNotOwner, got %v", err)
	}
}

func TestWishClaimService_Approve_WrongState(t *testing.T) {
	t.Parallel()
	// 圆梦中的认领不能验收。
	svc, _, _ := newConfirmService(t, constants.WishStatusInProgress, 60, func(userID uint64) error { return nil })
	_, err := svc.Approve(context.Background(), 1, 9, "127.0.0.1", "req-8")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeClaimStatusInvalid {
		t.Fatalf("expected CodeClaimStatusInvalid, got %v", err)
	}
}

func TestWishClaimService_Reject(t *testing.T) {
	t.Parallel()
	svc, wishRepo, claimRepo := newConfirmService(t, constants.WishStatusPendingConfirm, 100, func(userID uint64) error {
		t.Fatal("completion badge must not be granted on rejection")
		return nil
	})
	savedClaim := &model.WishClaim{}
	savedWish := &model.Wish{}
	claimRepo.updateWithTxFn = func(tx *gorm.DB, claim *model.WishClaim) error {
		savedClaim = claim
		return nil
	}
	wishRepo.updateWithTxFn = func(tx *gorm.DB, wish *model.Wish) error {
		savedWish = wish
		return nil
	}

	// 发布者退回：写明原因，恢复进行中。
	claim, err := svc.Reject(context.Background(), 1, 9, dto.RejectClaimRequest{Reason: "照片还没补全，请再补充记录"}, "127.0.0.1", "req-9")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if claim.Status != constants.WishStatusInProgress {
		t.Fatalf("expected in_progress claim, got %s", claim.Status)
	}
	if savedClaim.RejectReason == "" {
		t.Fatal("reject reason should be persisted on claim")
	}
	if savedWish.Status != constants.WishStatusInProgress {
		t.Fatalf("expected in_progress wish, got %s", savedWish.Status)
	}

	// 非发布者不能退回。
	_, err = svc.Reject(context.Background(), 3, 9, dto.RejectClaimRequest{Reason: "无关人员"}, "127.0.0.1", "req-9")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeWishNotOwner {
		t.Fatalf("expected CodeWishNotOwner, got %v", err)
	}
}

func TestWishClaimService_Reject_CompletedState(t *testing.T) {
	t.Parallel()
	// 已完成的认领不能再退回（退回原因必填由 handler 层 binding 校验）。
	svc, _, _ := newConfirmService(t, constants.WishStatusCompleted, 100, func(userID uint64) error { return nil })
	_, err := svc.Reject(context.Background(), 1, 9, dto.RejectClaimRequest{Reason: "已完成的不能再退"}, "127.0.0.1", "req-9")
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != constants.CodeClaimStatusInvalid {
		t.Fatalf("expected CodeClaimStatusInvalid, got %v", err)
	}
}

// TestWishClaimService_ConfirmationLifecycle 用有状态内存假仓储串起完整验收生命周期：
// 提交 → 重复提交保持原状态 → 退回 → 调整进度 → 再次提交 → 通过 → 重复验收被拒。
func TestWishClaimService_ConfirmationLifecycle(t *testing.T) {
	t.Parallel()
	// 共享可变状态，模拟数据库行；UpdateWithTx 为 noop（service 持同一指针，变更即时生效）。
	wish := &model.Wish{ID: 5, UserID: 1, Status: constants.WishStatusClaimed}
	claim := &model.WishClaim{ID: 9, WishID: 5, UserID: 2, Progress: 0, Status: constants.WishStatusClaimed}
	completionGrants := 0
	wishRepo := &mockWishRepo{
		findByIDFn:          func(id uint64) (*model.Wish, error) { return wish, nil },
		findByIDForUpdateFn: func(tx *gorm.DB, id uint64) (*model.Wish, error) { return wish, nil },
		updateWithTxFn:      func(tx *gorm.DB, w *model.Wish) error { return nil },
		countCompletedFn:    func(userID uint64) (int64, error) { return 1, nil },
	}
	claimRepo := &mockClaimRepo{
		findByIDFn:     func(id uint64) (*model.WishClaim, error) { return claim, nil },
		updateWithTxFn: func(tx *gorm.DB, c *model.WishClaim) error { return nil },
	}
	badge := &mockBadge{grantCompletionFn: func(userID uint64) error { completionGrants++; return nil }}
	svc := NewWishClaimService(&mockTx{}, wishRepo, claimRepo, &mockUserRepo{}, badge, &mockAudit{}, testLogger())

	// 1. 更新到 60%：圆梦中。
	c, err := svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 60, Note: "做了一半"}, "ip", "r1")
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if c.Status != constants.WishStatusInProgress || wish.Status != constants.WishStatusInProgress {
		t.Fatalf("step 1: expected in_progress, claim=%s wish=%s", c.Status, wish.Status)
	}

	// 2. 提交完成（100%）：只进入待确认，进度保留 100%。
	c, err = svc.Complete(context.Background(), 2, 9, dto.CompleteClaimRequest{Note: "全部做到了"}, "ip", "r2")
	if err != nil {
		t.Fatalf("step 2: %v", err)
	}
	if c.Status != constants.WishStatusPendingConfirm || wish.Status != constants.WishStatusPendingConfirm {
		t.Fatalf("step 2: expected pending_confirm, claim=%s wish=%s", c.Status, wish.Status)
	}
	if c.Progress != 100 || completionGrants != 0 {
		t.Fatalf("step 2: progress=%d completionGrants=%d", c.Progress, completionGrants)
	}

	// 3. 待确认期间重复提交：保持原状态，里程碑不重复累加。
	milestones := c.MilestoneCount
	c, err = svc.Complete(context.Background(), 2, 9, dto.CompleteClaimRequest{Note: "再次提交"}, "ip", "r3")
	if err != nil {
		t.Fatalf("step 3: %v", err)
	}
	if c.Status != constants.WishStatusPendingConfirm || c.MilestoneCount != milestones {
		t.Fatalf("step 3: expected unchanged pending_confirm and milestone %d, got %s %d", milestones, c.Status, c.MilestoneCount)
	}

	// 4. 待确认期间下调进度：被拒绝。
	if _, err = svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 50}, "ip", "r4"); err == nil {
		t.Fatal("step 4: lowering progress during pending confirmation must fail")
	}

	// 5. 非发布者不能退回。
	if _, err = svc.Reject(context.Background(), 3, 9, dto.RejectClaimRequest{Reason: "陌生人来退"}, "ip", "r5"); err == nil {
		t.Fatal("step 5: stranger must not reject")
	}

	// 6. 发布者退回写明原因：恢复进行中，进度仍保留 100%，完成徽章仍未发。
	c, err = svc.Reject(context.Background(), 1, 9, dto.RejectClaimRequest{Reason: "缺少完成照片，请补充"}, "ip", "r6")
	if err != nil {
		t.Fatalf("step 6: %v", err)
	}
	if c.Status != constants.WishStatusInProgress || wish.Status != constants.WishStatusInProgress {
		t.Fatalf("step 6: expected in_progress, claim=%s wish=%s", c.Status, wish.Status)
	}
	if c.Progress != 100 || c.RejectReason == "" || completionGrants != 0 {
		t.Fatalf("step 6: progress=%d reason=%q grants=%d", c.Progress, c.RejectReason, completionGrants)
	}

	// 7. 圆梦人调整进度（退回原因被清除），随后再次提交。
	c, err = svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 90, Note: "补充了照片"}, "ip", "r7")
	if err != nil {
		t.Fatalf("step 7a: %v", err)
	}
	if c.RejectReason != "" {
		t.Fatalf("step 7a: reject reason should be cleared, got %q", c.RejectReason)
	}
	c, err = svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 100, Note: "这次齐了"}, "ip", "r7")
	if err != nil {
		t.Fatalf("step 7b: %v", err)
	}
	if c.Status != constants.WishStatusPendingConfirm || wish.Status != constants.WishStatusPendingConfirm {
		t.Fatalf("step 7b: expected pending_confirm, got %s/%s", c.Status, wish.Status)
	}

	// 8. 发布者验收通过：转为已完成，计入完成数/排行榜（发放完成徽章）。
	c, err = svc.Approve(context.Background(), 1, 9, "ip", "r8")
	if err != nil {
		t.Fatalf("step 8: %v", err)
	}
	if c.Status != constants.WishStatusCompleted || wish.Status != constants.WishStatusCompleted {
		t.Fatalf("step 8: expected completed, claim=%s wish=%s", c.Status, wish.Status)
	}
	if completionGrants != 1 {
		t.Fatalf("step 8: completion badge should be granted once, got %d", completionGrants)
	}

	// 9. 已完成后不能再次验收/退回。
	if _, err = svc.Approve(context.Background(), 1, 9, "ip", "r9"); err == nil {
		t.Fatal("step 9: duplicate approve must fail")
	}

	// 10. 已完成后进度不可再修改。
	if _, err = svc.UpdateProgress(context.Background(), 2, 9, dto.UpdateProgressRequest{Progress: 80}, "ip", "r10"); err == nil {
		t.Fatal("step 10: completed claim must not allow progress update")
	}
}
