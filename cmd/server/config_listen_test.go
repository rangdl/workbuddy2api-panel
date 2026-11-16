package main

import (
	"strings"
	"testing"
)

// config_listen_test.go 守「listen 非法值不得落盘」这条约束。
//
// 背景：配置页的「监听地址」会被浏览器自动填充（Safari 把昵称/地址塞进
// name="listen" 的框）。用户手滑保存后，config.json 里是个非地址字符串，
// 进程下次启动直接 bind 失败——而那时面板也打不开，只能手工改文件。
//
// 前端已加 autocomplete 抑制（尽力而为，各浏览器行为不一），
// 这里断言的是落盘前的确定性防线。

// TestNormalizeRejectsInvalidListen 非地址串必须被 normalize 拒绝。
func TestNormalizeRejectsInvalidListen(t *testing.T) {
	bad := []string{
		"rang",                // 浏览器把用户昵称填进来
		"张三",                  // 中文输入
		"https://example.com", // 填成了 URL
		":",                   // 只有冒号，没有端口
		":abc",                // 端口不是数字
		":99999",              // 端口越界
		"1.2.3.4:8080:9090",   // 多个冒号
	}
	for _, listen := range bad {
		c := Default()
		c.Listen = listen
		err := c.normalize()
		if err == nil {
			t.Errorf("listen=%q 应被拒绝，却通过了 normalize", listen)
			continue
		}
		if !strings.Contains(err.Error(), "listen") {
			t.Errorf("listen=%q 的错误信息应提到 listen，得到: %v", listen, err)
		}
	}
}

// TestNormalizeAcceptsValidListen 合法形态必须放行（含自动补冒号的简写）。
func TestNormalizeAcceptsValidListen(t *testing.T) {
	good := []string{
		":7863",
		":0", // 端口 0 = 随机分配，合法
		"127.0.0.1:8080",
		"0.0.0.0:3089",
		"[::1]:8080", // IPv6
		"7863",       // 简写，normalize 补成 ":7863"
	}
	for _, listen := range good {
		c := Default()
		c.Listen = listen
		if err := c.normalize(); err != nil {
			t.Errorf("listen=%q 应被接受，却报错: %v", listen, err)
		}
	}
}

// TestNormalizeExpandsBarePort 简写端口补成 ":端口"（既有行为不能丢）。
func TestNormalizeExpandsBarePort(t *testing.T) {
	c := Default()
	c.Listen = "3089"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Listen != ":3089" {
		t.Errorf("listen = %q, want \":3089\"", c.Listen)
	}
}
