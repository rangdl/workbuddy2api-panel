// anthropic_config.go 面板「Anthropic / Claude Code 接入」配置页接口：
// 读写独立的 anthropic.json（与 config.json 同目录），模式与 responses_config.go 一致。
package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// claudeDefaultModels 面板 model_map 预填的 Claude 客户端模型名（左列）。
// 来源：Claude Code 常用模型 slug；网关仅作展示默认，用户可增删。
var claudeDefaultModels = []string{
	"claude-opus-4-6",
	"claude-sonnet-4-5",
	"claude-sonnet-4-0",
	"claude-haiku-4-5",
	"claude-3-7-sonnet-latest",
}

// defaultAnthropicConfig anthropic.json 不存在时回显的默认值（与 loadAnthropicConfig 语义一致）。
func defaultAnthropicConfig() map[string]any {
	return map[string]any{
		"enabled":       false,
		"model_map":     map[string]string{},
		"default_model": "",
	}
}

// getAnthropicConfig 读取 anthropic.json（不存在时返回默认值，不报错）。
func (p *Panel) getAnthropicConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.AnthropicPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "anthropic config api not available")
		return
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "path": path, "config": defaultAnthropicConfig(), "claude_models": claudeDefaultModels,
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read anthropic.json: "+err.Error())
		return
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, "parse anthropic.json: "+err.Error())
		return
	}
	// 原始字节返回：保留 model_map 键顺序（前端拖拽排序展示）。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "path": path, "config": json.RawMessage(raw), "claude_models": claudeDefaultModels,
	})
}

// saveAnthropicConfig 校验并写入 anthropic.json；成功后触发热重载（无需重启）。
func (p *Panel) saveAnthropicConfig(w http.ResponseWriter, r *http.Request) {
	path := p.cfg.AnthropicPath
	if path == "" {
		writeErr(w, http.StatusNotImplemented, "anthropic config api not available")
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
	if err := validateAnthropicConfig(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 保存时格式化原始 body：保留前端传入的字段顺序（model_map 键顺序持久化）。
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
		writeErr(w, http.StatusInternalServerError, "write anthropic.json: "+err.Error())
		return
	}
	restartRequired := []string{}
	if p.cfg.ReloadAnthropic != nil {
		if err := p.cfg.ReloadAnthropic(); err != nil {
			log.Printf("panel: anthropic.json 热重载失败: %v", err)
			restartRequired = []string{"anthropic"}
		} else {
			log.Printf("panel: anthropic.json 已保存并热重载（%s）", path)
		}
	} else {
		restartRequired = []string{"anthropic"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restartRequired})
}

// validateAnthropicConfig 校验各字段类型（拒绝脏值，不写盘）。
func validateAnthropicConfig(cfg map[string]any) error {
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
	return nil
}
