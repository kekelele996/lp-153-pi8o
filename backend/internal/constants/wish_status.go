package constants

// 心愿状态机：pending -> claimed -> in_progress -> pending_confirmation -> completed。
// 圆梦人提交完成后进入 pending_confirmation（待确认），由发布者验收：通过才 completed，退回回到 in_progress。
const (
	WishStatusPending             = "pending"
	WishStatusClaimed             = "claimed"
	WishStatusInProgress          = "in_progress"
	WishStatusPendingConfirmation = "pending_confirmation"
	WishStatusCompleted           = "completed"
)

// ValidWishStatuses 状态筛选白名单。
var ValidWishStatuses = []string{
	WishStatusPending,
	WishStatusClaimed,
	WishStatusInProgress,
	WishStatusPendingConfirmation,
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
	case WishStatusPendingConfirmation:
		return "待确认"
	case WishStatusCompleted:
		return "已完成"
	default:
		return "未知"
	}
}
