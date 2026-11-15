// codebuddy_config.go 面板「配置 → WorkBuddy 上游」区块的读写接口。
//
// 与 trae_config.go 同构：上游配置各自独立成文件，面板按文件分区展示，
// 归属不会看错。这里是 CodeBuddy 那一份。
package panel

import (
	"encoding/json"
	"io"
	"net/http"

	backendcb "github.com/linguo2625469/workbuddy2api-panel/internal/backend/codebuddy"
)

// codeBuddyConfigView GET 响应体。
//
// 只暴露**用户在配置页能改**的字段。内置常量（默认 base、默认版本号）不返回：
// 它们不是配置项，返回了反而让人以为可以改。
type codeBuddyConfigView struct {
	CN     codeBuddyRealmView `json:"cn"`
	Global codeBuddyGlobal    `json:"global"`

	UserAgent     string `json:"user_agent"`
	ClientVersion string `json:"client_version"`
	CliVersion    string `json:"cli_version"`
	ClientName    string `json:"client_name"`
	PassthroughIP bool   `json:"passthrough_ip"`

	// SanitizeBlacklistFingerprints 上游专有的指纹脱敏开关。
	// 放在本文件（而不是主配置）正是「上游专有配置独立成文件」的体现。
	SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
}

type codeBuddyRealmView struct {
	ChatBase    string `json:"chat_base"`
	BillingBase string `json:"billing_base"`
}

type codeBuddyGlobal struct {
	codeBuddyRealmView
	Enabled bool `json:"enabled"`
}

// getCodeBuddyConfig 回显当前配置（文件缺失时返回内置默认）。
func (p *Panel) getCodeBuddyConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CodeBuddyPath == "" {
		writeErr(w, http.StatusNotImplemented, "codebuddy config api not available")
		return
	}
	cfg, err := backendcb.Load(p.cfg.CodeBuddyPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read codebuddy.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"path": p.cfg.CodeBuddyPath,
		"config": codeBuddyConfigView{
			CN: codeBuddyRealmView{
				ChatBase:    cfg.CN.ChatBase,
				BillingBase: cfg.CN.BillingBase,
			},
			Global: codeBuddyGlobal{
				codeBuddyRealmView: codeBuddyRealmView{
					ChatBase:    cfg.Global.ChatBase,
					BillingBase: cfg.Global.BillingBase,
				},
				Enabled: cfg.Global.Enabled,
			},
			UserAgent:                     cfg.UserAgent,
			ClientVersion:                 cfg.ClientVersion,
			CliVersion:                    cfg.CliVersion,
			ClientName:                    cfg.ClientName,
			PassthroughIP:                 cfg.PassthroughIP,
			SanitizeBlacklistFingerprints: cfg.SanitizeBlacklistFingerprints,
		},
	})
}

// codeBuddySaveRequest 保存请求。
//
// 全部字段用指针：**没提交 = 不改**。上游配置里有几个"空串有意义"的字段
// （user_agent 空 = 用内置默认），用值类型就分不清「没提交」和「提交了空串」。
type codeBuddySaveRequest struct {
	CN     *codeBuddyRealmView `json:"cn"`
	Global *struct {
		ChatBase    *string `json:"chat_base"`
		BillingBase *string `json:"billing_base"`
		Enabled     *bool   `json:"enabled"`
	} `json:"global"`

	UserAgent                     *string `json:"user_agent"`
	ClientVersion                 *string `json:"client_version"`
	CliVersion                    *string `json:"cli_version"`
	ClientName                    *string `json:"client_name"`
	PassthroughIP                 *bool   `json:"passthrough_ip"`
	SanitizeBlacklistFingerprints *bool   `json:"sanitize_blacklist_fingerprints"`
}

// saveCodeBuddyConfig 保存配置（校验后原子落盘）。
//
// 不碰账号：账号文件的生命周期只由账号池接口管理。
func (p *Panel) saveCodeBuddyConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CodeBuddyPath == "" {
		writeErr(w, http.StatusNotImplemented, "codebuddy config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req codeBuddySaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// 以磁盘当前值为基线：只覆盖请求里出现的字段，其余保持原样。
	cfg, err := backendcb.Load(p.cfg.CodeBuddyPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read codebuddy.json: "+err.Error())
		return
	}
	if req.CN != nil {
		cfg.CN.ChatBase = req.CN.ChatBase
		cfg.CN.BillingBase = req.CN.BillingBase
	}
	if req.Global != nil {
		if req.Global.ChatBase != nil {
			cfg.Global.ChatBase = *req.Global.ChatBase
		}
		if req.Global.BillingBase != nil {
			cfg.Global.BillingBase = *req.Global.BillingBase
		}
		if req.Global.Enabled != nil {
			cfg.Global.Enabled = *req.Global.Enabled
		}
	}
	if req.UserAgent != nil {
		cfg.UserAgent = *req.UserAgent
	}
	if req.ClientVersion != nil {
		cfg.ClientVersion = *req.ClientVersion
	}
	if req.CliVersion != nil {
		cfg.CliVersion = *req.CliVersion
	}
	if req.ClientName != nil {
		cfg.ClientName = *req.ClientName
	}
	if req.PassthroughIP != nil {
		cfg.PassthroughIP = *req.PassthroughIP
	}
	if req.SanitizeBlacklistFingerprints != nil {
		cfg.SanitizeBlacklistFingerprints = *req.SanitizeBlacklistFingerprints
	}

	if err := backendcb.Save(p.cfg.CodeBuddyPath, cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// 这些字段在装配期写进出站 client，改完要重启才生效。
		"restart_required": []string{
			"cn.chat_base", "global.chat_base",
		},
	})
}
