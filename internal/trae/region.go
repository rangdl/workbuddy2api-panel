package trae

import "strings"

// region.go Trae 服务区域（域）判定。
//
// 为什么必须有这一层：Trae 国内版与国际版是**同一上游的两个域**，不是两个
// 上游、也不是两种协议。域决定四件事——聊天 base、目录 base、用量数据源、
// 签到是否可用。这四项的差异见 docs/trae-intl.md。
//
// 判定原则（对齐参考项目 dsh-connect-trae 的 regionOfCredential）：
//
//	userRegion.region  →  host 后缀  →  （本项目）配置 base
//
// 前两级都来自**凭证自身**，所以不需要用户配置。第三级是本项目特有的兜底：
// 老账号文件没有 Host / UserRegion 字段（升级前导入的），只能按配置里的 base
// 判域——这正是改造前的行为，保留它是为了零回归。
//
// **域是账号属性，不是配置属性。** 改造前 realmOfBase(cfg.ChatBase, ...) 让整份
// trae.json 只能是单域；国际账号与国内账号混在一份配置里时就无法表达。

// ServiceRegion 凭证自身声明的服务区域。
//
// 比 realm 枚举（cn / global）**宽**，多出 us：美国区是真实存在、可识别、
// 但本项目尚未实现的区域。把它显式表示出来是刻意的——参考项目踩过这个坑
// （issue #66）：US 凭据既不被 userRegion 也不被 host 认出，于是三级回退
// 落到「按 edition 猜」，被判成 SG，所有请求打到 *.trae.ai 得 401，再被
// 包成 HTTP 500，用户看到的是「插件坏了」并反复重登。
type ServiceRegion string

const (
	// RegionCN 国内版。
	RegionCN ServiceRegion = "cn"
	// RegionGlobal 国际版（参考项目内部叫 ai 桶，本项目统一用 global）。
	RegionGlobal ServiceRegion = "global"
	// RegionUS 美国区：可识别、未实现。
	//
	// **绝不能回落成 RegionGlobal**——US 集群与 SG 集群的契约不同
	// （US 的 remote 目录 404、llm_utils_chat 回 403 40006），用 SG 的
	// 网关打 US 只会得到 401 并污染熔断判据。
	RegionUS ServiceRegion = "us"
)

// IsServable 报告该区域是否被本实现支持（只有 cn / global）。
func (r ServiceRegion) IsServable() bool {
	return r == RegionCN || r == RegionGlobal
}

// Realm 把服务区域映射为网关的域值。
//
// us 返回空串：**不能**映射成 global，那正是要避免的静默误路由。
// 调用方拿到空串必须把账号排除出池（见 backend/trae 的 Wrap）。
func (r ServiceRegion) Realm() string {
	if !r.IsServable() {
		return ""
	}
	return string(r)
}

// RegionOfUserRegion 从凭证的 userRegion 声明判定区域。
//
// 两种形态都接受（均为实测）：
//   - 桌面版 storage.json 里是对象 {"region":"CN","_aiRegion":"CN"}
//   - App 日志里是裸值，且**是小写** "sg"
//
// 所以判定必须大小写不敏感。
func RegionOfUserRegion(value any) (ServiceRegion, bool) {
	raw := value
	if m, ok := value.(map[string]any); ok {
		raw = m["region"]
		if raw == nil {
			raw = m["_aiRegion"]
		}
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	switch lower := trimLower(s); lower {
	case "cn":
		return RegionCN, true
	case "sg", "ai", "global", "intl", "international":
		// 国际版在不同位置被标成 sg / ai；global / intl 是本项目自己的叫法。
		return RegionGlobal, true
	case "us":
		return RegionUS, true
	}
	return "", false
}

// RegionOfHost 从凭证 host 的后缀判定区域。
//
// 判定顺序敏感：traeapi.us 必须先于 trae.ai 判断（它不是 .trae.ai 的特例，
// 是另一个集群），trae.com.cn 必须先于 trae.com。
func RegionOfHost(host string) (ServiceRegion, bool) {
	h := trimLower(host)
	if h == "" {
		return "", false
	}
	// 去掉协议与路径，只留主机名。
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	switch {
	case h == "traeapi.us" || strings.HasSuffix(h, ".traeapi.us"):
		return RegionUS, true
	case h == "trae.ai" || strings.HasSuffix(h, ".trae.ai"):
		return RegionGlobal, true
	case h == "trae.cn" || strings.HasSuffix(h, ".trae.cn"),
		h == "trae.com.cn" || strings.HasSuffix(h, ".trae.com.cn"):
		return RegionCN, true
	}
	return "", false
}

// RegionOfCredential 从账号自身携带的判据判定区域。
//
// 返回 ok=false 表示凭证没给出任何可识别判据（老账号文件、或字段缺失），
// 调用方应回落到配置 base 判定——**不要**默认成 cn 以外的任何值。
func RegionOfCredential(host, userRegion string) (ServiceRegion, bool) {
	if r, ok := RegionOfUserRegion(userRegion); ok {
		return r, true
	}
	if r, ok := RegionOfHost(host); ok {
		return r, true
	}
	return "", false
}

// RegionGateways 一个区域的三个网关 base。
//
// 三个 base 用途不同，不能合并：
//   - Chat   对话（llm_utils_chat）与 wire 目录（get_detail_param）
//   - Remote 展示目录（/api/remote/v1/models）
//   - API    签到与用量（国内是 api.trae.cn；国际用凭证自带 host）
type RegionGateways struct {
	Chat   string
	Remote string
	API    string
}

// regionGateways 各区域的内置网关（实测值，见 docs/trae-intl.md §1）。
//
// 国际版的 API base 是**凭证 host**，不是固定值（SG 侧有 api-sg-central 与
// growsg-normal 两种），所以这里给的是缺省值，实际用 credentialHost 优先。
var regionGateways = map[ServiceRegion]RegionGateways{
	RegionCN: {
		Chat:   DefaultChatBase,
		Remote: DefaultChatBase, // 国内 remote 目录也走 mchost.guru（见 remote_models.go）
		API:    DefaultAPITraeBase,
	},
	RegionGlobal: {
		Chat:   DefaultChatBaseGlobal,
		Remote: DefaultChatBaseGlobal,
		API:    DefaultAPITraeBaseGlobal,
	},
}

// GatewaysFor 取某区域的网关（未知区域回落国内）。
func GatewaysFor(r ServiceRegion) RegionGateways {
	if g, ok := regionGateways[r]; ok {
		return g
	}
	return regionGateways[RegionCN]
}

// trimLower 去空白并转小写。
func trimLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
