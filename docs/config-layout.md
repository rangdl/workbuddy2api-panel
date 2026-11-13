# 配置与数据目录

从本版开始，配置文件不再平铺在启动目录，而是分成两类目录：

```
config/                        用户会手改的配置
├── config.json                主配置（监听、api_key、池参数、排程…）
├── responses.json             Responses / Codex 接入
├── anthropic.json             Anthropic / Claude Code 接入
├── third_party.json           第三方 OpenAI 兼容上游
├── trae.json                  Trae 上游
├── model.json                 模型上下文/输出上限缓存（可手改）
└── .migrated                  迁移标记（自动生成，勿手动创建）

data/                          程序自己写的运行时状态
├── state.json                 账号池状态
├── usage.json                 逐请求用量
├── credit-history.json        积分变动流水
├── output_probes.json         模型上限探测结果
└── request-logs/              JSONL 请求归档

auths/                         账号凭证（每账号一文件，权限 0600）
├── workbuddy-<uid>.json       CodeBuddy 账号
└── trae-<uid>.json            Trae 账号
```

分家的理由：`config/` 只需要读、备份频率低、内容是人写的；`data/` 高频写入、
可以直接丢弃重建；`auths/` 含明文 token 与设备私钥，权限单独收紧且**绝不入库**。
分成三个目录后，备份策略和挂载权限可以分别设置。

## Trae 账号存储（一账号一文件）

Trae 账号**不再内嵌在 `trae.json` 里**，改为 `auths/trae-<uid>.json`，
与 CodeBuddy 的 `auths/workbuddy-<uid>.json` 形态一致。`trae.json` 只保留全局配置
（开关、base、排程、UA、verify_claim 等）。

理由：

- **并发安全**：Trae 的 refreshToken 是轮换链，两个账号同时续期时若都重写同一个
  `trae.json`，后写者会覆盖先写者刚更新的另一个账号。分文件后各写各的。
- **权限最小化**：账号文件含明文 refreshToken 与设备私钥，单独 `0600`。
- **心智一致**：备份 / 手工编辑 / 导入导出的方式与 CodeBuddy 相同。

`trae.json` 的 `auth_dir` 指定账号目录，缺省为 `trae.json` 同级的 `auths/`
（与 CodeBuddy 共用目录，靠 `trae-` / `workbuddy-` 前缀区分）。

> **旧版自动迁移**：启动时若发现 `trae.json` 里还有 `accounts` 数组，会把它拆成
> 一账号一文件后清空该数组。迁移是幂等的；任何一条写失败都会保留源数组并告警，
> 下次启动重试——不会让账号凭空消失。

## 首次启动

- 两个目录不存在时自动创建。
- `config/config.json` 不存在时，生成一份含随机 `api_key` 的推荐配置，并把
  `api_key` 打印到日志（只打印这一次）。
- 四个副配置不存在时，各写一份默认文件。其中 `anthropic.json` /
  `third_party.json` / `trae.json` 一律 `enabled: false`——写默认值不会替用户
  开启一个没配过的上游；`responses.json` 保持既有行为，默认启用。

## 从旧版升级

旧版把上述文件全部平铺在启动目录（`./config.json`、`./state.json`…）。新版本
启动时会自动**复制**到新位置：

- 只复制、不删除。旧文件原样保留，确认无误后自行删除。
- 目标已存在时一律跳过，绝不覆盖。
- 迁移成功后在 `config/.migrated` 写标记，之后不再探测旧路径。
- 显式用 `-config` 指到别处时**不做迁移**——那是调用方给的明确意图。

## `-config` 参数

```
wb2api -config config/config.json     # 默认值
wb2api -config /etc/wb2/config.json   # 配置根取 /etc/wb2/
```

配置根 = `-config` 的父目录。`data/` 固定在启动目录下，不跟着配置文件跑。

## Docker

```yaml
volumes:
  - ./auths:/app/auths
  - ./data:/app/data
  - ./config:/app/config
```

旧版是 `./config.json:/app/config.json`。升级时把宿主机文件移进目录：

```sh
mkdir -p config && mv config.json config/ 2>/dev/null
```

容器启动命令为 `-config /app/config/config.json`。
