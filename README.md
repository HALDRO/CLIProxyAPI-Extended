# CLIProxyAPI-Extended

> Fork of [CLIProxyAPIPlus](https://github.com/router-for-me/CLIProxyAPIPlus) with advanced Canonical IR architecture and full Ollama compatibility.

[![Original Repo](https://img.shields.io/badge/Original-router--for--me%2FCLIProxyAPI-blue)](https://github.com/router-for-me/CLIProxyAPI)
[![Plus Version](https://img.shields.io/badge/Plus-router--for--me%2FCLIProxyAPIPlus-green)](https://github.com/router-for-me/CLIProxyAPIPlus)

## Why This Fork?

This fork pioneered the **Canonical IR architecture** before the official Plus version. As of January 28, 2026, it synchronizes with [CLIProxyAPIPlus](https://github.com/router-for-me/CLIProxyAPIPlus) while maintaining unique improvements:

| Feature | Description |
|---------|-------------|
| **Full Ollama Compatibility** | Complete bidirectional protocol support (`/api/chat`, `/api/generate`) with streaming — use any provider through Ollama API |
| **KiloCode Integration** | Built-in KiloCode provider with free models support (via Kilo gateway) and full device-auth login flow |
| **Enhanced Stability** | Improved compatibility with Cursor, Copilot Chat, and other AI coding clients |
| **Advanced Architecture** | Refined Canonical IR implementation with 54% codebase reduction (17,464 → 7,992 lines) |

**Canonical IR benefits:**
- Hub-and-spoke model eliminates N×M translation paths
- Type-safe `UnifiedChatRequest` with compile-time guarantees
- Single `UnifiedEvent` type for SSE/NDJSON/binary protocols
- Zero-allocation `gjson`-based parsers
- 54% codebase reduction (17,464 → 7,992 lines)

---

## Quick Start

New features are **enabled by default**:

```yaml
use-canonical-translator: true   # Canonical IR architecture (default)
show-provider-prefixes: true     # Visual provider prefixes (default)
```

**Provider prefixes:** Visual identification in model list (e.g., `[Gemini CLI] gemini-2.5-flash`). Purely cosmetic — models work with or without prefix.

**Provider selection:** Without prefix (or with prefixes disabled), system uses **round-robin** for load balancing.

**Note:** Ollama API requires `use-canonical-translator: true`

## Architecture

**Hub-and-spoke** with unified Intermediate Representation (IR):

```
    OpenAI ─────┐                       ┌───── OpenAI
    Claude ─────┤                       ├───── Claude
    Ollama ─────┼─────► Canonical ◄─────┼───── Gemini (AI Studio)
      Kiro ─────┤       IR              ├───── Gemini CLI
   Copilot ─────┘                       ├───── Antigravity
                                        └───── Ollama
```

**Result:** 21 files (7 parsers + 7 emitters + 6 IR core + 1 adapter), 7,992 lines

| Metric                    | Legacy        | Canonical IR  | Δ         |
|---------------------------|---------------|---------------|-----------|
| Files                     | 99            | 21            | **−79%**  |
| Lines of code             | 17,464        | 7,992         | **−54%**  |
| Translation paths         | N×M           | 2N (hub)      | **−48%**  |

---

## Ollama Compatibility

The proxy acts as a **full Ollama-compatible server** — clients can use any provider through Ollama API:

```
Ollama client (/api/chat, /api/generate)
    ↓ parse directly to IR (no OpenAI conversion)
Canonical IR
    ↓ convert to provider format
Provider (Gemini/Claude/OpenAI/etc.)
    ↓ response through IR
Ollama response (streaming/non-streaming)
```

**Recommended:** Run on port `11434` for maximum client compatibility.

**Use case:** IDEs with Ollama support but without OpenAI API (e.g., some Copilot Chat configurations).

## Provider Support

| Provider      | Input (to_ir)        | Output (from_ir)     | Status |
|---------------|:--------------------:|:--------------------:|:------:|
| OpenAI        | ✅ Req/Resp/Stream   | ✅ Req/Resp/Stream   | ✅ Tested |
| Claude        | ✅ Req/Resp/Stream   | ✅ Req/Resp/SSE      | ✅ Tested |
| Gemini        | ✅ Resp/Stream       | ✅ Req/Resp/Stream   | ✅ Tested |
| Gemini CLI    | ✅ (shared)          | ✅ CLI format        | ✅ Tested |
| Antigravity   | ✅ Req/Resp          | ✅ v1internal        | ✅ Tested |
| **Ollama**    | ✅ Req/Resp/Stream   | ✅ Req/Resp/Stream   | ✅ Tested |
| **KiloCode**  | ✅ (via OpenAI)      | ✅ (via OpenAI)      | ✅ Tested |
| Kiro          | ✅ Resp/Stream       | ✅ Req               | ✅ Tested |
| Codex         | ✅ Req/Resp          | ✅ Responses API     | ✅ Tested |
| Copilot       | ✅ (via OpenAI)      | ✅ (via OpenAI)      | ✅ Tested |
| Qwen          | ❌                   | ❌                   | ⚠️ Migration needed |
| iFlow         | ❌                   | ❌                   | ⚠️ Migration needed |

**Key Features:**
- Reasoning/Thinking blocks with `reasoning_tokens` tracking
- Tool calls with unified ID generation
- Multimodal support (images, PDF, inline data)
- Streaming: SSE (OpenAI/Claude), NDJSON (Gemini/Ollama)
- Responses API (`/v1/responses`)

**Known Issues:**
- Antigravity GPT-OSS: thinking mode disabled (infinite planning loops)
- CLI agents (Aider, etc.): not tested

## Authentication

### KiloCode
KiloCode is supported as a first-class provider with a built-in device-auth login flow (no API key required for free-tier models).

### Other Providers
Full OAuth2 flows with auto browser opening (Gemini, Claude, Codex, GitHub Copilot, etc.) — see [original documentation](https://help.router-for.me/)

---

## Getting Started

- **Guides:** [https://help.router-for.me/](https://help.router-for.me/)
- **Management API:** [MANAGEMENT_API.md](https://help.router-for.me/management/api)
- **Amp CLI Integration:** [Complete Guide](https://help.router-for.me/agent-client/amp-cli.html)
- **SDK Documentation:**
  - [Usage](docs/sdk-usage.md) | [Advanced](docs/sdk-advanced.md) | [Access](docs/sdk-access.md) | [Watcher](docs/sdk-watcher.md)
  - [Custom Provider Example](examples/custom-provider)

---

## Original Features

From [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI):

- OpenAI/Gemini/Claude compatible endpoints
- OAuth login (Codex, Claude Code, Qwen, iFlow, GitHub Copilot)
- Streaming/non-streaming, function calling, multimodal
- Multiple accounts with round-robin
- Reusable Go SDK

---

## 📋 Contributing

**Experimental fork** — sharing for community use.

- **Cherry-pick freely** — take features/fixes useful for your projects
- **Limited maintenance** — time constraints on extensive reviews
- **Clear solutions only** — provide specific fixes or clear reproduction steps



## Ecosystem






Browser-based tool to translate SRT subtitles using your Gemini subscription via CLIProxyAPI with automatic validation/error correction - no API keys needed

### [CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)

CLI wrapper for instant switching between multiple Claude accounts and alternative models (Gemini, Codex, Antigravity) via CLIProxyAPI OAuth - no API keys needed

### [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)

Native macOS GUI for managing CLIProxyAPI: configure providers, model mappings, and endpoints via OAuth - no API keys needed.

### [Quotio](https://github.com/nguyenphutrong/quotio)

Native macOS menu bar app that unifies Claude, Gemini, OpenAI, Qwen, and Antigravity subscriptions with real-time quota tracking and smart auto-failover for AI coding tools like Claude Code, OpenCode, and Droid - no API keys needed.

### [CodMate](https://github.com/loocor/CodMate)

Native macOS SwiftUI app for managing CLI AI sessions (Codex, Claude Code, Gemini CLI) with unified provider management, Git review, project organization, global search, and terminal integration. Integrates CLIProxyAPI to provide OAuth authentication for Codex, Claude, Gemini, Antigravity, and Qwen Code, with built-in and third-party provider rerouting through a single proxy endpoint - no API keys needed for OAuth providers.

### [ProxyPilot](https://github.com/Finesssee/ProxyPilot)

Windows-native CLIProxyAPI fork with TUI, system tray, and multi-provider OAuth for AI coding tools - no API keys needed.

### [Claude Proxy VSCode](https://github.com/uzhao/claude-proxy-vscode)

VSCode extension for quick switching between Claude Code models, featuring integrated CLIProxyAPI as its backend with automatic background lifecycle management.

### [ZeroLimit](https://github.com/0xtbug/zero-limit)

Windows desktop app built with Tauri + React for monitoring AI coding assistant quotas via CLIProxyAPI. Track usage across Gemini, Claude, OpenAI Codex, and Antigravity accounts with real-time dashboard, system tray integration, and one-click proxy control - no API keys needed.

### [CPA-XXX Panel](https://github.com/ferretgeek/CPA-X)

A lightweight web admin panel for CLIProxyAPI with health checks, resource monitoring, real-time logs, auto-update, request statistics and pricing display. Supports one-click installation and systemd service.

> [!NOTE]  
> If you developed a project based on CLIProxyAPI, please open a PR to add it to this list.

## More choices

Those projects are ports of CLIProxyAPI or inspired by it:

### [9Router](https://github.com/decolua/9router)

A Next.js implementation inspired by CLIProxyAPI, easy to install and use, built from scratch with format translation (OpenAI/Claude/Gemini/Ollama), combo system with auto-fallback, multi-account management with exponential backoff, a Next.js web dashboard, and support for CLI tools (Cursor, Claude Code, Cline, RooCode) - no API keys needed.

> [!NOTE]  
> If you have developed a port of CLIProxyAPI or a project inspired by it, please open a PR to add it to this list.

## License

MIT License - see [LICENSE](LICENSE) file.

**Original project:** [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
