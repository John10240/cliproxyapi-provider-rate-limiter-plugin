# CLIProxyAPI Provider Rate Limiter

English | [简体中文](README.zh-CN.md)

CLIProxyAPI dynamic plugin implementing the official `scheduler` capability. It limits each candidate independently before CPA selects an auth record; no `X-Provider` header, extra port, or path is required.

## Configuration

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

Limit precedence is `auths.<auth_id>` > `providers.<provider>` > `default_rpm`. Each candidate has its own sliding one-minute window. A value of `0` disables the limit at that level. When every candidate is over limit, the plugin returns a scheduler error instead of allowing CPA to fall through to an over-limit candidate.

`auths` is the right setting when several Codex accounts share the `codex` provider and must have different limits. The plugin counts an admission when it returns an `AuthID`; this is request RPM, including failed upstream attempts.

The `providers` value is applied independently to each candidate belonging to that provider; it is not an aggregate cap across all accounts. Invalid negative limits are rejected during plugin configuration. When all candidates are exhausted, CPA receives HTTP 429 with a retryable plugin error.

## Management menu

The plugin registers a CPA management menu named **Provider Rate Limiter**. Open the sidebar menu, enter the CPA management key, and load the real accounts from `/v0/management/auth-files`. The page shows one editable RPM field per AuthID and a separate Provider-default table, so account IDs do not need to be copied into JSON by hand. The menu's `PUT /v0/management/plugins/provider-rate-limiter/settings` update changes the running plugin immediately; persist the same values under `plugins.configs.provider-rate-limiter` in `config.yaml` before restarting CPA, because the official plugin ABI does not provide a host configuration-write callback.

## Build

```bash
cd go
go test ./...
go build -buildmode=c-shared -o /tmp/provider-rate-limiter.so .
```

Copy the `.so` into the CPA plugin directory for the target architecture, then restart or reload plugins according to the CPA deployment. This plugin is currently single-process and stores windows in memory; use a host-level single CPA instance or add shared storage before running multiple CPA replicas.

## Local CPA source development

For unreleased CPA SDK changes, add a temporary local `replace` in `go/go.mod`, then remove it before publishing:

```text
replace github.com/router-for-me/CLIProxyAPI/v7 => ../cliproxyapi-fork
```

## GitHub Actions

The repository CI runs race tests, static checks, and dynamic-library builds on Linux and macOS.

## Custom CPA plugin-store source

This repository includes `registry.json` for use as a third-party CPA plugin-store source. Add the raw URL below to the CPA configuration under `plugins.store-sources`:

```yaml
plugins:
  store-sources:
    - https://raw.githubusercontent.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin/main/registry.json
```
