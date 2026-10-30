# cpa_qcoder

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **Qoder** 插件。
把 [Qoder](https://qoder.com) 作为 CPA 原生 provider 接入：账号、路由、调度、
用量统计、认证 UI 全部复用 CPA，插件只补 Qoder 特有的部分。

账号以普通 CPA auth 文件保存（一个 Qoder 账号一个文件），因此 CPA 的调度器、
认证界面、模型别名、用量统计都直接生效。

基于 [qoder2api](https://github.com/Zhengyuuuui/qoder2api) 的 bridge 核心二次开发。

## 功能

- OAuth 设备登录、PAT 导入，设备令牌自动刷新
- 模型列表（静态 + 按账号动态）、模型开关
- OpenAI `chat/completions`（流式 / 非流式）
- 额度查询（含专属资源包）、每日签到（手动 + 定时）
- **新版推理协议**：账号配置真实 `checkin_fingerprint` 后，`model/list` 额外解锁
  Sonus（`smodel`）/ Cantus（`cmodel`）/ Fast（`fast`）；未配置的账号保持旧协议
- 上游 `RESOURCE_LIMIT`、排队（`10605 isQueued`）等错误返回可读原因
- Token 续期沿用宿主的 `auth.refresh` 定时回调（OAuth 每 24 小时检查）
- 内嵌管理面板

## 构建

需要 Go（cgo）和 C 工具链：

```bash
scripts/build-cpa-plugin.sh 0.1.1
# -> dist/qoder-v0.1.1.so (+ .h)
```

## 安装

```bash
cp dist/qoder-v0.1.1.so /path/to/cli-proxy-api/plugins/linux/amd64/
```

在 `config.yaml` 里启用：

```yaml
plugins:
    enabled: true
    configs:
        qoder:
            enabled: true
            # 可选
            checkin_auto: false
            checkin_times: ["10:00"]
            checkin_on_start: false
            default_region: global
```

重启 CPA。面板地址：`/v0/resource/plugins/qoder/panel`。

## 添加账号

**OAuth（推荐）**：CPA 管理界面 **Add auth → Qoder**。插件驱动设备登录：返回登录
链接，浏览器授权后由宿主轮询拿到 token，最终存成普通 CPA auth 文件。

**PAT / 手动导入**：创建 auth 文件（或在面板导入）：

```json
{ "type": "qoder", "secret": "pt-xxxxxxxx", "region": "global" }
```

OAuth 凭证也可直接导入：

```json
{ "type": "qoder", "device_token": "dt-xxxx", "refresh_token": "drt-xxxx", "region": "global" }
```

凭证会先校验再保存；`region` 默认 `global`，国内账号用 `cn`。

## 管理 API

所有路由需要 CPA 管理密钥（`Authorization: Bearer <key>`），基址
`/v0/management/plugins/qoder`。

| Method | Path | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表（region、plan、签到天数） |
| POST | `/checkin` | 签到：`{account_id}` 单账号，留空全部 |
| GET | `/account/quota?account_id=…` | 单账号实时额度 |
| POST | `/import` | 导入 `{secret, region, name}` |
| POST | `/accounts/delete` | 删除 `{account_id}` |
| GET/POST | `/checkin/config` | 读取/修改自动签到配置 |
| GET/POST | `/models/config` | 读取/保存模型开关 `{"disabled":[…]}` |

## 模型开关与签到

面板 **模型管理** 打开全部可选模型的勾选网格；取消勾选即从 `/v1/models`
隐藏并不可路由。开关以 denylist 存在 `<data-dir>/models.json`，新发现的模型
默认可见。与宿主的 `oauth-excluded-models` 叠加生效。

签到走 `campaigns` 流程，支持手动/定时、本地连续天数统计。国际版账号可能需要在
auth 文件里提供官方客户端的 `checkin_fingerprint`；国内账号则**不能**发送自造指纹。

### 新版推理协议（Sonus / Cantus / Fast）

上游按 `(cosy-clienttype, cosy-version)` 下发模型清单。插件在账号 auth 文件里读到
真实设备指纹后，会把该账号切到官方桌面端同款的新版协议；没有指纹的账号继续走旧协议，
行为不变。

```json
{
  "type": "qoder",
  "secret": "dt-...",
  "region": "global",
  "checkin_fingerprint": {
    "machine_id": "抓包得到的 UUID",
    "machine_token": "与 machine_id 相同",
    "machine_type": "5",
    "client_ip": "抓包得到的 UUID",
    "client_type": "10",
    "version": "1.1.69",
    "scene": "app",
    "product": "app",
    "data_policy": "agree"
  }
}
```

`machine_token` 是切换开关；只填 `machine_id` 不会改变协议。指纹必须来自官方客户端
抓包，自造值会被上游以 `RESOURCE_LIMIT` 拒绝。切换后 `model/list` 会从 13 个模型
增加到 15 个，新增 Sonus / Cantus / Fast。Sonus 倍率为 8，热门时段返回
`10605 isQueued` 属正常容量排队，换模型或稍后重试即可。

## 数据布局

| 路径 | 内容 |
|---|---|
| `~/.cli-proxy-api/qoder-*.json` | 账号凭证（CPA auth 文件，权威存储） |
| `~/.qoder2api-cpa/models.json` | 模型开关 |
| `~/.qoder2api-cpa/plugin_settings.json` | 面板改过的配置 |
| `~/.qoder2api-cpa/checkin_history.json` | 本地签到历史 |
| `~/.qoder2api-cpa/logs/` | 插件日志 |

数据根可用 `QODER2API_HOME` 覆盖。

## 模块

```
cmd/cpa-plugin/
  main.go        C ABI + RPC 分发 + 注册 + 配置
  auth.go        凭证解析、auth.parse/login/refresh
  refresh.go     设备令牌刷新
  settings.go    面板配置持久化
  modelstate.go  模型开关 denylist
  models.go      model.static / model.for_auth
  executor.go    execute / execute_stream + host 流桥
  checkin.go     签到核心 + 调度
  management.go  管理路由 + host auth RPC
  panel.go/.html 内嵌面板
```

插件复用 `qoder2api/account`、`internal/cosy`、`internal/bridge`；上游核心只保留
最小补丁（`account/oauth.go`、`account/region.go`、`internal/bridge/*`、`go.mod`），
同步上游时按此清单解决冲突。

## 上游同步

- 2026-10-30：合并 `6cae517..8d49aa6`（CALL_COUNT_LIMIT 排队报文被预筛选漏掉）。
  上游把 `parseUpstreamNestedError` 预筛选从匹配带引号的 `"subCode"` 改为匹配
  不带引号的 `subCode`——内层 JSON 嵌在外层 message 字符串里、引号被转义成
  `\"subCode\"`，导致与 `RESOURCE_LIMIT` 同构的 `CALL_COUNT_LIMIT`（带
  `isQueued:true`/`retryAfterSeconds`）被误判普通 `upstream_error`，误冷却+换号、
  同账号等待不生效。修复后真排队报文正确进入排队路径。
  **分歧处理（本地语义优先）**：上游新增回归用例
  `TestFriendlyUpstreamError_CallCountLimitQueued` 断言排队错误在
  `ShouldFailover` 一律不换号——与本地架构相悷（见上条 2026-10-27 记录），
  已改写为本地语义：池层保持可换号，不换号由同账号等待路径完成；
  并补充 `queueWorthSameAccountRetry`（真队列）断言。
  README 冲突按本地精简版解决，未并入上游完整 README。

- 2026-10-27：合并 `872d9b9..6cae517`（队列「首字前一直等」开关 + 最近排队等待
  记录）。上游把 10605 `isQueued` 从「按次数封顶重试」改为可无限等待，并新增
  `wait-log` 环形缓冲 + `/api/queue/log` 与控制台面板。
  **冲突处理（本地语义优先）**：上游版的 `streamWithSlot` 丢掉了
  `queueWorthSameAccountRetry` gate——「首字前一直等」一旦生效，假队列
  （`queueCount=0 && serviceAvailable=false`）会被它一直等到客户端断开，外层
  账号池就再也不换号了。本地保留 gate，并把它放在「首字前一直等」判定**之前**，
  假队列永远先上抛去换号；新增回归用例
  `TestStreamWithSlotFakeQueueIgnoresWaitFirstToken`（去掉 gate 即 3s 后失败）。
  README 冲突按本地精简版解决，未并入上游完整 README。
  注意：`ApplyQueueSettings` 签名新增第 4 个参数，本地调用点（`service.go`、
  `main.go`）已全部更新。

- 2026-10-26：合并 `62facdb..872d9b9`（纯文档/面板，无 Go 代码改动）。
  上游刷新模型清单快照：`kmodel_latest` Kimi-K3 → **Kimi-K3.1-Pro**（key 不变，
  倍率 1.4→1）、CN 移除 `qmodel`（11→10 个）、Global `gfmodel` max_output
  16,384→32,768、CN `qmodel_38max` 倍率 0.2→0.5；`webconsole/index.html` 的
  NewAPI 模型数组同步为当前 key。模型清单本就运行时从 `/algo/api/v2/model/list`
  动态拉取，故本次不改代码；本地静态兜底表（`cmd/cpa-plugin/models.go`）仍是
  2026-10-20 快照，仅在探测失败时使用，本次未同步（见下条待办）。

- 2026-10-23：合并 [qoder2api](https://github.com/Zhengyuuuui/qoder2api)
  `2671a21..62facdb`，含 **context 多档位（200K / 400K / 1M）** 与排队重试补全
  （`errors.go`/`pool.go`）。档位可经 `context_tier_default` / `context_tier_overrides`
  配置，按模型目录 5 分钟缓存解析为 token 数后写入 `model_config.max_input_tokens`
  （上游标注：该传递字段未实测验证）。插件侧保留本地 `AvailableCredits` 汇总与
  reasoning_effort / thinking_budget 透传，无行为冲突。

- 2026-10-23：合并 [qoder2api](https://github.com/Zhengyuuuui/qoder2api)
  `8e02f24..2671a21`，含新版推理协议（Sonus/Cantus/Fast）、Token 自动续期、
  `RESOURCE_LIMIT` 友好错误与注册研究文档。插件侧已把 `checkin_fingerprint`
  接入 `NewBridgeWithFP`，并在配置指纹时补充 Sonus/Cantus/Fast 的静态回退清单。
- 2026-10-20：合并 `4789816..8e02f24`，含专属资源包（活动积分）额度解析、
  账号签到灰度差异与模型清单快照。细节见 [docs/upstream-notes.md](docs/upstream-notes.md)。
- 2026-10-20：回退模型清单同步到上游快照（`mmodel` 更名、价格/推理标记/上下文），
  并把 CLIProxyAPI SDK 从 v8.0.32 升到 v8.0.35（与当前宿主一致）。
