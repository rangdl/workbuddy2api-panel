// traepoc — Trae 接入的 P0 可行性验证工具。
//
// 回答两个决定项目成败的问题（见 docs/trae-upstream-plan.md §5）：
//
//	① 服务端环境（无 Trae 客户端）能否完成 OAuth 登录？
//	② token 续期是否必须 DeviceProof 签名（无 proof 的 Legacy 变体是否还有效）？
//
// 用法：
//
//	traepoc login                    交互式：生成授权链接 → 粘贴回调 URL → 逐变体试交换
//	traepoc refresh -refresh <rt>    逐变体试刷新（无 -key 时只试 Legacy 兜底变体）
//	traepoc status -token <jwt>      调签到 status 验证凭证与设备头是否被接受
//	traepoc device <storage.json>    解密客户端凭证（脱敏展示，用于确认 pepper 表仍有效）
//
// 所有子命令都会逐变体打印成功/失败，并在末尾给出结论——这正是 P0 要的答案。
// 本工具只做只读验证：除 login 会真的换取一次 token 外，不改动任何本地状态。
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/trae"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "login":
		cmdLogin(os.Args[2:])
	case "refresh":
		cmdRefresh(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "device":
		cmdDevice(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `traepoc — Trae 接入 P0 可行性验证

  traepoc login                          交互式 OAuth 登录（逐变体报告）
  traepoc refresh -refresh <rt> [-key f] 逐变体试刷新（无 -key 只试 Legacy）
  traepoc status  -token <jwt> [-device-id d]   验证凭证与设备头
  traepoc device  <storage.json>         解密客户端设备凭证（脱敏）

通用参数：-key <私钥 PEM 文件>（提供后才会试 DeviceProof 变体）
          -device-id / -machine-id（缺省自动生成并打印）
          -host <ExchangeToken host>（缺省 api.trae.com.cn）
`)
}

// traceClient 构造带变体追踪的客户端。
func traceClient() *trae.Client {
	c := trae.NewClient()
	c.Trace = func(tag string, ok bool, err error) {
		if ok {
			fmt.Printf("    ✓ %-34s 成功\n", tag)
			return
		}
		fmt.Printf("    ✗ %-34s %v\n", tag, err)
	}
	return c
}

// loadDeviceCredential 读取设备私钥（可选）。返回 nil 表示"无设备凭证"，
// 此时只会尝试无 DeviceProof 的兜底变体——正是 P0 要验证的服务端场景。
func loadDeviceCredential(keyPath, deviceID, machineID string) *trae.DeviceCredential {
	if keyPath == "" {
		return nil
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取私钥文件失败: %v\n", err)
		os.Exit(1)
	}
	pemStr := strings.TrimSpace(string(raw))
	if _, err := trae.ParseECPrivateKey(pemStr); err != nil {
		fmt.Fprintf(os.Stderr, "私钥无法解析（需 EC P-256，PKCS#8 或 SEC1）: %v\n", err)
		os.Exit(1)
	}
	return &trae.DeviceCredential{
		DeviceID:      deviceID,
		MachineID:     machineID,
		PrivateKeyPEM: pemStr,
	}
}

// genDevice 无既有设备标识时生成一对（打印出来，便于后续复用同一设备）。
func genDevice() (deviceID, machineID string) {
	digits := make([]byte, 15)
	for i := range digits {
		h, err := trae.RandomHex(1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "随机数生成失败: %v\n", err)
			os.Exit(1)
		}
		digits[i] = '0' + (h[0] % 10)
	}
	machineID, err := trae.RandomHex(32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "随机数生成失败: %v\n", err)
		os.Exit(1)
	}
	return string(digits), machineID
}

func prompt(label string) string {
	fmt.Print(label)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// ---- login ----

func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	keyPath := fs.String("key", "", "设备私钥 PEM 文件（提供后才试 DeviceInfo 主变体）")
	deviceID := fs.String("device-id", "", "设备 id（缺省自动生成）")
	machineID := fs.String("machine-id", "", "机器 id（缺省自动生成）")
	host := fs.String("host", "", "ExchangeToken host（缺省 api.trae.com.cn）")
	_ = fs.Parse(args)

	dID, mID := *deviceID, *machineID
	if dID == "" || mID == "" {
		g1, g2 := genDevice()
		if dID == "" {
			dID = g1
		}
		if mID == "" {
			mID = g2
		}
	}
	fmt.Println("=== 步骤 1/3：生成授权链接 ===")
	fmt.Printf("  设备 id: %s\n  机器 id: %s\n", dID, mID)

	cred := loadDeviceCredential(*keyPath, dID, mID)
	if cred == nil {
		fmt.Println("  提示：未提供 -key，本次只尝试【无 DeviceProof 的兜底变体】——")
		fmt.Println("        这正是「服务端无 Trae 客户端」的真实场景。")
	} else {
		fmt.Println("  已加载设备私钥，将同时尝试 DeviceInfo 主变体。")
	}

	url, sess, err := trae.BuildLoginURL(trae.LoginURLOptions{
		DeviceID:    dID,
		MachineID:   mID,
		DeviceBrand: "traepoc",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成授权链接失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\n  在浏览器打开以下链接并完成授权：")
	fmt.Println("  " + url)
	fmt.Println("\n=== 步骤 2/3：粘贴回调 URL ===")
	fmt.Println("  授权成功后浏览器会跳转到回调地址（可能显示无法访问，正常）。")
	fmt.Println("  把地址栏里的【完整 URL】粘贴到下面并回车：")
	callback := prompt("  回调 URL> ")
	if callback == "" {
		fmt.Fprintln(os.Stderr, "未提供回调 URL，中止。")
		os.Exit(1)
	}
	authCode, traceID, err := trae.ParseAuthCallback(callback)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析回调失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  已提取 authCode（%d 字符），loginTraceID=%q\n", len(authCode), traceID)

	fmt.Println("\n=== 步骤 3/3：逐变体尝试换取 token ===")
	cfg := &trae.Config{Enabled: true}
	if *host != "" {
		cfg.OAuthHost = *host
	}
	client := traceClient()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pair, err := client.ExchangeAuthCode(ctx, cfg, authCode, sess.PKCEVerifier, cred, dID, mID)
	fmt.Println()
	if err != nil {
		fmt.Printf("  ✗ 全部交换变体失败：%v\n", err)
		fmt.Println("\n=== P0 结论 ===")
		fmt.Println("  ① 服务端能否完成 OAuth 登录：❌ 本次未成功（见上方逐变体错误）")
		fmt.Println("     常见原因：授权链接与回调不匹配 / 授权已过期 / client_id 已变更。")
		fmt.Println("     可重试一次；若持续失败，需按参考实现重新抓包核对参数形态。")
		os.Exit(1)
	}
	fmt.Printf("  ✓ 换取成功：access_token %d 字符，refresh_token %s\n",
		len(pair.AccessToken), present(pair.RefreshToken))
	if uid := trae.JWTUserID(pair.AccessToken); uid != "" {
		fmt.Printf("  账号 uid：%s\n", uid)
	}
	if exp := trae.ExpiryFromJWT(pair.AccessToken); exp > 0 {
		fmt.Printf("  token 过期：%s（剩余 %s）\n",
			time.Unix(exp, 0).Format(time.RFC3339), time.Until(time.Unix(exp, 0)).Round(time.Minute))
	}
	fmt.Println("\n=== P0 结论 ===")
	fmt.Println("  ① 服务端能否完成 OAuth 登录：✅ **可以**（上方标记 ✓ 的变体即为可行路径）")
	fmt.Println("  ② refresh 是否必须 DeviceProof：请接着跑 `traepoc refresh -refresh <rt>` 验证")
	if pair.RefreshToken != "" {
		fmt.Printf("\n  下一步：traepoc refresh -refresh %s\n", pair.RefreshToken)
	}
}

// ---- refresh ----

func cmdRefresh(args []string) {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	refresh := fs.String("refresh", "", "refresh_token")
	keyPath := fs.String("key", "", "设备私钥 PEM 文件")
	deviceID := fs.String("device-id", "", "设备 id")
	machineID := fs.String("machine-id", "", "机器 id")
	host := fs.String("host", "", "ExchangeToken host")
	_ = fs.Parse(args)

	rt := *refresh
	if rt == "" {
		rt = prompt("  refresh_token> ")
	}
	if rt == "" {
		fmt.Fprintln(os.Stderr, "缺少 refresh_token。")
		os.Exit(2)
	}
	dID := *deviceID
	if dID == "" {
		dID, _ = genDevice()
		fmt.Printf("  未指定 -device-id，本次使用随机设备 %s（真实续期须与签发时一致）\n", dID)
	}

	cred := loadDeviceCredential(*keyPath, dID, *machineID)
	fmt.Println("=== 逐变体尝试刷新 ===")
	if cred == nil {
		fmt.Println("  未提供 -key → 只会尝试【Legacy 无 proof 变体】")
	} else {
		fmt.Println("  已加载设备私钥 → 先试 DeviceProof 变体，再试 Legacy")
	}

	cfg := &trae.Config{Enabled: true}
	if *host != "" {
		cfg.OAuthHost = *host
	}
	acc := &trae.Account{UID: "poc", RefreshToken: rt, DeviceID: dID, MachineID: *machineID}
	if cred != nil {
		acc.PrivateKeyPEM = cred.PrivateKeyPEM
	}
	client := traceClient()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pair, err := client.ExchangeRefresh(ctx, cfg, acc)
	fmt.Println()
	if err != nil {
		fmt.Printf("  ✗ 全部刷新变体失败：%v\n", err)
		fmt.Println("\n=== P0 结论 ===")
		fmt.Println("  ② refresh 是否必须 DeviceProof：⚠️ 本次全部变体失败，结论不成立")
		fmt.Println("     若上方 Legacy 变体报「20405 Device proof required」，说明**必须**设备私钥：")
		fmt.Println("     → 面板「上游接入」页导入 storage.json 后重试。")
		os.Exit(1)
	}
	fmt.Printf("  ✓ 刷新成功：access_token %d 字符，refresh_token %s\n",
		len(pair.AccessToken), present(pair.RefreshToken))
	fmt.Println("\n=== P0 结论 ===")
	fmt.Println("  ② refresh 是否必须 DeviceProof：见上方标记 ✓ 的变体——")
	fmt.Println("     · 若 ✓ 的是 Legacy → **不需要**设备私钥（服务端可直接续期，最理想）")
	fmt.Println("     · 若 ✓ 的是 Proof  → **需要**设备私钥（用户须导入 storage.json）")
}

// ---- status ----

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	token := fs.String("token", "", "access_token（可带 Cloud-IDE-JWT 前缀）")
	deviceID := fs.String("device-id", "", "设备 id")
	base := fs.String("api-base", "", "api base（缺省 api.trae.cn）")
	_ = fs.Parse(args)

	tk := *token
	if tk == "" {
		tk = prompt("  access_token> ")
	}
	if tk == "" {
		fmt.Fprintln(os.Stderr, "缺少 access_token。")
		os.Exit(2)
	}
	dID := *deviceID
	if dID == "" {
		dID, _ = genDevice()
		fmt.Printf("  未指定 -device-id，本次使用随机设备 %s\n", dID)
	}
	cfg := &trae.Config{Enabled: true}
	if *base != "" {
		cfg.APITraeBase = *base
	}
	acc := &trae.Account{UID: "poc", AccessToken: tk, DeviceID: dID}

	fmt.Println("=== 调用签到 status 验证凭证与设备头 ===")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := trae.NewClient().CheckinStatus(ctx, cfg, acc)
	if err != nil {
		fmt.Printf("  ✗ 失败：%v\n", err)
		fmt.Println("\n  判读：")
		fmt.Println("   · 401 / code=1001 → token 无效或已被吊销（与设备头无关）")
		fmt.Println("   · 20403/20405     → 设备标识与签发时不一致（需用原设备 id）")
		os.Exit(1)
	}
	checked := "未知"
	if st.CheckedIn != nil {
		if *st.CheckedIn {
			checked = "今日已签到"
		} else {
			checked = "今日未签到"
		}
	}
	fmt.Printf("  ✓ 凭证有效：%s，当前积分 %d\n", checked, st.Credits)
	fmt.Println("\n=== 结论 ===")
	fmt.Println("  ③ 凭证 + 设备头被上游接受 ✅（chat 链路可复用同一凭证）")
}

// ---- device ----

func cmdDevice(args []string) {
	fs := flag.NewFlagSet("device", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "用法: traepoc device <storage.json 路径>")
		os.Exit(2)
	}
	path := fs.Arg(0)
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("=== 解析 storage.json 中的 icube 设备凭证 ===")
	creds, err := trae.ParseStorageJSON(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ✗ 解析失败: %v\n", err)
		os.Exit(1)
	}
	if len(creds) == 0 {
		fmt.Println("  ✗ 未找到 iCubeAuthInfo 条目")
		fmt.Println("\n  判读：")
		fmt.Println("   · 该客户端可能未登录过 Trae（无设备凭证）")
		fmt.Println("   · 或 pepper 常量表已随客户端版本变化（需从 out/main.js 重新提取）")
		os.Exit(1)
	}
	for i, c := range creds {
		fmt.Printf("  [%d] device_id=%s machine_id=%s 私钥指纹=%s\n",
			i+1, c.DeviceID, present(c.MachineID), keyFingerprint(c.PrivateKeyPEM))
	}
	fmt.Println("\n=== 结论 ===")
	fmt.Println("  ✅ tc 信封解密可用（pepper 常量表仍有效）——")
	fmt.Println("     可把该文件（或单条 deviceId:密文）粘贴到面板「上游接入」页导入。")
}

// ---- 小工具 ----

// present 只报"有没有"与长度，绝不打印凭证内容。
func present(s string) string {
	if strings.TrimSpace(s) == "" {
		return "无"
	}
	return fmt.Sprintf("已获取(%d 字符)", len(s))
}

// keyFingerprint 私钥指纹（SHA-256 前 8 字节）：用于确认导入的是哪一把密钥，
// 不可逆、不足以还原私钥。
func keyFingerprint(pemStr string) string {
	if strings.TrimSpace(pemStr) == "" {
		return "无"
	}
	sum := sha256.Sum256([]byte(pemStr))
	return hex.EncodeToString(sum[:8])
}
