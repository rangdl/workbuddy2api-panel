// trae_config.go 面板「上游接入」页的 Trae 配置接口。
//
// 安全约定：**面板永不回显凭证**。GET 只返回"是否已配置"与脱敏尾号；
// 凭证的写入路径只有两条——OAuth 登录换取的 token，以及用户主动粘贴的
// 设备密文（storage.json 条目，服务端解密后只取私钥，不回显）。
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

// traeLoginTTL 登录会话有效期（超过则视为过期，需重新生成授权链接）。
const traeLoginTTL = 15 * time.Minute

// traeLoginSessions 进行中的登录会话（state → 会话）。面板进程常驻，容量天然有界。
var (
	traeLoginMu       sync.Mutex
	traeLoginSessions = map[string]*trae.LoginSession{}
)

// traeAccountView 账号的脱敏视图（凭证只透出"是否已配置"+尾号）。
type traeAccountView struct {
	UID        string `json:"uid"`
	Name       string `json:"name"`
	DeviceID   string `json:"device_id"`
	MachineID  string `json:"machine_id"`
	Disabled   bool   `json:"disabled"`
	AppVersion string `json:"app_version"`
	// HasToken / HasRefresh / HasDeviceKey 凭证是否已配置。
	HasToken     bool `json:"has_token"`
	HasRefresh   bool `json:"has_refresh"`
	HasDeviceKey bool `json:"has_device_key"`
	// TokenTail token 尾 4 位（便于用户核对是哪一份凭证，不足以还原）。
	TokenTail string `json:"token_tail"`
	// ExpiresAt token 过期时间（Unix 秒；0 = 未知）。
	ExpiresAt int64 `json:"expires_at"`
}

// traeScheduleView 排程视图（与 trae.CheckinSchedule 同名字段）。
type traeScheduleView struct {
	Enabled bool  `json:"checkin_enabled"`
	Hours   []int `json:"checkin_hours"`
}

// traeConfigView GET 响应体。
type traeConfigView struct {
	Enabled        bool              `json:"enabled"`
	ChatBase       string            `json:"chat_base"`
	IDEVersion     string            `json:"ide_version"`
	OAuthCallback  string            `json:"oauth_callback_url"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Accounts       []traeAccountView `json:"accounts"`
	// Schedule 签到排程。
	Schedule traeScheduleView `json:"schedule"`
	// Cooldowns 冷却中的账号（来自运行期 handler 的快照）。
	Cooldowns []trae.CoolInfo `json:"cooldowns"`
	// Usable 当前可参与轮转的账号数（扣掉 disabled 与冷却中）。
	Usable int `json:"usable"`
}

// getTraeConfig 读取 trae.json 并返回脱敏视图（文件不存在时返回默认值）。
func (p *Panel) getTraeConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	cfg, err := trae.Load(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read trae.json: "+err.Error())
		return
	}
	view := traeConfigView{
		Enabled:        false,
		ChatBase:       trae.DefaultChatBase,
		IDEVersion:     trae.DefaultIDEVersion,
		OAuthCallback:  trae.DefaultRedirectURI,
		TimeoutSeconds: trae.DefaultTimeoutSeconds,
		Accounts:       []traeAccountView{},
	}
	if cfg != nil {
		view.Enabled = cfg.Enabled
		view.ChatBase = cfg.ChatBase
		view.IDEVersion = cfg.IDEVersion
		view.TimeoutSeconds = cfg.TimeoutSeconds
		view.Schedule = traeScheduleView{
			Enabled: cfg.Schedule.Enabled,
			Hours:   cfg.Schedule.Hours,
		}
		if cfg.OAuthCallbackURL != "" {
			view.OAuthCallback = cfg.OAuthCallbackURL
		}
		for _, a := range cfg.Accounts {
			view.Accounts = append(view.Accounts, traeAccountView{
				UID:          a.UID,
				Name:         a.Name,
				DeviceID:     a.DeviceID,
				MachineID:    a.MachineID,
				Disabled:     a.Disabled,
				AppVersion:   a.AppVersion,
				HasToken:     a.AccessToken != "",
				HasRefresh:   a.RefreshToken != "",
				HasDeviceKey: a.PrivateKeyPEM != "",
				TokenTail:    tail(a.AccessToken, 4),
				ExpiresAt:    a.ExpiresAt,
			})
		}
	}
	if p.cfg.TraeCooldowns != nil {
		view.Cooldowns = p.cfg.TraeCooldowns.Snapshot()
	}
	for _, a := range view.Accounts {
		if a.Disabled || !a.HasToken {
			continue
		}
		if p.cfg.TraeCooldowns != nil && !p.cfg.TraeCooldowns.Available(a.UID) {
			continue
		}
		view.Usable++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "config": view})
}

// traeSaveRequest 保存请求（只含非敏感字段；凭证不在本接口内传递）。
type traeSaveRequest struct {
	Enabled        bool             `json:"enabled"`
	ChatBase       string           `json:"chat_base"`
	IDEVersion     string           `json:"ide_version"`
	TimeoutSeconds int              `json:"timeout_seconds"`
	Accounts       []string         `json:"accounts"` // 保留的 uid 列表（顺序即轮转顺序）
	Disabled       []string         `json:"disabled"` // 其中被禁用的 uid
	Schedule       traeScheduleView `json:"schedule"`
}

// saveTraeConfig 保存非敏感字段：读-改-写，**保留已有账号的凭证字段**。
//
// 之所以按 uid 列表而非完整账号对象提交：面板不回显凭证，也就无法原样回传；
// 用 uid 列表表达"保留哪些账号、顺序、启用状态"，凭证由服务端从旧文件带过来。
func (p *Panel) saveTraeConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req traeSaveRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	old, err := trae.Load(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read trae.json: "+err.Error())
		return
	}
	if old == nil {
		old = &trae.Config{}
	}
	disabled := make(map[string]bool, len(req.Disabled))
	for _, uid := range req.Disabled {
		disabled[uid] = true
	}
	next := &trae.Config{
		Enabled:           req.Enabled,
		ChatBase:          req.ChatBase,
		IDEVersion:        old.IDEVersion,
		IDEVersionCode:    old.IDEVersionCode,
		AppID:             old.AppID,
		OAuthClientID:     old.OAuthClientID,
		OAuthClientSecret: old.OAuthClientSecret,
		OAuthHost:         old.OAuthHost,
		OAuthCallbackURL:  old.OAuthCallbackURL,
		TimeoutSeconds:    req.TimeoutSeconds,
		AuthDir:           old.AuthDir,
		APITraeBase:       old.APITraeBase,
		Schedule: trae.CheckinSchedule{
			Enabled: req.Schedule.Enabled,
			Hours:   req.Schedule.Hours,
			GapMS:   old.Schedule.GapMS,
		},
	}
	if req.IDEVersion != "" {
		next.IDEVersion = req.IDEVersion
	}
	// 按提交的 uid 顺序重建账号列表，凭证从旧配置带入。
	for _, uid := range req.Accounts {
		a := old.AccountByUID(uid)
		if a == nil {
			continue // 未知 uid：忽略（不凭空造账号）
		}
		a.Disabled = disabled[uid]
		next.Accounts = append(next.Accounts, *a)
	}
	if err := next.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := writeTraeConfig(path, next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.reloadTrae()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": []string{}})
}

// traeImportRequest 设备凭证导入请求（两种形态二选一）。
type traeImportRequest struct {
	// UID 目标账号；为空则只解析不写入（供前端预览）。
	UID string `json:"uid"`
	// StorageJSON 整个客户端 storage.json 内容。
	StorageJSON string `json:"storage_json"`
	// DeviceID + ICubeValue 单条形态（从 storage.json 里复制某一个键的值）。
	DeviceID   string `json:"device_id"`
	ICubeValue string `json:"icube_value"`
}

// importTraeDevice 解密并导入设备凭证（tc 信封 → EC 私钥），写入目标账号。
//
// 私钥只在内存流转：解析结果不回显，只回报"成功/失败 + 设备 id"。
func (p *Panel) importTraeDevice(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req traeImportRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	creds, err := parseImportCredentials(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(creds) == 0 {
		writeErr(w, http.StatusBadRequest, "未在输入中找到 iCubeAuthInfo 设备凭证条目")
		return
	}
	// 预览模式：只回报解析到的设备 id（不含私钥）。
	if req.UID == "" {
		ids := make([]string, 0, len(creds))
		for _, c := range creds {
			ids = append(ids, c.DeviceID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "device_ids": ids})
		return
	}

	cfg, err := trae.Load(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read trae.json: "+err.Error())
		return
	}
	if cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在：请先完成一次登录或手工创建配置")
		return
	}
	target := cfg.AccountByUID(req.UID)
	if target == nil {
		writeErr(w, http.StatusBadRequest, "账号 "+req.UID+" 不存在：请先用 OAuth 登录添加账号")
		return
	}
	cred := creds[0]
	for i := range cfg.Accounts {
		if cfg.Accounts[i].UID != req.UID {
			continue
		}
		cfg.Accounts[i].DeviceID = cred.DeviceID
		cfg.Accounts[i].PrivateKeyPEM = cred.PrivateKeyPEM
		if cred.MachineID != "" {
			cfg.Accounts[i].MachineID = cred.MachineID
		}
		if cred.AppVersion != "" {
			cfg.Accounts[i].AppVersion = cred.AppVersion
		}
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := writeTraeConfig(path, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.reloadTrae()
	if p.cfg.TraeCooldowns != nil {
		p.cfg.TraeCooldowns.Clear(req.UID) // 补齐设备凭证后解除"设备问题"冷却
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": req.UID, "device_id": cred.DeviceID,
		"message": "设备凭证已导入（含签名私钥），该账号现具备 DeviceProof 能力",
	})
}

// parseImportCredentials 从请求解析设备凭证（整文件优先，其次单条）。
func parseImportCredentials(req traeImportRequest) ([]*trae.DeviceCredential, error) {
	if strings.TrimSpace(req.StorageJSON) != "" {
		return trae.ParseStorageJSON([]byte(req.StorageJSON))
	}
	if strings.TrimSpace(req.ICubeValue) == "" {
		return nil, fmt.Errorf("请提供 storage_json 或 device_id + icube_value")
	}
	cred, err := trae.DecryptICubeValue(req.DeviceID, req.ICubeValue)
	if err != nil {
		return nil, fmt.Errorf("设备凭证解密失败（pepper 表可能已随客户端版本变化）: %w", err)
	}
	return []*trae.DeviceCredential{cred}, nil
}

// traeLoginStart 生成授权 URL（PKCE + CSRF state）并登记登录会话。
func (p *Panel) traeLoginStart(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	cfg, _ := trae.Load(path)
	clientID, callback := "", ""
	if cfg != nil {
		clientID = cfg.OAuthClientID
		callback = cfg.OAuthCallbackURL
	}
	// 设备标识：优先复用已导入凭证的设备 id（与签名私钥同源，否则上游 20403/20405）。
	deviceID, machineID := "", ""
	if cfg != nil {
		for _, a := range cfg.Accounts {
			if a.DeviceID != "" {
				deviceID, machineID = a.DeviceID, a.MachineID
				break
			}
		}
	}
	if deviceID == "" {
		// 无既有设备：生成一对稳定标识（登录后可再导入真实设备凭证覆盖）。
		var err error
		if deviceID, err = randomDigits(15); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if machineID, err = trae.RandomHex(32); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	url, sess, err := trae.BuildLoginURL(trae.LoginURLOptions{
		ClientID:    clientID,
		CallbackURL: callback,
		DeviceID:    deviceID,
		MachineID:   machineID,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	traeLoginMu.Lock()
	traeLoginSessions[sess.State] = sess
	// 顺带清理过期会话（数量有界，惰性清理即可）。
	for k, v := range traeLoginSessions {
		if time.Since(v.CreatedAt) > traeLoginTTL {
			delete(traeLoginSessions, k)
		}
	}
	traeLoginMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "url": url, "state": sess.State,
		"callback_url": orDefaultStr(callback, trae.DefaultRedirectURI),
		"device_id":    deviceID,
	})
}

// traeLoginFinish 用回调 URL 完成登录（换取 token 并落账号）。
func (p *Panel) traeLoginFinish(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" || p.cfg.TraeClient == nil {
		writeErr(w, http.StatusNotImplemented, "trae login api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	authCode, traceID, err := trae.ParseAuthCallback(req.CallbackURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := takeTraeLogin(traceID)
	if sess == nil {
		writeErr(w, http.StatusBadRequest,
			"回调 URL 中的 loginTraceID 与本机登录会话不匹配或已过期（请重新点击「生成授权链接」）")
		return
	}

	cfg, _ := trae.Load(path)
	if cfg == nil {
		cfg = &trae.Config{}
	}
	cred := &trae.DeviceCredential{DeviceID: sess.DeviceID, MachineID: sess.MachineID}
	// 已有同设备账号的私钥时复用它（设备身份一致才不会被拒）。
	for _, a := range cfg.Accounts {
		if a.DeviceID == sess.DeviceID && a.PrivateKeyPEM != "" {
			cred.PrivateKeyPEM = a.PrivateKeyPEM
			break
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	pair, err := p.cfg.TraeClient.ExchangeAuthCode(ctx, cfg, authCode, sess.PKCEVerifier, cred, sess.DeviceID, sess.MachineID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "换取 token 失败: "+err.Error())
		return
	}

	uid := trae.JWTUserID(pair.AccessToken)
	if uid == "" {
		uid = "trae-" + tail(pair.AccessToken, 8)
	}
	// 合并到配置：同 uid 覆盖 token，新 uid 追加。
	merged := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].UID != uid {
			continue
		}
		cfg.Accounts[i].AccessToken = pair.AccessToken
		if pair.RefreshToken != "" {
			cfg.Accounts[i].RefreshToken = pair.RefreshToken
		}
		cfg.Accounts[i].ExpiresAt = pair.ExpiresAt
		cfg.Accounts[i].Disabled = false
		cfg.Accounts[i].DisabledReason = ""
		merged = true
	}
	if !merged {
		cfg.Accounts = append(cfg.Accounts, trae.Account{
			UID:           uid,
			Name:          "trae-" + uid,
			AccessToken:   pair.AccessToken,
			RefreshToken:  pair.RefreshToken,
			ExpiresAt:     pair.ExpiresAt,
			DeviceID:      sess.DeviceID,
			MachineID:     sess.MachineID,
			PrivateKeyPEM: cred.PrivateKeyPEM,
			AppVersion:    trae.DefaultPageAppVersion,
		})
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusInternalServerError, "配置校验失败: "+err.Error())
		return
	}
	if err := writeTraeConfig(path, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.reloadTrae()
	if p.cfg.TraeCooldowns != nil {
		p.cfg.TraeCooldowns.Clear(uid)
	}
	log.Printf("panel: Trae 登录成功 uid=%s（设备 %s）", uid, sess.DeviceID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "has_refresh": pair.RefreshToken != "",
		"has_device_key": cred.PrivateKeyPEM != "",
	})
}

// removeTraeAccount 移除账号（按 uid）。
func (p *Panel) removeTraeAccount(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	uid := r.PathValue("uid")
	cfg, err := trae.Load(path)
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在或无法读取")
		return
	}
	kept := cfg.Accounts[:0]
	removed := false
	for _, a := range cfg.Accounts {
		if a.UID == uid {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	if !removed {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	cfg.Accounts = kept
	if err := writeTraeConfig(path, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.reloadTrae()
	if p.cfg.TraeCooldowns != nil {
		p.cfg.TraeCooldowns.Clear(uid)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// takeTraeLogin 取出并删除登录会话（一次性）。
// traceID 为空时（授权页未回传）退化为"取唯一未过期会话"，兼顾兼容性。
func takeTraeLogin(traceID string) *trae.LoginSession {
	traeLoginMu.Lock()
	defer traeLoginMu.Unlock()
	if traceID != "" {
		s := traeLoginSessions[traceID]
		delete(traeLoginSessions, traceID)
		if s != nil && time.Since(s.CreatedAt) <= traeLoginTTL {
			return s
		}
		return nil
	}
	var found *trae.LoginSession
	for k, v := range traeLoginSessions {
		if time.Since(v.CreatedAt) > traeLoginTTL {
			delete(traeLoginSessions, k)
			continue
		}
		if found != nil {
			return nil // 多个未过期会话且无 traceID：无法确定归属，拒绝
		}
		found = v
		delete(traeLoginSessions, k)
	}
	return found
}

// writeTraeConfig 原子落盘（tmp + rename），权限 0600（含凭证）。
func writeTraeConfig(path string, cfg *trae.Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("原子替换失败: %w", err)
	}
	return nil
}

// reloadTrae 触发热重载（未注入钩子时静默——配置已落盘，重启即生效）。
func (p *Panel) reloadTrae() {
	if p.cfg.ReloadTrae == nil {
		return
	}
	if err := p.cfg.ReloadTrae(); err != nil {
		log.Printf("panel: trae.json 热重载失败: %v", err)
	}
}

// tail 取字符串末 n 位（脱敏展示用）。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// randomDigits 生成 n 位数字串（Trae 的 device_id 形态）。
func randomDigits(n int) (string, error) {
	const digits = "0123456789"
	buf := make([]byte, n)
	for i := range buf {
		r, err := trae.RandomHex(1)
		if err != nil {
			return "", err
		}
		buf[i] = digits[int(r[0])%10]
	}
	return string(buf), nil
}

// orDefaultStr 空串回落默认值。
func orDefaultStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
