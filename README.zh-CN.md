# CLIProxyAPI Provider Rate Limiter

[English](README.md) | 简体中文

这是一个独立的 CLIProxyAPI 动态插件，通过 CPA 官方 `scheduler` 能力在账号选择前执行限流。它不需要 `X-Provider` 请求头，也不需要额外端口或路径。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    provider-rate-limiter:
      enabled: true
      priority: 100
      default_rpm: 60
      providers:
        codex: 60
        anthropic: 30
      auths:
        codex-account-01: 120
        codex-account-02: 90
```

限制优先级为：

```text
auths.<auth_id> > providers.<provider> > default_rpm
```

每个候选账号都有独立的滑动一分钟窗口。值为 `0` 表示该层级不限流。当所有候选账号都达到限制时，插件会返回调度错误，不会让 CPA fallback 到已经超限的账号。

如果多个 Codex 账号共享同一个 `codex` Provider，但需要不同的 RPM，应使用 `auths.<auth_id>`。插件在返回一个 `AuthID`、即请求被调度准入时计数；后续上游请求失败也会计入，这是请求 RPM 的语义。

`providers.<provider>` 是该 Provider 下每个候选账号分别使用的限制，不是所有账号合计的总限制。负数 RPM 配置会在插件配置阶段被拒绝。当所有候选都耗尽时，CPA 会收到 HTTP `429` 和可重试的插件错误。

## 管理菜单

插件会向 CPA 注册 **Provider Rate Limiter** 管理菜单。请打开侧边栏菜单并输入 CPA 管理密钥，页面会从 `/v0/management/auth-files` 读取真实账号列表，按 AuthID 为每个账号提供独立的 RPM 输入框，同时提供按 Provider 设置默认限制的表格，不再需要手工填写 `auths` JSON。保存时会同时调用 CPA 的 `/v0/management/plugins/provider-rate-limiter/config` 配置接口和插件运行时设置接口，因此修改立即生效，重启 CPA 后也会保留。

## 构建

```bash
cd go
go test ./...
go build -buildmode=c-shared -o /tmp/provider-rate-limiter.so .
```

将生成的动态库复制到目标架构对应的 CPA 插件目录，然后按照 CPA 的插件重载机制重启或重新加载。当前插件将窗口保存在单个进程内存中；如果运行多个 CPA 副本，需要先接入 Redis 等共享存储。

## 本地 CPA 源码开发

如果需要使用尚未发布的 CPA SDK 变更，可以临时在 `go/go.mod` 添加：

```text
replace github.com/router-for-me/CLIProxyAPI/v7 => ../cliproxyapi-fork
```

发布版本前请删除这条 `replace`。

## GitHub Actions

仓库的 CI 会在 Linux 和 macOS 上执行 race 测试、静态检查和动态库构建。

## 自定义 CPA 插件商店源

仓库包含可直接作为第三方 CPA 插件商店源使用的 `registry.json`。在 CPA 配置的 `plugins.store-sources` 下添加：

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin/main/registry.json
```
