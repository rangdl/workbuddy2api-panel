# 配置与数据目录

从本版开始，配置文件不再平铺在启动目录，而是分成两类目录：

```
config/                        用户会手改的配置
├── config.json                主配置（监听、api_key、池参数、排程…）—— 与上游无关
├── codebuddy.json             CodeBuddy 上游（cn / global 两域）
├── trae.json                  Trae 上游（cn 域）
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

## 上游配置按文件分离

**每个上游的独特配置各占一个 JSON 文件**，`config.json` 只放与上游无关的东西
（监听、密钥、账号池策略、排程）。

| 文件 | 归谁 | 内容 |
|---|---|---|
| `codebuddy.json` | CodeBuddy | `cn` / `global` 两域 base、超时、UA、版本、设备令牌、指纹脱敏 |
| `trae.json` | Trae | `cn` 域 base、IDE 版本、签到 UA、OAuth 常量、签到链路可调项 |

为什么这样分：

- **加上游 = 加一个文件**，`config.json` 一行不动；
- **删上游 = 删一个文件**，不留没人敢删的死字段；
- **字段归属看得见**，不用猜 `user_agent` 是给谁的。

文件内**保留域维度**（`cn` / `global` 各一段）：域隔离是硬约束（见
`docs/architecture.md` 的「域隔离失效」历史事故）。Trae 目前只有 `cn`，
将来加 global 只加一段、不改结构。

**超时与 SSE 的两个上限（首字节 / 流空闲）属于 `config.json` 的 `upstream` 段，
不在任何上游文件里**。原因：它们描述的是**传输层行为**，与上游协议无关——
WorkBuddy 与 Trae 的对话都是 SSE 流式长连接，两个上限对两者语义完全相同。

> 曾经 Trae 的首字节超时是硬编码常量、流中空闲完全没有监控，正是因为它被当成
> "CodeBuddy 的东西"。放进上游文件会继续强化这种误解。

### 缺字段的行为

`codebuddy.json` / `trae.json` 缺失或字段为空时**全部回落内置默认**，不会启动失败：

| 情况 | 行为 |
|---|---|
| 文件不存在 | 用内置默认（`codebuddy.json` 首次启动会自动生成） |
| 整个 `cn` 段缺失 | 该域用内置 base |
| `user_agent` 为空串 | 用内置三段式 UA |
| `timeout_seconds` 为 0 | 回落 120 |

### 从旧版升级（自动迁移）

旧版把上游字段放在 `config.json` 的 `cn` / `global` / `upstream` / `features`
四段里。启动时若发现 `codebuddy.json` 不存在而这些段还在，会：

1. 把字段搬进 `codebuddy.json`；
2. 从 `config.json` 删掉这四个键（保证**配置单一来源**，避免改旧键不生效）；
3. 打一条日志说明。

迁移只在 `codebuddy.json` **不存在**时执行——已存在的文件是当前事实来源，
不会被旧配置覆盖。

## Trae 账号存储（一账号一文件）

Trae 账号**不再内嵌在 `trae.json` 里**，改为 `auths/trae-<uid>.json`，
与 CodeBuddy 的 `auths/workbuddy-<uid>.json` 形态一致。`trae.json` 只保留全局配置
（开关、base、排程、UA、verify_claim 等）。

理由：

- **并发安全**：Trae 的 refreshToken 是轮换链，两个账号同时续期时若都重写同一个
  `trae.json`，后写者会覆盖先写者刚更新的另一个账号。分文件后各写各的。
- **权限最小化**：账号文件含明文 refreshToken 与设备私钥，单独 `0600`。
- **心智一致**：备份 / 手工编辑 / 导入导出的方式与 CodeBuddy 相同。

`trae.json` 现在只剩**上游协议+凭证**类字段（OAuth 客户端常量、设备私钥策略、
IDE 版本、签到 UA、`verify_claim` 等），**不再是"上游开关"的载体**：

- 没有 `enabled` 开关了——上游平权，加了账号即启用（WorkBuddy 也一样）。
- 没有 `chat_base` 可配项——上游路径不该由用户改。
- 没有 `schedule` 生效语义——签到排程统一在 `config.json` 的
  `schedule.checkin_enabled`（总开关）+ `schedule.checkin_backends`（按上游）。
  文件里的 `schedule` 键仍会被反序列化，但**不参与任何决策**。

因此 `trae.json` 仍然需要保留：去掉它就得把这些上游协议常量硬编码进代码，
反而失去"上游改版时不发版就能覆盖"的能力。

`trae.json` 的 `auth_dir` 指定账号目录；**留空时用主配置 `config.json` 的
`auth_dir`**（缺省 `./auths`，相对启动目录），从而与 workbuddy 账号落在同一目录，
靠 `trae-` / `workbuddy-` 前缀区分。

> 注意不要按 `trae.json` 的位置推导账号目录：配置在 `config/`、账号在根 `auths/`，
> 两者**不同级**。早期实现按配置文件推导，导致 Trae 账号被写到 `config/auths/`
> 而与 workbuddy 分家。

> **旧版自动迁移**：启动时若发现 `trae.json` 里还有 `accounts` 数组，会把它拆成
> 一账号一文件后清空该数组。迁移是幂等的；任何一条写失败都会保留源数组并告警，
> 下次启动重试——不会让账号凭空消失。

## 首次启动

- 两个目录不存在时自动创建。
- `config/config.json` 不存在时，生成一份含随机 `api_key` 的推荐配置，并把
  `api_key` 打印到日志（只打印这一次）。
- `trae.json` 不存在时写一份默认文件。注意：**没有 `enabled` 开关**（上游平权，
  加了账号即启用），文件存在与否不代表上游启用与否，真正的判据是有没有账号。（`responses.json` / `anthropic.json` / `third_party.json`
  已随对应功能移除，不再生成，也不再从旧目录迁移。）

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
