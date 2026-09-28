// thirdparty_config.go 面板「上游接入」页的第三方上游接口：
// 读写独立的 third_party.json（与 config.json 同目录），不并入主配置表单。
//
// 之所以独立成页 + 独立文件：第三方上游是本 fork 的增量能力，独立文件与独立接口
// 可降低与上游 config.go / 配置页的合并冲突（与 responses_config.go 同一取舍）。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/thirdparty"
)

// probeTimeout 面板「测试连接」的单次超时（探活是同步阻塞调用，必须有硬上限）。
const probeTimeout = 30 * time.Second

// defaultThirdPartyConfig third_party.json 不存在时回显的默认值。
// enabled 默认 false：第三方上游需要用户显式填 base_url/api_key，默认关闭更安全
// （也不会因为误配把流量打到未知上游）。
func defaultThirdPartyConfig() map[string]any {
	return map[string]any{
		"enabled":   false,
		"providers": []any{},
	}
}

// getThirdPartyConfig 读取 third_party.json（不存在时返回默认值，不报错）。
func (p *Panel) getThirdPartyConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.ThirdPartyPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "third-party config api not available")
		return
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "path": path, "config": defaultThirdPartyConfig(),
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read third_party.json: "+err.Error())
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "parse third_party.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "config": cfg})
}

// saveThirdPartyConfig 校验并写入 third_party.json，随后触发热重载。
func (p *Panel) saveThirdPartyConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.ThirdPartyPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "third-party config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// 校验用结构化解码（同一套规则见 thirdparty.Config.Validate），
	// 写盘用原始 body（保留用户手写的未知字段与字段顺序）。
	var cfg thirdparty.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		b, merr := json.MarshalIndent(cfg, "", "  ")
		if merr != nil {
			writeErr(w, http.StatusInternalServerError, "marshal: "+merr.Error())
			return
		}
		pretty.Reset()
		pretty.Write(b)
	}
	pretty.WriteByte('\n')
	if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "write third_party.json: "+err.Error())
		return
	}
	restartRequired := []string{}
	if p.cfg.ReloadThirdParty != nil {
		if err := p.cfg.ReloadThirdParty(); err != nil {
			log.Printf("panel: third_party.json 热重载失败: %v", err)
			restartRequired = []string{"thirdparty"}
		} else {
			log.Printf("panel: third_party.json 已保存并热重载（%s）", path)
		}
	} else {
		restartRequired = []string{"thirdparty"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
}

// testThirdPartyProvider 对单个 provider 做连通性探活。
//
// 请求体传**完整 provider 对象**而非名字：这样用户可以在保存前先测未落盘的配置
// （改名/换 base_url 后立即验证），不必先写盘再测。
func (p *Panel) testThirdPartyProvider(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ThirdPartyClient == nil {
		writeErr(w, http.StatusNotImplemented, "third-party client not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req struct {
		Provider thirdparty.Provider `json:"provider"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// 借 Config.Validate 完成 provider 的缺省补齐与校验（单一校验口径）。
	probeCfg := &thirdparty.Config{Enabled: true, Providers: []thirdparty.Provider{req.Provider}}
	if err := probeCfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	msg, err := p.cfg.ThirdPartyClient.Probe(ctx, &probeCfg.Providers[0])
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}
