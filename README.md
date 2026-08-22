# CLIProxyAPI Provider Rate Limiter

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

## Build

```bash
cd go
go test ./...
go build -buildmode=c-shared -o /tmp/provider-rate-limiter.so .
```

Copy the `.so` into the CPA plugin directory for the target architecture, then restart or reload plugins according to the CPA deployment. This plugin is currently single-process and stores windows in memory; use a host-level single CPA instance or add shared storage before running multiple CPA replicas.

For unreleased CPA SDK changes, add a temporary local `replace` in `go/go.mod`, then remove it before publishing.
