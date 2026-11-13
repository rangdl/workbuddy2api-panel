// layout.go 统一配置与数据的存放布局，并在启动时把旧版散落的文件迁移到新布局。
//
// 目录约定（相对于启动目录）：
//
//	config/   用户会手改的配置：config.json / responses.json / anthropic.json
//	          / third_party.json / trae.json / model.json
//	data/     程序自己写的运行时状态：state.json / usage.json / credit-history.json
//	          / output_probes.json / request-logs/
//
// 为什么要分家：旧版把这些全平铺在启动目录，用户手改的配置和程序频繁覆写的状态
// 混在一起，备份、挂载、权限都只能整体处理。
//
// 迁移策略（只复制、不删除）：
//   - 新路径不存在而旧平铺路径存在 → 复制过去并打日志，旧文件原样保留，
//     用户确认无误后自行删除。多占一份磁盘，换掉不可逆迁移的风险。
//   - 目标已存在一律跳过，绝不覆盖。
//   - 显式用 -config 指到 config/ 之外时不做迁移：那是脚本/容器给的明确意图。
//   - 迁移后写 config/.migrated 标记，之后不再探测旧路径。
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

const (
	configDirName = "config"
	dataDirName   = "data"
	migrateFlag   = ".migrated"
)

// layout 一次启动内解析出的全部路径，避免各处重复推导。
type layout struct {
	ConfigDir string // config/ 绝对或相对路径
	DataDir   string // data/ 路径
}

// newLayout 由 -config 参数解析布局。
//
// -config 的语义：它指向主配置文件本身。默认值是 config/config.json，
// 此时 ConfigDir/DataDir 就是从启动目录算起的两个子目录。
// 若用户显式指到别处（如 /app/config.json），配置根取它的父目录，
// 且 DataDir 固定在启动目录下（不跟着配置文件到处跑）。
func newLayout(configPath string) layout {
	dir := filepath.Dir(configPath)
	if dir == "" || dir == "." {
		return layout{ConfigDir: configDirName, DataDir: dataDirName}
	}
	return layout{ConfigDir: dir, DataDir: dataDirName}
}

// File 返回布局内某个文件的完整路径。
func (l layout) File(dir, name string) string { return filepath.Join(dir, name) }

// configPath 返回主配置路径（就是 -config 原值）。
func (l layout) configPath(flagValue string) string { return flagValue }

// Ensure 创建两个目录。
func (l layout) Ensure() error {
	for _, d := range []string{l.ConfigDir, l.DataDir} {
		if d == "" || d == "." {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

// legacyFile 旧平铺名 → 迁移目标（"config" 或 "data" 类）。
type legacyFile struct {
	Name   string
	Target string // "config" | "data"
}

var legacyFiles = []legacyFile{
	{"config.json", "config"},
	{"responses.json", "config"},
	{"anthropic.json", "config"},
	{"third_party.json", "config"},
	{"trae.json", "config"},
	{"model.json", "config"},
	{"state.json", "data"},
	{"usage.json", "data"},
	{"credit-history.json", "data"},
	{"output_probes.json", "data"},
}

// Migrate 把启动目录下旧版平铺的文件复制进新布局。返回复制成功的文件数。
//
// 仅在启动目录（"."）下探测：—config 指到别处时调用方不应调用本函数。
func (l layout) Migrate() int {
	if _, err := os.Stat(filepath.Join(l.ConfigDir, migrateFlag)); err == nil {
		return 0
	}
	moved := 0
	for _, lf := range legacyFiles {
		dstDir := l.ConfigDir
		if lf.Target == "data" {
			dstDir = l.DataDir
		}
		dst := filepath.Join(dstDir, lf.Name)
		if _, err := os.Stat(lf.Name); err != nil {
			continue // 旧文件不存在
		}
		if _, err := os.Stat(dst); err == nil {
			continue // 新文件已存在，不覆盖
		}
		if err := copyFile(lf.Name, dst); err != nil {
			log.Printf("WARN: [migrate] 复制 %s → %s 失败: %v", lf.Name, dst, err)
			continue
		}
		log.Printf("[migrate] 已复制 %s → %s（旧文件保留，确认无误后可删除）", lf.Name, dst)
		moved++
	}
	// 请求归档目录整目录复制。
	if st, err := os.Stat("request-logs"); err == nil && st.IsDir() {
		dst := filepath.Join(l.DataDir, "request-logs")
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if err := copyDir("request-logs", dst); err != nil {
				log.Printf("WARN: [migrate] 复制归档目录失败: %v", err)
			} else {
				log.Printf("[migrate] 已复制 request-logs → %s", dst)
				moved++
			}
		}
	}
	if moved > 0 {
		_ = os.MkdirAll(l.ConfigDir, 0o755)
		_ = os.WriteFile(filepath.Join(l.ConfigDir, migrateFlag), []byte("migrated\n"), 0o600)
	}
	return moved
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		return copyFile(path, target)
	})
}
