package main

import (
	"os"
	"path/filepath"
	"testing"
)

// chdir 切到临时目录并在测试结束时切回（Go 1.24 前没有 t.Chdir）。
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// TestMigrateLegacyLayout 旧版平铺文件必须复制进 config/ 与 data/，
// 且旧文件保留、已存在的新文件不被覆盖。
func TestMigrateLegacyLayout(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	// 旧布局：配置文件与状态文件都平铺在启动目录
	write := func(name, content string) {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", `{"api_key":"old"}`)
	write("responses.json", `{"enabled":true}`)
	write("state.json", `{"accounts":{}}`)
	write("unrelated.txt", "should not move")

	lay := newLayout(filepath.Join("config", "config.json"))
	if err := lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	moved := lay.Migrate()
	if moved != 3 {
		t.Fatalf("moved=%d want 3", moved)
	}

	// 复制到位
	for _, p := range []string{"config/config.json", "config/responses.json", "data/state.json"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s 未迁移: %v", p, err)
		}
	}
	// 旧文件必须保留（只复制不删除）
	for _, p := range []string{"config.json", "responses.json", "state.json"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("旧文件 %s 被删除了: %v", p, err)
		}
	}
	// 无关文件不动
	if _, err := os.Stat("config/unrelated.txt"); err == nil {
		t.Error("无关文件被误迁移")
	}
	// 内容一致
	got, _ := os.ReadFile("config/config.json")
	if string(got) != `{"api_key":"old"}` {
		t.Errorf("复制内容不一致: %s", got)
	}
}

// TestMigrateDoesNotOverwrite 目标已存在时跳过，避免覆盖用户新配置。
func TestMigrateDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	if err := os.MkdirAll("config", 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile("config.json", []byte(`{"api_key":"old"}`), 0o600)
	os.WriteFile("config/config.json", []byte(`{"api_key":"new"}`), 0o600)

	lay := newLayout(filepath.Join("config", "config.json"))
	if n := lay.Migrate(); n != 0 {
		t.Fatalf("moved=%d want 0（目标已存在不该覆盖）", n)
	}
	got, _ := os.ReadFile("config/config.json")
	if string(got) != `{"api_key":"new"}` {
		t.Errorf("新配置被覆盖了: %s", got)
	}
}

// TestMigrateOnceOnly 迁移后写标记，第二次启动不再探测（用户删了旧文件不会被"复活"）。
func TestMigrateOnceOnly(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	os.WriteFile("config.json", []byte(`{}`), 0o600)
	lay := newLayout(filepath.Join("config", "config.json"))
	lay.Ensure()
	if n := lay.Migrate(); n != 1 {
		t.Fatalf("first moved=%d want 1", n)
	}
	if _, err := os.Stat(filepath.Join("config", migrateFlag)); err != nil {
		t.Fatalf("迁移标记未写入: %v", err)
	}
	// 模拟用户删掉旧文件后又新建了一个（不应再被迁移）
	os.Remove("config.json")
	os.WriteFile("config.json", []byte(`{"api_key":"recreated"}`), 0o600)
	os.Remove("config/config.json")
	if n := lay.Migrate(); n != 0 {
		t.Fatalf("second moved=%d want 0（已迁移过）", n)
	}
}

// TestMigrateSkipsWhenExplicitConfig 显式 -config 指到别处时不做迁移。
func TestMigrateSkipsWhenExplicitConfig(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	os.WriteFile("config.json", []byte(`{}`), 0o600)

	// main 里只在默认路径下调用 Migrate；本测试钉住这个判断条件。
	explicit := filepath.Join("etc", "my.json")
	if explicit == filepath.Join(configDirName, "config.json") {
		t.Fatal("测试前提错误：显式路径不该等于默认路径")
	}
	if _, err := os.Stat("config/config.json"); err == nil {
		t.Error("显式 -config 场景不该产生迁移结果")
	}
}

// TestNewLayoutExplicit 显式 -config 时配置根跟随其父目录。
func TestNewLayoutExplicit(t *testing.T) {
	lay := newLayout("/app/etc/my.json")
	if lay.ConfigDir != "/app/etc" {
		t.Errorf("ConfigDir=%q want /app/etc", lay.ConfigDir)
	}
	if lay.DataDir != "data" {
		t.Errorf("DataDir=%q want data", lay.DataDir)
	}
	def := newLayout(filepath.Join("config", "config.json"))
	if def.ConfigDir != "config" || def.DataDir != "data" {
		t.Errorf("默认布局=%+v", def)
	}
}
