<p align="center">
  <img src="docs/banner.svg" alt="Tutor MCP — Self-learning is a superpower." width="100%" />
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT license" /></a>
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-server-7c3aed.svg" alt="MCP server" /></a>
  <a href="https://github.com/ArnaudGuiovanna/tutor-mcp/releases"><img src="https://img.shields.io/badge/status-alpha-yellow.svg" alt="Alpha" /></a>
</p>

# Tutor MCP

**Your personal learning academy: any subject, a complete learning path, an adaptive AI tutor.**

[Description](#description) · [Demo](#demo) · [Installation](#installation) · [Quickstart](#quickstart) · [How it works](#how-it-works) · [Documentation](#documentation) · [Creator](#creator) · [License](#license)

## Description

**Tutor MCP is an open-source MCP engine for adaptive learning, self-learning and personalized AI tutoring.** Built on the Model Context Protocol, it brings course creation, a structured learning path and ongoing tutoring into your AI assistant. Choose a subject and a goal — from conversational Spanish to backend engineering — and build a course that evolves with your progress.

- **Build a curriculum on any subject.** Your AI maps the subject into a skill graph with concepts, prerequisites and goals. Tutor validates and versions that graph as your course develops.
- **Follow a guided learning journey.** Diagnose your starting point, work through personalized lessons and exercises, revisit weak areas, and test your ability to apply what you learn.
- **Get ongoing, personalized guidance.** Knowledge estimates, review dates, misconceptions and session memory shape the next activity, across conversations.
- **See the evidence behind your progress.** Inspect why an activity was recommended and distinguish estimated knowledge, retained learning and demonstrated skills.

**Generative teaching. Deterministic pedagogy.** This is Tutor's defining engineering choice: the AI creates and explains; a persistent, auditable engine governs progression. Bayesian Knowledge Tracing (BKT), FSRS spaced repetition, prerequisite rules and assessment evidence drive what to learn, practice or revisit next. Narrative memory gives the tutor the context to make that guidance personal.

**Your learning engine travels across AI clients.** Use Claude Code, Claude Desktop, ChatGPT, Hermes, Pi (with an MCP extension), Gemini CLI, Le Chat and other compatible MCP clients. Keep the same learning history by connecting to the same Tutor installation, locally over stdio or remotely over HTTPS/OAuth. See the [client guide](docs/clients.md).

<p align="left">
  <a href="docs/clients.md"><img src="docs/assets/logos/claude.svg" width="36" height="36" alt="Claude Code and Claude Desktop" title="Claude" /></a>
  &nbsp;&nbsp;
  <a href="docs/clients.md"><img src="docs/assets/logos/openai.svg" width="36" height="36" alt="ChatGPT" title="ChatGPT" /></a>
  &nbsp;&nbsp;
  <a href="docs/clients.md"><img src="docs/assets/logos/gemini.svg" width="36" height="36" alt="Gemini CLI" title="Gemini" /></a>
  &nbsp;&nbsp;
  <a href="docs/clients.md"><img src="docs/assets/logos/mistral.svg" width="36" height="36" alt="Le Chat by Mistral AI" title="Le Chat" /></a>
</p>

## Demo

<p align="center">
  <img src="docs/assets/demo.gif" alt="A real Tutor MCP session in Claude Code: a goal turned into a scored curriculum, a cold diagnostic graded against a frozen rubric, and a session closed with its summary written to learner memory" width="100%" />
</p>

A real first session, running locally over stdio in Claude Code. Tutor maps Go backend development
into an 18-concept skill graph, diagnoses what transfers from the learner's Python background, then
refuses the Feynman and transfer probes because the mastery estimate sits at 0.33 against a routing
threshold of 0.85. Two practice reps later the estimate clears the threshold, the transfer probe runs
on an unseen debugging scenario, and mastery is recorded as transfer-verified rather than asserted.

## Installation

Choose where your learning data lives. Every profile uses the same learning engine.

| Profile | Best for | Setup |
|---|---|---|
| **Local** | Learning on your own computer | One binary + `--local`. Your MCP client starts it; SQLite stores your history. No account or server setup. [Local setup](docs/installation.md#local-client-configuration) |
| **Hobby** | A personal VPS or a small group | `--profile hobby`, SQLite and an HTTPS proxy. Invite users over SSH; they sign in with a username and password. [Native installation](docs/installation.md#native-hobby-vps-binary-systemd-and-caddy) · [Docker Compose](docs/installation.md#hobby-vps-with-docker-compose) |
| **Institution** | An organization running a shared service | `--profile institution`, PostgreSQL, verified email and separate API/worker/migrator roles. [Institutional setup](docs/installation.md#institutional-deployment) |

Binaries target **Linux, macOS and Windows**, on amd64 and arm64. Profiles require **v0.6.0+**; install the [v0.6.1 patch release](https://github.com/ArnaudGuiovanna/tutor-mcp/releases/tag/v0.6.1) for the latest fixes. See [installation details](docs/installation.md) for installers, service configuration and backups.

## Quickstart

### 1. Get the binary

Build the current local-mode implementation with Git and [Go 1.26.8+](https://go.dev/dl/):

```sh
git clone --branch main https://github.com/ArnaudGuiovanna/tutor-mcp.git
cd tutor-mcp
go build .
```

This creates `tutor-mcp` (`tutor-mcp.exe` on Windows). Keep its full path for the next step.

### 2. Connect your AI

For a client using `mcpServers` JSON, such as Claude Desktop, add this to its MCP configuration and replace `command` with the binary's full path:

```json
{
  "mcpServers": {
    "tutor": {
      "command": "/absolute/path/to/tutor-mcp",
      "args": ["--local"]
    }
  }
}
```

On Windows, use a path such as `C:/tools/tutor-mcp.exe`. Restart your client. It launches Tutor automatically and stores your learning profile in `~/.tutor-mcp/local`.

[Claude Code](docs/installation.md#claude-code), [Hermes](docs/installation.md#hermes) and [other clients](docs/clients.md) have their own setup instructions. ChatGPT and other cloud clients use a VPS profile with a public HTTPS endpoint.

### 3. Start learning

> Use Tutor MCP to help me learn Go for backend development. Find out what I already know, create a learning plan, and guide me through a first 20-minute session. Save my progress when we finish.

Next time: **“Resume my Go learning with Tutor MCP.”** Connect to the same Tutor installation to continue with the same history.

## How it works

Your AI handles the conversation, explanations and exercises. Tutor MCP gives it two persistent layers:

| Layer | What it keeps | Why it matters |
|---|---|---|
| **Learning engine** | Concepts, prerequisites, mastery estimates, review timing and assessment evidence | Chooses what to practice, revisit or assess next. |
| **Narrative memory** | Session summaries, goals, recurring misconceptions and useful learner context | Helps the AI pick up the thread and explain things in context. |

The loop is simple: **choose an activity → teach and practice → record the response → update the learning state**. The client calls `get_next_activity` for guidance and `record_interaction` to save observations. Session notes enrich the next conversation.

BKT estimates knowledge, FSRS schedules reviews, and prerequisite checks keep the path coherent. Decisions are inspectable; their quality depends on the evidence the AI records. See the [architecture and diagrams](docs/architecture.md) and [algorithm guide](docs/algorithms.md) for the mechanics and limits.

## Documentation

### Understand and extend Tutor

| Topic | Read more |
|---|---|
| **Architecture** — diagrams, both layers and every runtime component | [Architecture](docs/architecture.md) |
| **Algorithms** — knowledge tracing, spaced repetition, prerequisites and activity selection | [Algorithms](docs/algorithms.md) |
| **MCP tools** — the complete tool catalog, purposes and calling conventions | [MCP tools](docs/mcp-tools.md) |
| **Learning evidence** — assessments, curriculum and progress claims | [Learning integrity](docs/learning-integrity.md) · [Assessment certification](docs/assessment-certification.md) |
| **Development** — contribution workflow and project changes | [Contributing](CONTRIBUTING.md) · [Changelog](CHANGELOG.md) |

### Installation and configuration map

| I want to… | Documentation |
|---|---|
| Connect Claude, ChatGPT, Hermes, Pi or another client | [Client guide](docs/clients.md) |
| Install on Linux, macOS or Windows | [Installation](docs/installation.md) |
| Choose a profile or manage hobby accounts | [Profiles and accounts](docs/profiles.md) |
| Configure ports, storage, OAuth, memory or feature flags | [Configuration reference](docs/configuration.md) |
| Deploy a VPS with systemd/Caddy or Docker Compose | [Native VPS](docs/installation.md#native-hobby-vps-binary-systemd-and-caddy) · [Compose](docs/installation.md#hobby-vps-with-docker-compose) |
| Operate an institutional service | [Institution setup](docs/installation.md#institutional-deployment) · [Operations](OPERATIONS.md) · [Scaling](docs/saas-runtime-operations.md) |
| Back up, restore or move learning data | [Local/VPS backups](docs/installation.md#backups-restore-and-upgrades) · [Tenant restoration](docs/tenant-restore-runbook.md) |
| Configure authentication, memory or notifications in depth | [OAuth registration](docs/oauth-dcr-production.md) · [OAuth scopes](docs/oauth-granular-scopes-rollout.md) · [Memory](docs/narrative-memory-operations.md) · [Webhooks](docs/webhook-delivery-operations.md) |
| Monitor or secure the service | [SLOs and monitoring](docs/saas-slo.md) · [Security](SECURITY.md) |

## Creator

Created and maintained by **Arnaud Guiovanna** — [aguiovanna.fr](https://www.aguiovanna.fr) · [GitHub](https://github.com/ArnaudGuiovanna).

## License

[MIT](LICENSE) — free to use, modify and distribute, including commercially, with the copyright and license notice preserved.
