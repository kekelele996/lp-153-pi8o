package dto

import (
	"github.com/wishwall/wishwall/internal/model"
)

// UpdateProgressRequest 更新圆梦进度入参（百分比+文字+里程碑打卡）。
type UpdateProgressRequest struct {
	Progress    int    `json:"progress" binding:"required,min=0,max=100"`
	Note        string `json:"note" binding:"omitempty,max=1000"`
	IsMilestone bool   `json:"is_milestone"`
}

// CompleteClaimRequest 提交完成入参（进入待确认，等待发布者验收）。
type CompleteClaimRequest struct {
	Note string `json:"note" binding:"omitempty,max=1000"`
}

// RejectClaimRequest 发布者退回入参（必须写明退回原因）。
type RejectClaimRequest struct {
	Reason string `json:"reason" binding:"required,min=2,max=500"`
}

// WishClaimResponse 认领记录返回结构。
type WishClaimResponse struct {
	ID             uint64 `json:"id"`
	WishID         uint64 `json:"wish_id"`
	WishTitle      string `json:"wish_title"`
	UserID         uint64 `json:"user_id"`
	FulfillerName  string `json:"fulfiller_name"`
	Progress       int    `json:"progress"`
	LatestNote     string `json:"latest_note"`
	Status         string `json:"status"`
	MilestoneCount int    `json:"milestone_count"`
	RejectReason   string `json:"reject_reason"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// ToWishClaimResponse 从模型构造返回结构。
func ToWishClaimResponse(c *model.WishClaim, wishTitle, fulfillerName string) WishClaimResponse {
	return WishClaimResponse{
		ID:             c.ID,
		WishID:         c.WishID,
		WishTitle:      wishTitle,
		UserID:         c.UserID,
		FulfillerName:  fulfillerName,
		Progress:       c.Progress,
		LatestNote:     c.LatestNote,
		Status:         c.Status,
		MilestoneCount: c.MilestoneCount,
		RejectReason:   c.RejectReason,
		CreatedAt:      c.CreatedAt.Format("2006-01-02 15:04:05"),
		UpdatedAt:      c.UpdatedAt.Format("2006-01-02 15:04:05"),
	}
}
