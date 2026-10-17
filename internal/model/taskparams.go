package model

// TaskParams is the panel-editable daily schedule of the task center
// (panel ↔ scheduler wire format; agentpoints 定时任务参数的对齐迁移).
// Hours are Asia/Shanghai wall clock; minutes are interval lengths.
type TaskParams struct {
	CheckinHours       []int `json:"checkin_hours"`        // 签到触发小时，默认 9,21
	GrowthHours        []int `json:"growth_hours"`         // 成长任务触发小时，默认 9
	ActivityHours      []int `json:"activity_hours"`       // 活跃上报触发小时，默认 10（agentpoints activity_hours）
	TravelHours        []int `json:"travel_hours"`         // 猫猫旅行触发小时，默认 9,21（agentpoints travel_hours）
	BlackcatHours      []int `json:"blackcat_hours"`       // 夜猫子触发小时，默认 23（真实对话只在该小时起的夜间窗口内计分）
	ProbeMinutes       int   `json:"probe_minutes"`        // 国模池额度刷新间隔（分钟），默认 5
	GoogleProbeMinutes int   `json:"google_probe_minutes"` // Google 额度刷新间隔（分钟），默认 15
	RefreshMinutes     int   `json:"refresh_minutes"`      // Token 预刷新间隔（分钟），默认 60
}
