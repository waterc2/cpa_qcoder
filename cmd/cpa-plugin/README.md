# qoder2api CPA plugin

> **构建 / 模块来源 / 路线图 / 上游与官方更新手册** 见仓库根目录的
> [`CPA-PLUGIN.md`](../../CPA-PLUGIN.md)。

CLIProxyAPI dynamic plugin that exposes [Qoder](https://qoder.com) as a native
CPA provider. It is a thin CPA-native adapter over the `qoder2api` bridge core.

Accounts are stored as ordinary CPA auth records (one Qoder account per auth
file), so CPA's built-in scheduler, auth UI, model aliasing and usage accounting
all apply. The plugin only adds what CPA does not already provide:

| Capability | What it does |
|---|---|
| `AuthProvider` | OAuth device login, PAT import, token refresh |
| `ModelProvider` | Qoder model catalog (static + per-auth dynamic) |
| `Executor` | OpenAI chat-completions → Qoder upstream (stream + non-stream) |
| `ManagementAPI` | Account list, quota, daily check-in, import/delete, model selection, panel |

Everything else (routing, retries, session affinity, usage stats, auth UI) comes
from CPA.

## Build

```bash
scripts/build-cpa-plugin.sh 0.1.0
# -> dist/qoder-v0.1.0.so  (+ .h)
```

Requires Go with cgo and a C toolchain. The plugin is built against
`github.com/router-for-me/CLIProxyAPI/v8`.

## Install

```bash
cp dist/qoder-v0.1.0.so /path/to/cli-proxy-api/plugins/linux/amd64/
```

Add the plugin to `config.yaml`:

```yaml
plugins:
    enabled: true
    dir: "plugins"
    configs:
        qoder:
            enabled: true
            # optional
            checkin_auto: false
            checkin_times: ["10:00"]
            checkin_on_start: false
            default_region: global
```

Restart CPA. The panel is available at
`/v0/resource/plugins/qoder/panel` and the management API under
`/v0/management/plugins/qoder/*`.

## Adding accounts

### OAuth (recommended)

In the CPA management UI use **Add auth → Qoder**. The plugin drives Qoder's
device login flow: it returns a login URL, the user authorizes in the browser,
and the host polls until the token arrives. The resulting account is persisted
as a normal CPA auth file.

### PAT import

Create an auth file (or import via the panel) in one of these shapes:

```json
{ "type": "qoder", "secret": "pt-xxxxxxxx", "region": "global" }
```

```json
{ "type": "qoder", "secret": "pt-xxxxxxxx", "region": "cn" }
```

OAuth credentials can also be imported directly:

```json
{ "type": "qoder", "device_token": "dt-xxxx", "refresh_token": "drt-xxxx", "region": "global" }
```

The credential is validated against Qoder before it is saved (invalid tokens are
rejected). `region` defaults to `global`; use `cn` for Qoder China accounts.

### Management API

| Method | Path | Description |
|---|---|---|
| GET | `/v0/management/plugins/qoder/accounts` | List accounts (region, plan, check-in streak) |
| POST | `/v0/management/plugins/qoder/checkin` | Check in one (`account_id`) or all accounts |
| GET | `/v0/management/plugins/qoder/account/quota?account_id=…` | Live quota for one account |
| POST | `/v0/management/plugins/qoder/import` | Import `{secret, region, name}` |
| POST | `/v0/management/plugins/qoder/accounts/delete` | Delete `{account_id}` |
| GET/POST | `/v0/management/plugins/qoder/checkin/config` | Get/update auto check-in config |
| GET | `/v0/management/plugins/qoder/models/config` | List selectable models and their enabled state |
| POST | `/v0/management/plugins/qoder/models/config` | Save model selection: `{"disabled": ["modelId", …]}` |

All routes require CPA's management key (`Authorization: Bearer <key>`).

## Model selection

The panel has a **模型管理** (Model management) button that opens a checkbox grid
of every model Qoder currently offers (static catalog merged with each account's
live `/model/list`). Unchecking a model hides it from `/v1/models` and removes it
from the routable set — the same idea as CodeBuddy's model picker.

Selection is stored as a denylist in `<data-dir>/models.json`
(`{"disabled": ["modelId", …]}`). Because it is a denylist, newly discovered
upstream models are exposed by default; only explicitly unchecked models are
hidden. Model ids are matched case-insensitively. This filter is applied on top
of the host's own `oauth-excluded-models`, so both work together.

## Check-in

Qoder's daily check-in is the `campaigns` flow. The plugin supports:

- Manual per-account and all-account check-in from the panel/API.
- Optional scheduled check-in (`checkin_auto` + `checkin_times`, local time).
- Local streak/total history (the upstream legacy `daily-check-in` endpoint is
  disabled, so streaks are computed locally).

Global accounts may require an official client device fingerprint
(`checkin_fingerprint` in the auth file) before the campaign is offered; CN
accounts must **not** send a self-made fingerprint. See the qoder2api check-in
notes for details.

## Layout

```
cmd/cpa-plugin/
  main.go        C ABI + RPC dispatch + registration + config
  auth.go        credential parsing, auth.parse/login/refresh
  refresh.go     device-token refresh (/api/v1/deviceToken/refresh)
  settings.go    panel config persistence (plugin_settings.json)
  modelstate.go  model enable/disable denylist (models.json)
  models.go      model.static / model.for_auth
  executor.go    execute / execute_stream + host stream bridge
  checkin.go     check-in core + scheduler
  management.go  management routes + host auth RPC
  panel.go/.html embedded dashboard
  baseprompt.json Qoder upstream request template
```

The plugin reuses `qoder2api/account`, `qoder2api/internal/cosy` and
`qoder2api/internal/bridge`; the standalone gateway and the plugin share the
same upstream implementation.
