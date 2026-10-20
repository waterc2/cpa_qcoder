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
- 内嵌管理面板

## 构建

需要 Go（cgo）和 C 工具链：

```bash
scripts/build-cpa-plugin.sh 0.1.0
# -> dist/qoder-v0.1.0.so (+ .h)
```

## 安装

```bash
cp dist/qoder-v0.1.0.so /path/to/cli-proxy-api/plugins/linux/amd64/
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

- 2026-10-20：合并 [qoder2api](https://github.com/Zhengyuuuui/qoder2api)
  `4789816..8e02f24`，含专属资源包（活动积分）额度解析、账号签到灰度差异与
  模型清单快照。细节见 [docs/upstream-notes.md](docs/upstream-notes.md)。
