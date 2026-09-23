package constants

// 心愿状态机：pending -> claimed -> in_progress -> pending_confirm -> completed。
// 发布者验收不通过时 pending_confirm -> in_progress（圆梦人调整进度后可再次提交）。
const (
	WishStatusPending        = "pending"
	WishStatusClaimed        = "claimed"
	WishStatusInProgress     = "in_progress"
	WishStatusPendingConfirm = "pending_confirm"
	WishStatusCompleted      = "completed"
)

// ValidWishStatuses 状态筛选白名单。
var ValidWishStatuses = []string{
	WishStatusPending,
	WishStatusClaimed,
	WishStatusInProgress,
	WishStatusPendingConfirm,
	WishStatusCompleted,
}

// WishStatusText 状态文本（formatters 亦引用）。
func WishStatusText(status string) string {
	switch status {
	case WishStatusPending:
		return "待认领"
	case WishStatusClaimed:
		return "已被认领"
	case WishStatusInProgress:
		return "圆梦中"
	case WishStatusPendingConfirm:
		return "待确认"
	case WishStatusCompleted:
		return "已完成"
	default:
		return "未知"
	}
}
