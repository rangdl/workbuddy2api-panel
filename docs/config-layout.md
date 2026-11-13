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
```

分家的理由：`config/` 只需要读、备份频率低、内容是人写的；`data/` 高频写入、
可以直接丢弃重建。分成两个目录后，备份策略和挂载权限可以分别设置。

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
