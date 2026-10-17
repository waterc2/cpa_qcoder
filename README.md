# cpa_qcoder

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **Qoder** 插件。
把 [Qoder](https://qoder.com) 作为 CPA 原生 provider 接入：账号、路由、调度、
用量统计、认证 UI 全部复用 CPA，插件只补 Qoder 特有的部分。

基于 [qoder2api](https://github.com/Zhengyuuuui/qoder2api) 的 bridge 核心二次开发。

## 功能

- OAuth 设备登录、PAT 导入，设备令牌自动刷新
- 模型列表（静态 + 按账号动态）、模型开关
- OpenAI `chat/completions`（流式 / 非流式）
- 额度查询、每日签到（手动 + 定时）
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
```

重启 CPA。面板地址：`/v0/resource/plugins/qoder/panel`。

## 文档

- [`cmd/cpa-plugin/README.md`](cmd/cpa-plugin/README.md) — 安装、账号、管理 API、模型开关、签到
- [`CPA-PLUGIN.md`](CPA-PLUGIN.md) — 构建、上游补丁、更新流程、排障
