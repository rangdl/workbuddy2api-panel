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
	// CheckinDone 今日是否已签（实时查询上游）。
	//
	// nil = 未知（未查询 / 查询失败），前端据此显示"签到"而不是"已签"；
	// 查询失败时 CheckinError 给出原因，避免把"查不到"当成"没签"。
	CheckinDone *bool `json:"checkin_done,omitempty"`
	// CheckinDeviceAlready 该**设备**今日已签（did_checked_in）。
	// 与 CheckinDone 的区别：后者是账号维度，前者是设备维度。多账号共用同一
	// 设备号时，只有第一个账号的 CheckinDone 会为真，其余账号在设备维度上是"已签"。
	CheckinDeviceAlready bool `json:"checkin_device_already,omitempty"`
	// CheckinError 签到状态查询失败的原因（空 = 查询成功）。
	CheckinError string `json:"checkin_error,omitempty"`
	// Stats 运行期计数（成功/失败/在途/用量/最近额度）。nil = 未注入统计表。
	Stats *trae.StatView `json:"stats,omitempty"`
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
	cfg, err := trae.Load(path, p.cfg.AuthDir)
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
	// 实时补签到状态（并发、带超时；失败只记录原因，不影响配置读取）。
	if cfg != nil && p.cfg.TraeClient != nil && r.URL.Query().Get("no_checkin") == "" {
		p.fillTraeCheckinStatus(r.Context(), cfg, view.Accounts)
	}
	// 运行期计数：进程内快照，无上游请求，直接套用。
	if p.cfg.TraeStats != nil {
		for i := range view.Accounts {
			st := p.cfg.TraeStats.Get(view.Accounts[i].UID)
			view.Accounts[i].Stats = &st
		}
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
	old, err := trae.Load(path, p.cfg.AuthDir)
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
		// 签到链路可调项：面板不编辑，但保存时必须原样带过去，
		// 否则用户手改过 trae.json 的这些字段会被一次面板保存清空。
		VerifyClaim:          old.VerifyClaim,
		RefreshBeforeCheckin: old.RefreshBeforeCheckin,
		CheckinUserAgent:     old.CheckinUserAgent,
		CheckinReqSource:     old.CheckinReqSource,
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
	// 账号本身**不写回 trae.json**（json:"-"）：这里只把「保留哪些、是否禁用」
	// 落到各自的账号文件上，删掉的账号还要移除其文件。
	for _, uid := range req.Accounts {
		a := old.AccountByUID(uid)
		if a == nil {
			continue // 未知 uid：忽略（不凭空造账号）
		}
		a.Disabled = disabled[uid]
		if !a.Disabled {
			a.DisabledReason = ""
		}
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
	// 账号文件：保留的写回（启用状态可能变了），未保留的删除。
	kept := make(map[string]bool, len(next.Accounts))
	for i := range next.Accounts {
		kept[next.Accounts[i].UID] = true
		if err := trae.SaveAccount(next.AuthDir, &next.Accounts[i]); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存账号 "+next.Accounts[i].UID+" 失败: "+err.Error())
			return
		}
	}
	for _, a := range old.Accounts {
		if kept[a.UID] {
			continue
		}
		if err := trae.RemoveAccount(old.AuthDir, a.UID); err != nil {
			log.Printf("panel: 删除 Trae 账号文件 %s 失败: %v", a.UID, err)
		}
	}
	p.invalidateCheckinCache()
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
	// uid 留空 = 按设备号自动匹配账号（只有一个可匹配项时才应用，
	// 避免把凭证写到错误账号上）；匹配不到或有多项时返回候选让人确认。
	if req.UID == "" {
		cfg0, _ := trae.Load(path, p.cfg.AuthDir)
		if cfg0 == nil {
			writeErr(w, http.StatusBadRequest, "trae.json 不存在：请先完成一次登录或手工创建配置")
			return
		}
		cred := creds[0]
		var matched []string
		for _, a := range cfg0.Accounts {
			// 同一账号（同设备号）或空白设备号的账号都可以接收这份凭证。
			if a.DeviceID == "" || a.DeviceID == cred.DeviceID {
				matched = append(matched, a.UID)
			}
		}
		ids := make([]string, 0, len(creds))
		for _, c := range creds {
			ids = append(ids, c.DeviceID)
		}
		if len(matched) != 1 {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "device_ids": ids, "matched_uids": matched,
				"message": "请填写要绑定到哪个 uid（当前有一项以上或没有可匹配的账号）",
			})
			return
		}
		req.UID = matched[0]
	}

	cfg, err := trae.Load(path, p.cfg.AuthDir)
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
	idx := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].UID == req.UID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusBadRequest, "账号 "+req.UID+" 不存在：请先用 OAuth 登录添加账号")
		return
	}
	cfg.Accounts[idx].DeviceID = cred.DeviceID
	cfg.Accounts[idx].PrivateKeyPEM = cred.PrivateKeyPEM
	if cred.PublicKeyPEM != "" {
		cfg.Accounts[idx].PublicKeyPEM = cred.PublicKeyPEM
	}
	if cred.MachineID != "" {
		cfg.Accounts[idx].MachineID = cred.MachineID
	}
	if cred.AppVersion != "" {
		cfg.Accounts[idx].AppVersion = cred.AppVersion
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 只写被改动的那个账号文件（trae.json 不含账号）。
	if err := trae.SaveAccount(cfg.AuthDir, &cfg.Accounts[idx]); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
		return
	}
	p.invalidateCheckinCache()
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
//
// 设备号可以在请求体里显式指定（`device_id` / `machine_id`）——这是登录能否
// 用于签到的关键：Trae 的每日签到限额按**设备**算，且只有客户端在服务端注册
// 过的真实设备号才能通过设备检查（生成号必然 9074）。不指定时的行为：
//   - 复用已导入凭证里**尚未被任何账号占用**的设备号（同机多账号共用一套设备
//     密钥是客户端自身的行为，但不该悄悄把新账号挂到已有账号的设备上）；
//   - 都没有才生成占位号，并在响应里明确标注 `device_generated: true`。
func (p *Panel) traeLoginStart(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.TraePath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "trae config api not available")
		return
	}
	var req struct {
		DeviceID  string `json:"device_id"`
		MachineID string `json:"machine_id"`
		// StorageJSON 可选：粘贴 Trae 客户端 storage.json 内容。
		// 解析出设备号 + 公钥（+ 私钥）后，AuthCode 兑换才能构造 DeviceInfo——
		// 没有它就只带平铺 DeviceID，上游会拒成 20405 Device proof required。
		StorageJSON string `json:"storage_json"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req)
	}
	req.DeviceID = strings.TrimSpace(req.DeviceID)
	req.MachineID = strings.TrimSpace(req.MachineID)
	if req.DeviceID != "" && !trae.IsValidDeviceID(req.DeviceID) {
		writeErr(w, http.StatusBadRequest,
			"device_id 应为一串 12~20 位十进制数字（Trae 客户端 storage.json 里 "+
				"iCubeAuthInfo://icube-dc:<数字> 键名中的数字）")
		return
	}

	// 粘贴了 storage.json：解出设备身份（公钥/私钥），并以其设备号为准。
	var cred *trae.DeviceCredential
	if strings.TrimSpace(req.StorageJSON) != "" {
		creds, err := trae.ParseStorageJSON([]byte(req.StorageJSON))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "storage.json 解析失败: "+err.Error())
			return
		}
		if len(creds) == 0 {
			writeErr(w, http.StatusBadRequest,
				"storage.json 里没有 iCubeAuthInfo://icube-dc:<数字> 条目——"+
					"请确认是在已登录 Trae 客户端的机器上复制的，且客户端版本未改变存储布局")
			return
		}
		// 多条时优先用户显式指定的设备号，否则取第一条。
		cred = creds[0]
		if req.DeviceID != "" {
			for _, c := range creds {
				if c.DeviceID == req.DeviceID {
					cred = c
					break
				}
			}
		}
		if req.DeviceID != "" && cred.DeviceID != req.DeviceID {
			writeErr(w, http.StatusBadRequest,
				"指定的 device_id 与 storage.json 中的设备号不一致（storage 里是 "+
					cred.DeviceID+"），请留空或改成一致")
			return
		}
		req.DeviceID = cred.DeviceID
		if req.MachineID == "" {
			req.MachineID = cred.MachineID
		}
	}

	cfg, _ := trae.Load(path, p.cfg.AuthDir)
	clientID, callback := "", ""
	if cfg != nil {
		clientID = cfg.OAuthClientID
		callback = cfg.OAuthCallbackURL
	}

	deviceID, machineID := req.DeviceID, req.MachineID
	deviceSource := "explicit"
	generated := false
	if deviceID == "" {
		// 复用已导入凭证的设备号，但跳过"已被某个账号占用"的（见函数注释）。
		if cfg != nil {
			used := make(map[string]bool, len(cfg.Accounts))
			for _, a := range cfg.Accounts {
				used[a.DeviceID] = true
			}
			for _, a := range cfg.Accounts {
				if a.DeviceID != "" && !used[a.DeviceID] {
					deviceID, machineID = a.DeviceID, a.MachineID
					deviceSource = "imported"
					break
				}
			}
			// 所有已导入设备号都已被占用时，仍复用第一个（同机多账号共用设备，
			// 与客户端行为一致），但要靠签到侧的 9095 提示暴露限制。
			if deviceID == "" {
				for _, a := range cfg.Accounts {
					if a.DeviceID != "" {
						deviceID, machineID = a.DeviceID, a.MachineID
						deviceSource = "shared"
						break
					}
				}
			}
		}
	}
	if deviceID == "" {
		// 占位号：登录本身可用，但用它签到必然 9074。
		var err error
		if deviceID, err = randomDigits(16); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		deviceSource = "generated"
		generated = true
	}
	if machineID == "" {
		var err error
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
	// 把设备身份带进会话，finish 阶段直接用来构造 DeviceInfo（免再贴一次）。
	if cred != nil {
		sess.PublicKeyPEM = cred.PublicKeyPEM
		sess.PrivateKeyPEM = cred.PrivateKeyPEM
		sess.MachineID = orDefaultStr(machineID, cred.MachineID)
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

	if cred != nil {
		deviceSource = "storage"
	}
	resp := map[string]any{
		"ok": true, "url": url, "state": sess.State,
		"callback_url":     orDefaultStr(callback, trae.DefaultRedirectURI),
		"device_id":        deviceID,
		"device_source":    deviceSource,
		"device_generated": generated,
		"has_device_key":   cred != nil && cred.PublicKeyPEM != "",
	}
	if generated {
		resp["warning"] = "设备号是自动生成的占位值，用它签到会被拒（9074）。" +
			"请从装了 Trae 客户端的机器上取真实设备号后重新登录。"
	}
	if deviceSource == "shared" {
		resp["warning"] = "该设备号已被其它账号使用：签到限额按设备算，一天只有第一个账号能签成。"
	}
	writeJSON(w, http.StatusOK, resp)
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
		// DeviceID / MachineID 可选：不带时沿用发起登录时登记的设备号。
		// 带了则以它为准（用于「生成号 → 真实号」的修正场景）。
		DeviceID  string `json:"device_id"`
		MachineID string `json:"machine_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if v := strings.TrimSpace(req.DeviceID); v != "" && !trae.IsValidDeviceID(v) {
		writeErr(w, http.StatusBadRequest,
			"device_id 应为一串 12~20 位十进制数字（Trae 客户端 storage.json 里 "+
				"iCubeAuthInfo://icube-dc:<数字> 键名中的数字）")
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

	cfg, _ := trae.Load(path, p.cfg.AuthDir)
	if cfg == nil {
		cfg = &trae.Config{}
	}
	// 设备身份来源优先级：请求体覆盖 > 登录时粘贴的 storage.json > 会话登记值。
	devID, machID := sess.DeviceID, sess.MachineID
	if v := strings.TrimSpace(req.DeviceID); v != "" {
		devID = v
	}
	if v := strings.TrimSpace(req.MachineID); v != "" {
		machID = v
	}
	cred := &trae.DeviceCredential{
		DeviceID:      devID,
		MachineID:     machID,
		PublicKeyPEM:  sess.PublicKeyPEM,
		PrivateKeyPEM: sess.PrivateKeyPEM,
	}
	// 会话里没带公钥时，退回复用同设备账号已导入的凭证（设备身份须一致）。
	if cred.PublicKeyPEM == "" && cred.PrivateKeyPEM == "" {
		for _, a := range cfg.Accounts {
			if a.DeviceID == devID && a.PrivateKeyPEM != "" {
				cred.PrivateKeyPEM = a.PrivateKeyPEM
				break
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	pair, err := p.cfg.TraeClient.ExchangeAuthCode(ctx, cfg, authCode, sess.PKCEVerifier, cred, devID, machID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "换取 token 失败: "+err.Error())
		return
	}

	uid := trae.JWTUserID(pair.AccessToken)
	if uid == "" {
		uid = "trae-" + tail(pair.AccessToken, 8)
	}
	// 合并到配置：同 uid 覆盖 token，新 uid 追加。
	idx := -1
	for i := range cfg.Accounts {
		if cfg.Accounts[i].UID == uid {
			idx = i
			break
		}
	}
	if idx < 0 {
		cfg.Accounts = append(cfg.Accounts, trae.Account{
			UID:           uid,
			Name:          "trae-" + uid,
			AccessToken:   pair.AccessToken,
			RefreshToken:  pair.RefreshToken,
			ExpiresAt:     pair.ExpiresAt,
			DeviceID:      devID,
			MachineID:     machID,
			PrivateKeyPEM: cred.PrivateKeyPEM,
			PublicKeyPEM:  cred.PublicKeyPEM,
			AppVersion:    trae.DefaultPageAppVersion,
		})
		idx = len(cfg.Accounts) - 1
	} else {
		cfg.Accounts[idx].AccessToken = pair.AccessToken
		if pair.RefreshToken != "" {
			cfg.Accounts[idx].RefreshToken = pair.RefreshToken
		}
		cfg.Accounts[idx].ExpiresAt = pair.ExpiresAt
		cfg.Accounts[idx].Disabled = false
		cfg.Accounts[idx].DisabledReason = ""
		// 设备身份跟着本次登录走：允许把"生成号"换成真实号（签到只认真实号）。
		cfg.Accounts[idx].DeviceID = devID
		cfg.Accounts[idx].MachineID = machID
		if cred.PrivateKeyPEM != "" {
			cfg.Accounts[idx].PrivateKeyPEM = cred.PrivateKeyPEM
		}
		if cred.PublicKeyPEM != "" {
			cfg.Accounts[idx].PublicKeyPEM = cred.PublicKeyPEM
		}
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusInternalServerError, "配置校验失败: "+err.Error())
		return
	}
	// 只写这一个账号文件；trae.json 的全局配置不需要改（账号不在里面）。
	if err := trae.SaveAccount(cfg.AuthDir, &cfg.Accounts[idx]); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存账号失败: "+err.Error())
		return
	}
	p.invalidateCheckinCache()
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
	cfg, err := trae.Load(path, p.cfg.AuthDir)
	if err != nil || cfg == nil {
		writeErr(w, http.StatusBadRequest, "trae.json 不存在或无法读取")
		return
	}
	if cfg.AccountByUID(uid) == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	// 删除账号文件本身（trae.json 不含账号，无需改写）。
	if err := trae.RemoveAccount(cfg.AuthDir, uid); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除账号文件失败: "+err.Error())
		return
	}
	p.invalidateCheckinCache()
	p.reloadTrae()
	if p.cfg.TraeCooldowns != nil {
		p.cfg.TraeCooldowns.Clear(uid)
	}
	// 运行期计数一并清掉：账号已删，残留条目只会在同 uid 重新登录时
	// 显示上一轮的历史数字，反而误导。
	if p.cfg.TraeStats != nil {
		p.cfg.TraeStats.Reset(uid)
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

// writeTraeConfig 原子落盘全局配置（tmp + rename），权限 0600。
//
// 账号**不在**这里：Config.Accounts 的 json tag 是 "-"，一账号一文件由
// trae.SaveAccount 负责（见 internal/trae/accounts.go）。
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

// traeCheckinStatusTimeout 面板查询逐账号签到状态的总超时。
//
// 这是**面板轮询路径**上的上游请求，不能让它拖慢页面：超时后账号的
// CheckinDone 保持 nil（前端显示"签到"），并在 CheckinError 说明原因。
const traeCheckinStatusTimeout = 8 * time.Second

// traeCheckinStatusTTL 签到状态缓存时长。
//
// 切视图 / 点按钮都会重新拉 trae_config，每次都打上游会白白消耗风控额度。
// 30 秒内的结果直接复用；签到成功与手动刷新会显式失效缓存。
const traeCheckinStatusTTL = 30 * time.Second

// traeCheckinCache 逐账号签到状态缓存（uid → 结果）。
type traeCheckinCache struct {
	mu   sync.Mutex
	at   time.Time
	data map[string]traeCheckinSnapshot
}

type traeCheckinSnapshot struct {
	done          *bool
	deviceAlready bool
	err           string
}

// invalidateCheckinCache 让下次读取重新查询上游（签到成功 / 手动刷新时调用）。
func (p *Panel) invalidateCheckinCache() {
	p.traeCheckin.mu.Lock()
	p.traeCheckin.data = nil
	p.traeCheckin.at = time.Time{}
	p.traeCheckin.mu.Unlock()
}

// fillTraeCheckinStatus 并发查询每个账号的"今日是否已签"，写回 view。
//
// 账号多时并发上限 4，避免一次面板刷新打出十几个上游请求。
func (p *Panel) fillTraeCheckinStatus(ctx context.Context, cfg *trae.Config, views []traeAccountView) {
	targets := make([]int, 0, len(views))
	for i := range views {
		if !views[i].Disabled && views[i].HasToken && views[i].DeviceID != "" {
			targets = append(targets, i)
		}
	}
	if len(targets) == 0 {
		return
	}

	// 先看缓存：命中且未过 TTL 就直接套用。
	p.traeCheckin.mu.Lock()
	cacheFresh := p.traeCheckin.data != nil && time.Since(p.traeCheckin.at) < traeCheckinStatusTTL
	cached := p.traeCheckin.data
	p.traeCheckin.mu.Unlock()
	if cacheFresh {
		hit := true
		for _, i := range targets {
			s, ok := cached[views[i].UID]
			if !ok {
				hit = false
				break
			}
			views[i].CheckinDone = s.done
			views[i].CheckinDeviceAlready = s.deviceAlready
			views[i].CheckinError = s.err
		}
		if hit {
			return
		}
	}

	ctx, cancel := context.WithTimeout(ctx, traeCheckinStatusTimeout)
	defer cancel()

	results := make(map[string]traeCheckinSnapshot, len(targets))
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, idx := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			acc := cfg.AccountByUID(views[i].UID)
			if acc == nil {
				return
			}
			st, err := p.cfg.TraeClient.CheckinStatus(ctx, cfg, acc)
			snap := traeCheckinSnapshot{}
			if err != nil {
				snap.err = err.Error()
			} else {
				snap.done = st.CheckedIn
				snap.deviceAlready = st.DeviceAlready
			}
			mu.Lock()
			results[views[i].UID] = snap
			mu.Unlock()
		}(idx)
	}
	wg.Wait()

	// 回写视图 + 更新缓存（只缓存本轮实际查到的账号）。
	p.traeCheckin.mu.Lock()
	if p.traeCheckin.data == nil {
		p.traeCheckin.data = make(map[string]traeCheckinSnapshot, len(results))
	}
	for uid, s := range results {
		p.traeCheckin.data[uid] = s
	}
	p.traeCheckin.at = time.Now()
	p.traeCheckin.mu.Unlock()

	for _, i := range targets {
		s, ok := results[views[i].UID]
		if !ok {
			continue
		}
		views[i].CheckinDone = s.done
		views[i].CheckinDeviceAlready = s.deviceAlready
		views[i].CheckinError = s.err
	}
}
