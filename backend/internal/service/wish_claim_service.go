package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"gorm.io/gorm"

	"github.com/wishwall/wishwall/internal/constants"
	"github.com/wishwall/wishwall/internal/dto"
	"github.com/wishwall/wishwall/internal/model"
	"github.com/wishwall/wishwall/internal/repository"
	"github.com/wishwall/wishwall/internal/util"
)

// WishClaimService 心愿认领服务：认领（事务+行锁）、进度更新、提交完成、发布者验收、我的认领。
type WishClaimService interface {
	Claim(ctx context.Context, userID, wishID uint64, ip, requestID string) (*model.WishClaim, error)
	UpdateProgress(ctx context.Context, userID, claimID uint64, req dto.UpdateProgressRequest, ip, requestID string) (*model.WishClaim, error)
	Complete(ctx context.Context, userID, claimID uint64, req dto.CompleteClaimRequest, ip, requestID string) (*model.WishClaim, error)
	Review(ctx context.Context, publisherID, wishID uint64, req dto.ReviewWishRequest, ip, requestID string) (*model.WishClaim, error)
	ListMine(userID uint64, q dto.PageQuery) (*dto.PageResult, error)
	GetByWishID(userID, wishID uint64) (*model.WishClaim, error)
}

type wishClaimService struct {
	tx     repository.TxManager
	wish   repository.WishRepository
	claim  repository.WishClaimRepository
	user   repository.UserRepository
	badge  BadgeService
	audit  AuditService
	logger *slog.Logger
}

// NewWishClaimService 构造认领服务。
func NewWishClaimService(
	tx repository.TxManager,
	wish repository.WishRepository,
	claim repository.WishClaimRepository,
	user repository.UserRepository,
	badge BadgeService,
	audit AuditService,
	logger *slog.Logger,
) WishClaimService {
	return &wishClaimService{tx: tx, wish: wish, claim: claim, user: user, badge: badge, audit: audit, logger: logger}
}

// Claim 认领心愿：SELECT ... FOR UPDATE 锁定心愿行防止并发重复认领。
func (s *wishClaimService) Claim(ctx context.Context, userID, wishID uint64, ip, requestID string) (*model.WishClaim, error) {
	var created *model.WishClaim
	err := s.tx.Transaction(func(tx *gorm.DB) error {
		wish, err := s.wish.FindByIDForUpdate(tx, wishID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(constants.CodeWishNotFound, constants.MsgWishNotFound, err)
			}
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if wish.UserID == userID {
			return util.NewAppError(constants.CodeWishStatusInvalid, "不能认领自己发布的心愿", errors.New("self claim forbidden"))
		}
		if wish.Status != constants.WishStatusPending {
			return util.NewAppError(constants.CodeWishAlreadyClaimed, constants.MsgWishAlreadyClaimed, errors.New("wish status not pending"))
		}
		claim := &model.WishClaim{
			WishID: wishID, UserID: userID,
			Progress: 0, Status: constants.WishStatusClaimed,
		}
		if err := s.claim.CreateWithTx(tx, claim); err != nil {
			if errors.Is(err, repository.ErrConflict) {
				return util.NewAppError(constants.CodeWishAlreadyClaimed, constants.MsgWishAlreadyClaimed, err)
			}
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		wish.Status = constants.WishStatusClaimed
		if err := s.wish.UpdateWithTx(tx, wish); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		created = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogWishClaimed, "wish_id", wishID, "claim_id", created.ID, "user_id", userID)
	_ = s.audit.Record(&model.AuditLog{
		UserID: userID, Action: "claim_wish", EntityType: "wish_claim", EntityID: u64str(created.ID),
		Detail: "认领心愿，成为圆梦人", IP: ip, RequestID: requestID,
	})
	if err := s.badge.GrantFirstClaim(userID); err != nil {
		s.logger.Warn("grant first claim badge failed", "error", err)
	}
	return created, nil
}

// UpdateProgress 更新圆梦进度。
// 进度达到 100% 仅保存进度，不会自动完成；需圆梦人显式“提交完成”才进入待确认。
// 待确认/已完成期间不允许修改进度（待确认重复提交走 Complete 的幂等分支）。
func (s *wishClaimService) UpdateProgress(ctx context.Context, userID, claimID uint64, req dto.UpdateProgressRequest, ip, requestID string) (*model.WishClaim, error) {
	var updated *model.WishClaim
	err := s.tx.Transaction(func(tx *gorm.DB) error {
		claim, err := s.claim.FindByID(claimID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(constants.CodeClaimNotFound, constants.MsgClaimNotFound, err)
			}
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if claim.UserID != userID {
			return util.NewAppError(constants.CodeClaimNotOwner, "只有圆梦人才能更新进度", errors.New("claim owner mismatch"))
		}
		switch claim.Status {
		case constants.WishStatusCompleted:
			return util.NewAppError(constants.CodeClaimStatusInvalid, "心愿已完成，不能再修改进度", errors.New("claim already completed"))
		case constants.WishStatusPendingConfirmation:
			return util.NewAppError(constants.CodeClaimStatusInvalid, "已提交发布者确认，暂不能修改进度", errors.New("claim pending confirmation"))
		}
		wish, err := s.wish.FindByID(claim.WishID)
		if err != nil {
			return util.NewAppError(constants.CodeWishNotFound, constants.MsgWishNotFound, err)
		}
		claim.Progress = req.Progress
		if req.Note != "" {
			claim.LatestNote = req.Note
		}
		if req.IsMilestone {
			claim.MilestoneCount++
		}
		// 保存进度不会触发验收流转；认领后首次更新进度即进入圆梦中。
		if claim.Status == constants.WishStatusClaimed {
			claim.Status = constants.WishStatusInProgress
		}
		wish.Status = constants.WishStatusInProgress
		if err := s.claim.UpdateWithTx(tx, claim); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if err := s.wish.UpdateWithTx(tx, wish); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		updated = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogClaimProgress, "claim_id", claimID, "progress", updated.Progress, "user_id", userID)
	_ = s.audit.Record(&model.AuditLog{
		UserID: userID, Action: "update_progress", EntityType: "wish_claim", EntityID: u64str(claimID),
		Detail: "更新圆梦进度至 " + itoa(updated.Progress) + "%", IP: ip, RequestID: requestID,
	})
	return updated, nil
}

// Complete 提交完成：进度置 100%，心愿与认领进入待确认（pending_confirmation），等待发布者验收。
// 待确认期间重复提交保持原状态（幂等返回，不重复记里程碑），通过验收后才真正完成。
func (s *wishClaimService) Complete(ctx context.Context, userID, claimID uint64, req dto.CompleteClaimRequest, ip, requestID string) (*model.WishClaim, error) {
	var submitted bool
	var updated *model.WishClaim
	err := s.tx.Transaction(func(tx *gorm.DB) error {
		claim, err := s.claim.FindByID(claimID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(constants.CodeClaimNotFound, constants.MsgClaimNotFound, err)
			}
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if claim.UserID != userID {
			return util.NewAppError(constants.CodeClaimNotOwner, "只有圆梦人才能提交完成", errors.New("claim owner mismatch"))
		}
		// 已完成不可重复提交。
		if claim.Status == constants.WishStatusCompleted {
			return util.NewAppError(constants.CodeClaimStatusInvalid, "心愿已完成", errors.New("claim already completed"))
		}
		wish, err := s.wish.FindByID(claim.WishID)
		if err != nil {
			return util.NewAppError(constants.CodeWishNotFound, constants.MsgWishNotFound, err)
		}
		// 待确认期间重复提交：保持原状态，直接幂等返回。
		if claim.Status == constants.WishStatusPendingConfirmation {
			updated = claim
			return nil
		}
		claim.Progress = 100
		if req.Note != "" {
			claim.LatestNote = req.Note
			wish.CompletionNote = req.Note
		}
		claim.MilestoneCount++
		claim.RejectReason = ""
		claim.Status = constants.WishStatusPendingConfirmation
		wish.Status = constants.WishStatusPendingConfirmation
		if err := s.claim.UpdateWithTx(tx, claim); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if err := s.wish.UpdateWithTx(tx, wish); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		updated = claim
		submitted = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if submitted {
		s.logger.Info(constants.LogClaimSubmitted, "claim_id", claimID, "wish_id", updated.WishID, "user_id", userID)
		_ = s.audit.Record(&model.AuditLog{
			UserID: userID, Action: "submit_complete", EntityType: "wish_claim", EntityID: u64str(claimID),
			Detail: "提交完成，等待发布者验收", IP: ip, RequestID: requestID,
		})
	}
	return updated, nil
}

// Review 发布者验收：通过则心愿/认领转为已完成（计入排行榜、发放成就）；退回则恢复圆梦中并记录原因。
func (s *wishClaimService) Review(ctx context.Context, publisherID, wishID uint64, req dto.ReviewWishRequest, ip, requestID string) (*model.WishClaim, error) {
	var approved bool
	var updated *model.WishClaim
	err := s.tx.Transaction(func(tx *gorm.DB) error {
		wish, err := s.wish.FindByIDForUpdate(tx, wishID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return util.NewAppError(constants.CodeWishNotFound, constants.MsgWishNotFound, err)
			}
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if wish.UserID != publisherID {
			return util.NewAppError(constants.CodeWishNotOwner, constants.MsgWishNotOwner, errors.New("wish owner mismatch"))
		}
		if wish.Status != constants.WishStatusPendingConfirmation {
			return util.NewAppError(constants.CodeClaimStatusInvalid, constants.MsgReviewNotPending, errors.New("wish not pending confirmation"))
		}
		claim, err := s.claim.FindByWishID(wishID)
		if err != nil {
			return util.NewAppError(constants.CodeClaimNotFound, constants.MsgClaimNotFound, err)
		}
		if !req.Approved {
			reason := strings.TrimSpace(req.Reason)
			if reason == "" {
				return util.NewAppError(constants.CodeValidationFailed, constants.MsgRejectReasonNeeded, errors.New("reject reason required"))
			}
			claim.Status = constants.WishStatusInProgress
			claim.RejectReason = reason
			wish.Status = constants.WishStatusInProgress
		} else {
			claim.Status = constants.WishStatusCompleted
			claim.RejectReason = ""
			wish.Status = constants.WishStatusCompleted
		}
		if err := s.claim.UpdateWithTx(tx, claim); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		if err := s.wish.UpdateWithTx(tx, wish); err != nil {
			return util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
		}
		approved = req.Approved
		updated = claim
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogWishReviewed, "wish_id", wishID, "claim_id", updated.ID, "approved", approved, "user_id", publisherID)
	if approved {
		_ = s.audit.Record(&model.AuditLog{
			UserID: publisherID, Action: "approve_wish", EntityType: "wish", EntityID: u64str(wishID),
			Detail: "验收通过，心愿完成", IP: ip, RequestID: requestID,
		})
		if err := s.badge.GrantCompletionBadges(updated.UserID); err != nil {
			s.logger.Warn("grant completion badge failed", "error", err)
		}
		if err := s.badge.InvalidateLeaderboard(ctx); err != nil {
			s.logger.Warn("invalidate leaderboard cache failed", "error", err)
		}
	} else {
		_ = s.audit.Record(&model.AuditLog{
			UserID: publisherID, Action: "reject_wish", EntityType: "wish", EntityID: u64str(wishID),
			Detail: "验收退回：" + strings.TrimSpace(req.Reason), IP: ip, RequestID: requestID,
		})
	}
	return updated, nil
}

func (s *wishClaimService) ListMine(userID uint64, q dto.PageQuery) (*dto.PageResult, error) {
	page, size := q.Normalize()
	total, err := s.claim.CountByUserID(userID)
	if err != nil {
		return nil, util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
	}
	claims, err := s.claim.ListByUserID(userID, (page-1)*size, size)
	if err != nil {
		return nil, util.NewAppError(constants.CodeInternalError, constants.MsgInternalError, err)
	}
	items := make([]dto.WishClaimResponse, 0, len(claims))
	for i := range claims {
		wishTitle := ""
		if wish, werr := s.wish.FindByID(claims[i].WishID); werr == nil {
			wishTitle = wish.Title
		}
		name := ""
		if u, uerr := s.user.FindByID(userID); uerr == nil {
			name = u.Nickname
		}
		items = append(items, dto.ToWishClaimResponse(&claims[i], wishTitle, name))
	}
	return &dto.PageResult{Items: items, Total: total, Page: page, PageSize: size}, nil
}

// GetByWishID 查询某心愿的认领（心愿详情页圆梦人模块复用）。
func (s *wishClaimService) GetByWishID(userID, wishID uint64) (*model.WishClaim, error) {
	claim, err := s.claim.FindByWishID(wishID)
	if err != nil {
		return nil, util.NewAppError(constants.CodeClaimNotFound, constants.MsgClaimNotFound, err)
	}
	if claim.UserID != userID {
		return nil, util.NewAppError(constants.CodeForbidden, constants.MsgNeedLogin+"：无权限查看该认领", errors.New("claim visibility forbidden"))
	}
	return claim, nil
}
