// responses_config.go 面板「Responses / Codex 接入」配置页接口：
// 读写独立的 responses.json（与 config.json 同目录），不并入主配置表单。
//
// 之所以独立：responses.json 是本 fork 的增量能力（Codex Responses 协议接入），
// 独立文件 + 独立接口可降低与上游 config.go / 配置页的合并冲突。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// codexDefaultModels 面板 model_map 预填的 codex 客户端模型名（左列）。
// 来源：codex 的 model catalog（slug 字段，含 gpt-5.x 系列）；网关仅作展示默认，
// 用户可增删。codex 升级后模型可能变化，可在面板里手动增行。
var codexDefaultModels = []string{
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.2",
}

// defaultResponsesConfig responses.json 不存在时回显的默认值（与 loadResponsesConfig 语义一致）。
func defaultResponsesConfig() map[string]any {
	return map[string]any{
		"enabled":              true,
		"model_map":            map[string]string{},
		"default_model":        "",
		"max_cached_responses": 512,
	}
}

// getResponsesConfig 读取 responses.json（不存在时返回默认值，不报错）。
func (p *Panel) getResponsesConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.ResponsesPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "responses config api not available")
		return
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "path": path, "config": defaultResponsesConfig(), "codex_models": codexDefaultModels,
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read responses.json: "+err.Error())
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "parse responses.json: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "path": path, "config": cfg, "codex_models": codexDefaultModels,
	})
}

// saveResponsesConfig 校验并写入 responses.json。
// ResponsesConfig 在进程启动时构造，故保存后需重启进程生效。
func (p *Panel) saveResponsesConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.ResponsesPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "responses config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := validateResponsesConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pretty, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal: "+err.Error())
		return
	}
	pretty = append(pretty, '\n')
	if err := os.WriteFile(path, pretty, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "write responses.json: "+err.Error())
		return
	}
	restartRequired := []string{}
	if p.cfg.ReloadResponses != nil {
		if err := p.cfg.ReloadResponses(); err != nil {
			// 热重载失败不吞：文件已落盘，但运行期未更新，提示需重启。
			log.Printf("panel: responses.json 热重载失败: %v", err)
			restartRequired = []string{"responses"}
		} else {
			log.Printf("panel: responses.json 已保存并热重载（%s）", path)
		}
	} else {
		restartRequired = []string{"responses"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
}

// validateResponsesConfig 校验各字段类型（拒绝脏值，不写盘）。
func validateResponsesConfig(cfg map[string]any) error {
	if v, ok := cfg["enabled"]; ok {
		if _, ok := v.(bool); !ok {
			return errors.New("enabled 必须是布尔值")
		}
	}
	if v, ok := cfg["model_map"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return errors.New("model_map 必须是对象（模型名 → 上游模型名）")
		}
		for k, val := range m {
			if _, ok := val.(string); !ok {
				return fmt.Errorf("model_map[%q] 的值必须是字符串", k)
			}
		}
	}
	if v, ok := cfg["default_model"]; ok {
		if _, ok := v.(string); !ok {
			return errors.New("default_model 必须是字符串")
		}
	}
	if v, ok := cfg["max_cached_responses"]; ok {
		f, ok := v.(float64)
		if !ok || f < 0 || f != float64(int(f)) {
			return errors.New("max_cached_responses 必须是非负整数")
		}
	}
	return nil
}
