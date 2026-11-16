// schedule.go 签到排程配置。
//
// 调度逻辑本身已并入 internal/scheduler（池遍历账号、逐个调 Checkin）：
// Trae 不再有自己的调度器，这里只保留 trae.json 里的配置字段定义，
// 供配置读写与面板展示使用。
package trae

// CheckinSchedule 签到排程配置。
type CheckinSchedule struct {
	// Enabled 是否启用自动签到（缺省 false：需用户显式开启）。
	Enabled bool `json:"checkin_enabled"`
	// Hours 每日执行的整点（缺省 [9]）。
	Hours []int `json:"checkin_hours"`
	// GapMS 账号间间隔（毫秒；缺省 800，防连打触发风控）。
	//
	// 统一流程后由池的并发上限承担，保留字段仅为兼容既有 trae.json。
	GapMS int `json:"checkin_gap_ms"`
}
