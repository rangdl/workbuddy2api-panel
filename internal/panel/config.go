// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"io"
	"log"
	"net/http"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	// checkin_backends_available：账号池里**实际存在**的上游列表，供配置页渲染
	// 「各上游自动签到」勾选项。由池提供而不是读配置——配置里列着但没账号的上游
	// 列出来只是噪音；新加账号的上游即使配置没提也必须出现（缺键 = 启用）。
	var avail []string
	if p.cfg.Pool != nil {
		avail = p.cfg.Pool.Backends()
	}
	if avail == nil {
		avail = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                         true,
		"path":                       p.cfg.ConfigPath,
		"config":                     cfg,
		"checkin_backends_available": avail,
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
