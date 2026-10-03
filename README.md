# CLIProxyAPI Vertex AI (ADC) plugin

## Description

A native Go plugin that connects CLIProxyAPI to Google Vertex AI using Application Default Credentials (ADC).
It runs inside the host, not as a separate HTTP proxy.
Supported platforms are Linux and macOS on AMD64/ARM64, FreeBSD on AMD64, and Windows on AMD64.

Authentication requires no service account and no JSON key.
The plugin uses the ambient ADC token from `gcloud auth application-default login`, `GOOGLE_APPLICATION_CREDENTIALS`, or a GCE/workload metadata server.
Because ADC tokens are not project-bound, every request also sends `x-goog-user-project` so Vertex can attribute service usage.

This is an independent integration, not an official Google product.
Vertex AI quotas, Model Garden enablement, and billing still apply.

## Features

- ADC authentication with automatic token refresh; no service-account key needed.
- Auto-discovery of accessible Claude (`publishers/anthropic`) and Gemini (`publishers/google`) models.
- Full request/response/stream translation between OpenAI, OpenAI-Responses, Anthropic Messages, and Gemini formats.
- Publisher-aware routing: Claude via `:rawPredict`/`:streamRawPredict`, Gemini via `:generateContent`/`:streamGenerateContent`.
- Configurable model prefix, publisher selection, allowlist, aliases, and exclusions.
- `global` and regional locations, plus an optional endpoint override for private service endpoints.
- An embedded dashboard for checking ADC status and refreshing the model catalog.
- Native library validation, isolated host integration tests, and plugin-store ZIP packaging.

## Build

Use Go 1.27.1 or newer, a working C compiler, and GNU Make (`gmake` on FreeBSD).
The plugin requires a plugin-enabled CLIProxyAPI host; v8.0.12 is the integration-test baseline.

```sh
make check
make build
```

A local build produces `dist/<goos>/<goarch>/vertex-adc.so`, `vertex-adc.dylib` on macOS, or `vertex-adc.dll` on Windows.
The build checks the library's architecture, native format, OS ABI, and exported entrypoint.
The host and library must match in OS, architecture, and C runtime.
Linux release builds use Ubuntu 26.04; build on your deployment distribution if you need a different libc baseline, including Alpine/musl.

Local builds use `UNCONFIGURED` repository metadata.
Supply your real repository URL for a distributable build:

```sh
make build VERSION=0.1.0 REPOSITORY=https://github.com/YOUR_ACCOUNT/YOUR_REPOSITORY
```

### FreeBSD cross-build

On Linux with Clang, LLD, and the script's download/extraction tools installed:

```sh
make freebsd
```

This uses a checksum-pinned FreeBSD 14.4 sysroot and produces `dist/freebsd/amd64/vertex-adc.so`.
It cross-compiles libraries and tests without running a FreeBSD VM or executing the FreeBSD tests.

### Integration tests

Supply an existing plugin-enabled CLIProxyAPI binary matching your platform:

```sh
make integration CPA_BINARY=/absolute/path/to/cli-proxy-api
```

CI runs build and unit checks; real-host integration tests must be run separately with `CPA_BINARY`.
The integration test is fully offline: it mocks the Vertex endpoints and uses a synthetic service-account key whose token endpoint points at the mock, so no real Google Cloud access is needed.
Additional dependency checks are available through `make audit`.

## Install

1. Stop CLIProxyAPI and back up its configuration.
2. Copy the built library into `<plugins-dir>/<goos>/<goarch>/`, keeping its original filename (`vertex-adc.<ext>`).
3. Ensure ADC is available to the host process (for example, run `gcloud auth application-default login` as the same user, or run on a GCE/workload identity).
4. Merge the configuration below into the existing host configuration.
5. Restart CLIProxyAPI and open **Vertex AI (ADC)** from its plugin management menu.

Keep existing API keys, management authentication, providers, and credentials.

## Configure settings

### Basic configuration

```yaml
plugins:
  enabled: true
  dir: ./plugins
  configs:
    vertex-adc:
      enabled: true
      project_id: gcp-project-id
      location: global
      publishers: [anthropic, google]
      model_prefix: vertex
      models: []
      models_excluded: []
      model_cache_ttl_seconds: 300
```

See [examples/config.yaml](examples/config.yaml) for the complete configuration.

### Model namespace and discovery

Public model IDs are `<model_prefix>/<publisher>/<upstream-id>`, for example `vertex/anthropic/claude-sonnet-4` or `vertex/google/gemini-2.5-pro`.
With an empty `models` list the plugin auto-discovers every accessible Claude and Gemini model from the Vertex `publishers/*/models` catalog.
Discovery lists the Model Garden catalog rather than per-project entitlements, so use `models` or `models_excluded` to prune to what your project can actually call.

| Setting | Default | Purpose |
| --- | --- | --- |
| `project_id` | ADC project | Google Cloud project used in the Vertex request path |
| `location` | `global` | `global` or a region such as `us-east5` |
| `quota_project_id` | ADC / project | Project for the `x-goog-user-project` header |
| `credentials` | standard ADC | Optional `file:/abs/path` to an ADC JSON file |
| `publishers` | `[anthropic, google]` | Publishers to surface |
| `model_prefix` | `vertex` | Public namespace prefix |
| `models` | `[]` | Optional `{name, alias}` allowlist; names are publisher-qualified |
| `models_excluded` | `[]` | Case-insensitive model ID prefixes to exclude |
| `model_cache_ttl_seconds` | `300` | Catalog cache lifetime, from 30 to 3600 seconds |
| `api_endpoint` | derived | Optional Vertex API host override |
| `proxy_url` | none | Optional proxy for the ADC token exchange |

```yaml
models:
  - name: anthropic/claude-sonnet-4@20250514
  - name: google/gemini-2.5-pro
    alias: gem-pro
```

Without an alias, the public ID is `<model_prefix>/<publisher>/<upstream-id>`.
An explicit alias replaces the entire public ID and must be unique.

### Discovery vs. explicit models (important)

Auto-discovery calls the Vertex Model Garden catalog (`publishers/*/models`), which has no project in its URL path and therefore relies entirely on the quota project (`x-goog-user-project`).
That quota project must have the Vertex AI API enabled and the caller must hold `serviceusage.services.use` on it, or discovery fails (the dashboard now shows the exact Google error, e.g. `PERMISSION_DENIED`/`SERVICE_DISABLED`).

Inference does not have this constraint: the model endpoint carries the project in its path, so it works as long as the credentials can call that project's models.

If discovery is not permitted in your environment, set an explicit `models` allowlist.
The plugin then serves those models directly and skips the catalog call entirely:

```yaml
models:
  - name: google/gemini-3.8-flash
  - name: anthropic/claude-opus-4-8@default
```

Note that Claude models are region-specific on Vertex; if a model returns 404 at `global`, set `location` to a region where it is enabled (and ensure that regional host is reachable from your network).

### Using with Claude Code on Vertex

Point Claude Code at CLIProxyAPI and select a discovered Claude model, for example `vertex/anthropic/claude-sonnet-4`.
The plugin accepts native Anthropic Messages requests, strips the body `model`, injects `anthropic_version: vertex-2023-10-16`, and forwards to the Vertex `:rawPredict`/`:streamRawPredict` surface.

### Status and refresh

Open `/v0/resource/plugins/vertex-adc/dashboard`, enter the host management key, and select **Check status**.
The dashboard reports credential readiness, the resolved project and quota project, the location, and the discovered model count, and can force a catalog **Refresh**.
It never displays tokens or credential contents, and does not persist the management key in browser storage, URLs, or cookies.

### Security and API limits

Endpoint and credential overrides are trusted operator settings, not values to accept from user prompts.
HTTPS is required for `api_endpoint`; `allow_insecure_base_url` permits loopback HTTP only for tests.
The ADC token exchange and discovery run through the configured `proxy_url` and the host transport; inference retains the host's request transport context.
Protect the management API and disable request logging for credential exchanges.
Native plugins run with the host's privileges.

Supported client routes are `POST /v1/chat/completions`, `POST /v1/responses`, and `POST /v1/messages`.
Same-protocol requests preserve native fields; cross-protocol conversion cannot preserve every provider-specific feature.
Non-generative Vertex families (embeddings, image, video) and generic HTTP forwarding are not supported; token counting is not proxied.
Incomplete streams fail, and delivered streams are never replayed by the plugin.
Set host `request-retry: 0` if you also need to disable host-level retries.
