# CLIProxyAPI-Extended

> Fork of [CLIProxyAPIPlus](https://github.com/router-for-me/CLIProxyAPIPlus) with advanced Canonical IR architecture, full Ollama compatibility, and Cline integration

[![Original Repo](https://img.shields.io/badge/Original-router--for--me%2FCLIProxyAPI-blue)](https://github.com/router-for-me/CLIProxyAPI)
[![Plus Version](https://img.shields.io/badge/Plus-router--for--me%2FCLIProxyAPIPlus-green)](https://github.com/router-for-me/CLIProxyAPIPlus)

## Why This Fork?

This fork pioneered the **Canonical IR architecture** before the official Plus version. As of January 28, 2026, it synchronizes with [CLIProxyAPIPlus](https://github.com/router-for-me/CLIProxyAPIPlus) while maintaining unique improvements:

| Feature | Description |
|---------|-------------|
| **Full Ollama Compatibility** | Complete bidirectional protocol support (`/api/chat`, `/api/generate`) with streaming — use any provider through Ollama API |
| **Cline Integration** | Free models support (MiniMax M2, Grok Code Fast 1) |
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

**Note:** Ollama API and Cline require `use-canonical-translator: true`

## Architecture

**Hub-and-spoke** with unified Intermediate Representation (IR):

```
    OpenAI ─────┐                       ┌───── OpenAI
    Claude ─────┤                       ├───── Claude
    Ollama ─────┼─────► Canonical ◄─────┼───── Gemini (AI Studio)
      Kiro ─────┤       IR              ├───── Gemini CLI
     Cline ─────┤                       ├───── Antigravity
   Copilot ─────┘                       ├───── Ollama
                                        └───── Cline
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
Provider (Gemini/Claude/OpenAI/Cline/etc.)
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
| **Cline**     | ✅ (via OpenAI)      | ✅ (via OpenAI)      | ✅ Tested |
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

### Cline
- Long-lived refresh token → short-lived JWT (~10 minutes)
- JWT used with `workos:` prefix for API requests
- **Note:** Obtaining refresh token requires Cline extension source modification



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

Simple bug fixes with ready-to-merge code welcome. For larger changes, consider forking for full control.

---

## Ecosystem

Projects based on CLIProxyAPI:

- **[vibeproxy](https://github.com/automazeio/vibeproxy)** — macOS menu bar app for Claude Code & ChatGPT with AI coding tools
- **[Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator)** — browser-based SRT translator via Gemini
- **[CCS (Claude Code Switch)](https://github.com/kaitranntt/ccs)** — CLI for instant account/model switching
- **[ProxyPal](https://github.com/heyhuynhgiabuu/proxypal)** — macOS GUI for CLIProxyAPI management
- **[Quotio](https://github.com/nguyenphutrong/quotio)** — macOS menu bar with unified quota tracking & auto-failover
- **[CodMate](https://github.com/loocor/CodMate)** — macOS SwiftUI app for CLI AI session management
- **[ProxyPilot](https://github.com/Finesssee/ProxyPilot)** — Windows-native fork with TUI & system tray
- **[Claude Proxy VSCode](https://github.com/uzhao/claude-proxy-vscode)** — VSCode extension for Claude Code model switching

**Ports & Inspired Projects:**

- **[9Router](https://github.com/decolua/9router)** — Next.js implementation with web dashboard & auto-fallback

> Open a PR to add your project to this list.

---

## License

MIT License - see [LICENSE](LICENSE) file.

**Original project:** [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
