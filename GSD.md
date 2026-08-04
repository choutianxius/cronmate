# cronmate

> Local LLM cron job daemon CLI in Go. `cronmate` manages prompt files on disk, schedules them via cron expressions, runs them through an LLM with safe tools (e.g. HN digest), and persists results in SQLite. Data at rest private and secure (0600 + OS keychain).

## 0. Decisions Log

* **Q1 LLM Provider:** Decided later. Code must abstract via `LLMClient` interface. Start impl with OpenAI-compatible (covers Ollama, OpenAI, vLLM) as it's easiest to test locally.
* **Q2 Config UX:** `$EDITOR` like `git config`. `cronmate config` opens `config.yaml` in `$EDITOR`. `cronmate config list/get/set/path` also supported. Same for `edit <job>` — opens prompt file in `$EDITOR`. Respect `VISUAL` > `EDITOR` > `vi`.
* **Q3 Encryption:** No SQLCipher in v1. Use `0600` file perms + keyring for secrets + startup permission audit. Optionally app-level AES-GCM for `runs.output` later, key in keyring.
* **Q4 Metadata Duplication (LOCKED Option A):** DB = source of truth for job metadata (schedule, model, tools, enabled). Prompt file = source of truth for prompt body (pure markdown, no frontmatter at runtime). No drift because each piece has one owner, linked by `jobs.prompt_path`. Export/import generates/parses frontmatter only for sharing (Option A/C hybrid). See Section 9.
* **Q5 Prompts Classification (Decided):** Prompts are **data**, not config. Treated as job payload / mutable user data under `XDG_DATA_HOME`, not `XDG_CONFIG_HOME`. Default `prompts_dir = {data_dir}/prompts`, 0600 perms, lifecycle tied to job. `create` copies into data dir by default, `--link` opts into external path.
* **Q6 External MCP Support (Added):** Jobs may call external MCP servers (Model Context Protocol). Tool system must be extensible: built-in tools + MCP tools (stdio/SSE/streamable HTTP). Config adds `mcp_servers` map. Executor holds MCP client manager. See Section 7b.

## 1. Product Summary

### Commands

```bash
# job commands — operates on Job Store
cronmate create  # interactive or flags
cronmate list [--json]
cronmate show <name|id>
cronmate edit <name|id>
cronmate pause <name|id>
cronmate resume <name|id>
cronmate remove <name|id> [--force]
cronmate run <name|id> --now  # manual trigger / dry run

# daemon commands
cronmate          # start if not running, else status (like orbstack/colima)
cronmate status [--watch]
cronmate kill
cronmate restart
cronmate config [list|get <k>|set <k> <v>|edit|path]
cronmate logs [-f --job <name>]
```

### Data Locations (XDG) — Updated: Prompts Are Data

```
Config:   $XDG_CONFIG_HOME/cronmate/  (~/.config/cronmate/)  [0600]
          └── config.yaml   — only declarative settings: providers, scheduler defaults, paths

Data:     $XDG_DATA_HOME/cronmate/  (~/.local/share/cronmate/)  [0700 dir, 0600 files]
          ├── cronmate.db    (WAL, jobs/runs/artifacts)
          ├── prompts/       <— prompts are DATA, mutable job payloads
          │   ├── <job-id>.md — one file per job, or <name>.md (ulid avoids collisions)
          │   └── ...        — hashed content stored in runs.input_prompt_hash for audit
          ├── cronmate.pid
          ├── cronmate.sock  (0600)
          ├── logs/
          └── outputs/       (file_write tool root, jailed)

Cache:    $XDG_CACHE_HOME/cronmate/ (LLM responses if cached)
```

Rationale for data:
* Config is small, portable, expected to be hand-edited, checked in dotfiles.
* Prompts are large, frequently changed via `edit`, tied to job lifecycle (created with `create`, removed with `remove`), versioned via `runs.input_prompt_hash`, backed up with DB.
* User may still symlink `prompts_dir` to a git repo if they want GitOps, but default is data dir. This also avoids confusion where `config` contains secrets-adjacent content.
* `cronmate create --prompt ./my.md` should **copy** file into `data_dir/prompts/<ulid>.md` and store that copied path in DB. Flag `--link` can keep external path for power users.

## 2. Architecture

```
CLI (cobra) ──HTTP over UDS──> Daemon
  |                              |
  ├─ parses flags                ├─ Daemon Lifecycle (pidfile flock + signal handling)
  ├─ client dials sock           ├─ Config Loader (YAML + env)
  └─ runs local if daemon req    ├─ SQLite Store (WAL, migrations)
                                  ├─ Scheduler (robfig/cron v3)
                                  ├─ Executor (worker pool, context timeout)
                                  │   ├─ LLM Registry → OpenAI / Anthropic / Ollama client
                                  │   └─ Tool Registry → fetch_hn, fetch_url, sqlite_write
                                  └─ Status API (/status, /jobs/*)
```

### Why Unix Domain Socket + HTTP

* Singleton guarantee (pidfile+flock + socket exists check).
* Live control: pause/remove without waiting for next poll.
* Tailing logs, streaming status.
* stdlib `net/http` over UDS is trivial; no custom binary protocol.

## 3. Data Model (SQLite)

One DB `cronmate.db`, WAL mode `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`.

```sql
CREATE TABLE jobs (
  id TEXT PRIMARY KEY, -- ulid
  name TEXT UNIQUE NOT NULL,
  schedule TEXT NOT NULL, -- cron expr or @daily etc
  prompt_path TEXT NOT NULL, -- absolute
  model_provider TEXT NOT NULL DEFAULT 'openai',
  model_name TEXT NOT NULL DEFAULT 'gpt-4o-mini',
  enabled INTEGER NOT NULL DEFAULT 1,
  tools_json TEXT NOT NULL DEFAULT '[]', -- JSON array of allowed tool names
  timeout_ms INTEGER NOT NULL DEFAULT 300000,
  max_retries INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE runs (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  status TEXT NOT NULL, -- pending, running, success, failed, timeout, skipped
  triggered_by TEXT NOT NULL, -- schedule, manual, startup-missed
  started_at TEXT,
  finished_at TEXT,
  input_prompt_hash TEXT,
  output TEXT,
  error TEXT,
  tool_calls_json TEXT, -- full trace
  llm_tokens_json TEXT, -- {prompt_tokens, completion_tokens}
  created_at TEXT NOT NULL
);

CREATE TABLE artifacts (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  key TEXT NOT NULL, -- e.g. 'hn_digest'
  value_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_runs_job_id ON runs(job_id, started_at DESC);
CREATE INDEX idx_artifacts_job_key ON artifacts(job_id, key);
```

**Security:**
* On open: `chmod 0600` db file if not already. On startup audit: walk data dir, warn if group/world readable.
* Secrets: `zalando/go-keyring` — `keyring.Set("cronmate/openai", "api_key", ...)`. Config stores only `provider: openai`. Never logs secrets.
* No SQLCipher. If paranoia, later add column-level AES-GCM for `runs.output` using key from keyring.

## 4. Config System

Library: `goccy/go-yaml` or `spf13/viper` light usage. Prefer viper only if needed, otherwise simple YAML marshal/unmarshal to keep deps small.

Prompts are data => `prompts_dir` default is inside `data_dir`, not config dir. Config stores only the path pointer, not the content.

```yaml
# ~/.config/cronmate/config.yaml (0600)
data_dir: ~/.local/share/cronmate
prompts_dir: ~/.local/share/cronmate/prompts  # default = {data_dir}/prompts
log_level: info
timezone: Local

models:
  default_provider: openai
  default_model: gpt-4o-mini
  providers:
    openai:
      base_url: https://api.openai.com/v1
    ollama:
      base_url: http://localhost:11434
    anthropic:
      base_url: https://api.anthropic.com

scheduler:
  max_concurrent_jobs: 2
  overlapping_policy: skip # skip | queue | run_parallel

security:
  tool_allowlist_default: [fetch_hn, fetch_url, sqlite_write, file_write]
  file_write_root: ~/.local/share/cronmate/outputs

# MCP servers — global defaults, can be overridden per job via tools allowlist
mcp_servers:
  filesystem:
    # stdio transport
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
    env: {}
    # timeout for tool calls
    timeout: 30s
  fetch:
    transport: stdio
    command: uvx
    args: ["mcp-server-fetch"]
  linear:
    # streamable HTTP / SSE
    transport: http
    url: https://mcp.linear.app/sse
    headers:
      Authorization: Bearer ${LINEAR_API_KEY}  # resolved from env/keyring
```

* `cronmate config edit` -> `sh -c "$EDITOR <config.yaml>"`. Git-style.
* `cronmate config set scheduler.max_concurrent_jobs 4` — dot-path set.
* env override: `CRONMATE_DATA_DIR`.

## 5. Daemon Lifecycle (internal/daemon)

1. Resolve paths via `internal/config`.
2. Acquire `cronmate.pid` flock (`syscall.Flock` + O_EXCL create). If fail, try to dial existing socket: if alive, "already running", if dead, stale cleanup.
3. Open DB, run migrations (`internal/store/migrations/*.sql` embedded via `go:embed`).
4. Create UDS listener at `cronmate.sock` with `0600`, unlink stale first. `http.Serve` with mux: `/api/status`, `/api/jobs/*`, `/api/logs`.
5. Load jobs from DB, create `scheduler.Runner` (wraps `cron.Cron`).
6. Signal handling: `SIGINT/SIGTERM` -> graceful: stop cron, wait for running jobs up to timeout, close DB, unlink sock+pid. `SIGHUP` -> reload config + resync scheduler.
7. Log to `logs/daemon.log` + stderr if foreground.

Bare `cronmate` command (no subcommand):
* Check socket: if dial ok -> show status (call `status` cmd).
* Else daemonize: `exec.Command(os.Args[0], "--daemon")` with `Setsid`, redirect logs, exit parent. Child runs daemon loop.

Go concepts: `os/signal`, `context.WithCancel`, `net.Listen("unix", ...)`, `sync.WaitGroup`, `embed.FS`.

## 6. Scheduler (internal/scheduler)

* Wrap `robfig/cron/v3` with `cron.WithSeconds(false)`, `cron.WithChain(cron.SkipIfStillRunning)` or custom wrapper to implement `skip/queue/parallel` policy based on job `enabled` + overlapping check.
* On tick: create `runs` row `pending`, check policy, if `skip` and previous `running` -> mark `skipped`.
* Hand off to `executor.Execute(ctx, job, run)`.

Timezone: use config `timezone`. Schedule parsing: accept both standard crontab and `@daily`, `@every 1h`.

## 7. Executor + LLM + Tools (including MCP)

### 7.0 Overview

Tools are now two sources:
* **Built-in**: `fetch_hn`, `fetch_url`, `sqlite_write`, `file_write` — in-process Go.
* **MCP**: external servers over stdio/SSE/HTTP, discovered via MCP `tools/list`.

LLM sees unified list: `fetch_hn` + `mcp__filesystem__read_file` + `mcp__fetch__fetch` etc. Prefix `mcp__<server>__<tool>` avoids collisions.

### Executor

```go
type Executor struct {
  store Store
  llmRegistry LLMRegistry
  toolRegistry ToolRegistry      // built-in
  mcpManager MCPManager          // external MCP clients
  workPool chan struct{}
}

func (e *Executor) Execute(ctx context.Context, job Job, run Run) error
```

Steps:
1. Load prompt file from `job.prompt_path`. Hash content.
2. Resolve allowed tools: `tools = builtins.Resolve(job.tools_json) + mcpManager.Resolve(job.tools_json)` — mcpManager lazily starts needed servers on first use, caches `tools/list`.
3. Build `CompletionRequest` — system = prompt file, tools = unified list.
4. Resolve `LLMClient` via `job.model_provider`.
5. Agent loop (max 10 turns): call LLM, if `tool_calls`, route each: if `mcp__*`, call `mcpManager.CallTool(ctx, server, tool, args)`, else built-in `Execute`.
6. On success: write `runs.output`, artifacts.
7. On fail/timeout: error.

`MCPManager` lifetime is daemon-wide, not per-run: keeps stdio processes alive, reconnects SSE. Per-run gets fresh `ctx`.

### 7a. LLM Abstraction

```go
type LLMClient interface {
  Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}
type CompletionRequest struct {
  Model string
  Messages []Message
  Tools []ToolDefinition
}
```

Implementations: `openaiCompatible`, `anthropic`. Factory reads api_key from keyring.

### 7b. Tool System + MCP + Safety

```go
type Tool interface {
  Name() string
  Definition() ToolDefinition
  Execute(ctx context.Context, args json.RawMessage) (string, error)
}
type MCPManager interface {
  ListServers() []ServerInfo
  ListTools(ctx context.Context) ([]ToolDefinition, error) // aggregated
  Resolve(allowlist []string) []ToolDefinition              // filter by job allowlist
  CallTool(ctx context.Context, server, tool string, args json.RawMessage) (string, error)
  Shutdown()
}
```

**MCP Transport:**
* `stdio`: spawn `command args`, speak JSON-RPC over stdin/stdout. Use `github.com/mark3labs/mcp-go` client or `modelcontextprotocol/go-sdk`. Manage process lifecycle: start on demand, keepalive 5m idle kill, `SIGTERM` on daemon shutdown. Env expansion from `os.Getenv` + keyring (`${VAR}`).
* `http` / `sse` / `streamable-http`: HTTP client with headers (auth from env/keyring), reconnect with backoff.

**Tool Naming & Allowlist:**
* MCP tool `filesystem:read_file` -> exposed as `mcp__filesystem__read_file`. Definitions rewritten to include server description.
* Job `tools_json` allowlist can contain `fetch_hn`, `mcp__filesystem__*` wildcard or `mcp__*` to allow all MCP. Validated against global `tool_allowlist_default` + `mcp_servers` keys.
* `cronmate list --tools` shows both builtin + MCP discovered tools.

**Safety for MCP (critical because external):**
* Built-ins: existing rules (no shell, SSRF guard, jailed file_write etc).
* MCP deemed **less trusted**:
  - Enforce timeout per server (default 30s) + max output size 1MB, per tool call log truncated in DB.
  - Spawned stdio commands allowlist: `security.mcp_allowlist_commands: [npx, uvx, node]` or `deny` all unless listed. No shell injection — use `exec.CommandContext` with args slice, not shell.
  - Env: only pass explicit env from config + minimal PATH, not full parent env.
  - File system MCP servers: user must configure root dir, can't access data dir unless allowed. Recommend separate `outputs/` jailed still.
  - Network MCP servers: apply same SSRF deny private IPs unless `security.allow_private_mcp` true.
  - Audit: all MCP tool calls logged in `runs.tool_calls_json` with server name, args hash.
  - Optional confirmation: `security.mcp_require_confirm_for: [filesystem__write_file]` -> pause job and require `cronmate run --approve <run_id>` for destructive tools (v2).
* No `exec` tool that lets LLM run arbitrary shell — even via MCP, block tools named `*exec*`, `*shell*` unless explicitly allowlisted.

**v1 Built-ins (still):**
* `fetch_hn`, `fetch_url`, `sqlite_write`, `file_write` as before.

**Example flow HN + MCP:**
* Job prompt: "Use fetch_hn then write markdown file via filesystem MCP"
* Executor resolves tools: `fetch_hn` (builtin) + `mcp__filesystem__write_file` (MCP)
* LLM first calls `fetch_hn`, then `mcp__filesystem__write_file(path=outputs/.../digest.md, content=...)`
* `mcpManager` routes second call to stdio process `npx ... filesystem` via JSON-RPC `tools/call`.

**Libraries for MCP in Go:**
* `github.com/mark3labs/mcp-go` — most mature Go MCP client/server, supports stdio + SSE.
* `github.com/modelcontextprotocol/go-sdk` — official, newer.
* Recommend `mcp-go` for v1: `client.NewStdioMCPClient(cmd, args)` and `client.NewSSEMCPClient(url)`.

All tool executions logged in `runs.tool_calls_json`. No shell.

## 8. CLI (internal/cli, cobra)

* Root cmd: if no args and not `--daemon` internal flag, check daemon status logic.
* Each job cmd uses client: `internal/daemon/client.go` that dials UDS. If daemon not running for read cmds (`list/show`), fallback to direct DB read? Decision: allow direct DB read for `list/show` when daemon down, but write cmds (`create/pause/remove`) require daemon or direct DB with warning. Simpler: always allow direct DB when daemon down except `status/kill`.
* `create`: flags `--name, --schedule, --prompt-file, --model, --tools`. If missing, interactive prompt with `AlecAivazis/survey` or `charmbracelet/huh`. Copy prompt file into `prompts_dir` if outside? Keep original path but warn. Insert into DB.
* `edit <job>`: 2 modes to be decided (see Section 9). For now: open prompt file in `$EDITOR`. After editor closes, re-hash, maybe update DB `updated_at`. Do not parse frontmatter yet pending Q4.
* `list/show`: table writer (`olekukonko/tablewriter`).
* `config edit`: `$EDITOR` config.yaml.

## 9. Metadata Drift — LOCKED: Option A (DB Truth, Prompt = Pure Body)

**Resolution per Q5 (prompts=Data):** This simplifies to no drift by design.

* **Config** (`~/.config/cronmate/config.yaml`): app-wide settings only: default models, paths, scheduler policy.
* **Data** (`~/.local/share/cronmate/`):
  * `cronmate.db` = **source of truth for job metadata**: `name, schedule, model_provider, model_name, enabled, tools_json, timeout`. Fast query for scheduler, no FS scan.
  * `prompts/<name>--<ulid>.md` = **source of truth for prompt body**: raw markdown, no frontmatter at runtime. Versioned via `runs.input_prompt_hash`.
  * `artifacts` table + `outputs/` = job outputs.

Mental model: like email — headers (DB) + body (prompt file). Like `crontab` — metadata separated from script body.

**Locked Rules:**

1.  **No frontmatter at runtime in v1.** `executor` loads prompt file as-is, no YAML parsing. If file contains `---` header, it's treated as part of prompt (harmless).
2.  **Create:** `cronmate create -n hn-daily --schedule "@daily" --prompt ./my.md --model openai:gpt-4o-mini`
    * DB row inserted, prompt file **copied** to `prompts_dir/hn-daily--01H...md`, path stored absolute in `jobs.prompt_path`. Hash saved.
    * Flag `--link` skips copy and stores external absolute path (power user, must handle missing file).
3.  **Edit:** `cronmate edit hn-daily` opens `prompt_path` in `$EDITOR`. For metadata: `cronmate edit hn-daily --schedule "0 8 * * *"` OR `cronmate pause/resume`. Never edit schedule by editing file.
4.  **Show:** `cronmate show hn-daily` joins DB + file preview. Flag `--export` generates portable frontmatter file **only for sharing**:
    ```markdown
    ---
    name: hn-daily
    schedule: 0 8 * * *
    model: openai:gpt-4o-mini
    tools: [fetch_hn, sqlite_write]
    cronmate_version: 1
    exported_at: 2026-07-23T...
    ---
    <prompt body>
    ```
5.  **Import:** `cronmate create --import ./exported.md` parses frontmatter once, splits into DB row + prompt data file, then discards frontmatter. Frontmatter is interchange format, not canonical.
6.  **Remove:** deletes DB row + prompt file if file lives inside `prompts_dir`. If external linked, only DB row removed (warn). Print path for recovery. Add `cronmate list --orphans` later to find leftover files.
7.  **No `fsnotify` watcher in v1.** Daemon does not watch FS for changes; it relies on DB. Prompt edits are picked up next run (executor always reads file fresh).

**File Naming:** Chosen `${name}--${ulid}.md` (name for debuggability via `ls`, ulid for uniqueness/collision-free). `ulid` also gives creation time ordering.

**Alternatives considered and rejected:**

* Option B File-as-Truth with frontmatter: violates prompts-as-data, needs bidirectional sync, race conditions.
* Option C full Hybrid with watch loop: deferred to v2 if GitOps demand.

**Result:** Zero drift. Each field has single owner. UX stays clean: `edit` = prompt, flags = schedule/model.

## 10. Project Layout (Updated: Prompts = Data)

```
cronmate/
  GSD.md
  go.mod
  Makefile
  cmd/cronmate/
    main.go
  internal/
    cli/           # cobra commands, table printing, editor launch, export/import
    config/        # XDG paths, load/save, validation, keyring helpers (ONLY config.yaml, no prompts)
    store/         # DB open, migrations, job/run/artifact CRUD, queries
    daemon/        # lifecycle, pidfile, socket server, client
    scheduler/     # cron wrapper + policy
    executor/      # execute loop, worker pool, calls toolRegistry + mcpManager
    llm/           # interface + openaiCompat + anthropic + registry
    tools/         # built-in registry + fetch_hn + fetch_url + sqlite_write + file_write
    mcp/           # MCP manager, stdio/http clients, server lifecycle, tool discovery
    security/      # perms check, SSRF guard, tool guards, mcp allowlists
  examples/
    hn_daily.md    # example prompt body (no frontmatter in v1), for docs only
  migrations/
    001_init.sql
  README.md

Runtime data dir (not in repo):
~/.local/share/cronmate/
  ├── cronmate.db
  ├── prompts/   <- data, e.g. hn-daily--01H...md (copied from user input)
  ├── outputs/
  └── logs/
```

Go libs:
* `cobra` + `charmbracelet/huh` (interactive)
* `robfig/cron/v3`
* `modernc.org/sqlite` (pure Go, no CGO) — alternative `mattn/go-sqlite3` if you want CGO performance.
* `zalando/go-keyring`
* `goccy/go-yaml` + `BurntSushi/toml` (support both yaml/toml config per discussion)
* `google/ulid` or `oklog/ulid`
* `rs/zerolog` for logging (or std `log/slog`)
* `olekukonko/tablewriter`
* MCP: `github.com/mark3labs/mcp-go` (client, stdio + SSE/http) + `modelcontextprotocol/go-sdk` alternative

## 11. Phased Plan + Go Learning Map

### Phase 0: Scaffolding (Day 1)
* `go mod init github.com/<you>/cronmate`, `cmd/cronmate/main.go` prints "cronmate".
* Install cobra scaffold, `Makefile` with `build`, `fmt`, `vet`.
* **Learn:** modules, packages, `main` vs `internal`, `go run`, `go build`, imports.

### Phase 1: Config + Store
* Implement `config.Paths()` returning XDG paths, create dirs with 0700/0600 checks.
* `config.yaml` load/save with defaults.
* `store` open SQLite WAL, embed migrations, CRUD for jobs.
* `cli`: `config list/path` direct file.
* **Learn:** structs, pointers, `os` package, `filepath`, `embed`, `database/sql`, error wrapping `%w`.

### Phase 2: Job CRUD CLI
* `create/list/show/remove/pause/resume` via direct DB (no daemon yet).
* `edit` launches `$EDITOR` — `os/exec.Command` with `Stdin=Stdin`.
* Table output.
* Tests: store tests with `t.TempDir()` + sqlite memory.
* **Learn:** cobra, flags, interfaces (Store interface), testing, temp files.

### Phase 3: Daemon Singleton
* `daemon.Daemon` pidfile flock, UDS HTTP server, client dialer.
* `status/kill/restart` commands.
* Bare `cronmate` = start if down else status.
* Signal handling.
* **Learn:** goroutines, channels, `context`, `net`, `net/http`, `syscall`, `os/signal`.

### Phase 4: Scheduler
* Wrap `cron`, restore jobs on daemon start, add/remove on CRUD via API.
* Overlapping policies.
* `run --now` endpoint.
* **Learn:** concurrency patterns, `sync.Mutex/RWMutex`, worker pool semaphore.

### Phase 5: LLM Abstraction
* `LLMClient` interface, `openaiCompatible` client using `net/http`, context timeout, keyring lookup.
* `executor.Execute` loads prompt file, calls LLM without tools first (echo/bare prompt works end-to-end).
* **Learn:** interfaces, `http.Client`, JSON marshalling, `context`.

### Phase 6: Tool System + HN Example
* `Tool` interface, registry, `fetch_hn`.
* Agent loop (tool calling).
* `sqlite_write` safe.
* Example prompt `hn_daily.md` + test run generates artifact.
* **Learn:** `json.RawMessage`, type switches, error handling, SSRF guards, dependency injection.

### Phase 6b: MCP Integration (New, after Phase 6)
* Implement `internal/mcp` — manager, stdio client lifecycle, SSE client.
* Config: `mcp_servers` map + env expansion + keyring for headers.
* Tool discovery: `tools/list`, cache, convert to OpenAI tool format with `mcp__<server>__<tool>` prefix.
* Routing: executor routes MCP calls to manager, logging + timeout.
* CLI: `cronmate config list --tools`, `cronmate mcp list/test <server>`.
* Safety: command allowlist, SSRF guard for http MCP, max output 1MB, audit log.
* Test with `mcp-server-fetch` + `filesystem` MCP servers.
* **Learn:** `os/exec` process management, JSON-RPC, context with timeout, concurrency (manager holds map of clients with mutex), `io` piping.

### Phase 7: Hardening + Logs
* Permissions audit at startup, keyring API key commands `cronmate config set-key openai`.
* `logs -f` via SSE or chunked HTTP over UDS, includes MCP server logs.
* `golangci-lint`, `go vet`, integration test e2e including MCP mock server.
* **Learn:** security, file modes, keyring, streaming.

### Phase 8: Polish & OS Integration
* launchd plist generator `cronmate config --init-launchd`.
* `export/import` for portability (now decided: implements Option A/C hybrid). Export includes `mcp_servers` needed? No, export is job-level, not server-level. But mention required MCP servers in exported comment.
* README, demo gif with MCP example (e.g. fetch + filesystem + linear).
* **Learn:** build tags, conditional compilation, release via `goreleaser`.

## 12. Threat Model Checklist (v1 + MCP)

* [ ] DB, sock, pid, logs all 0600, dirs 0700.
* [ ] No API keys in config.yaml or logs (redact). MCP headers from keyring/env, not logged.
* [ ] Tool allowlist enforced, no shell. MCP tools namespaced `mcp__*` filtered by allowlist + wildcard.
* [ ] fetch_url blocks private IPs, limits size, timeouts.
* [ ] file_write jailed to data dir, no `..`.
* [ ] sqlite_write no raw SQL from LLM.
* [ ] Prompt file path validated (no symlink escape outside allowed? Consider).
* [ ] Daemon socket only accessible by same UID (filesystem perms ensures).
* [ ] `run` output not executed as code.
* [ ] MCP: stdio command allowlist, no shell interpolation, minimal env, timeout + max output.
* [ ] MCP http servers SSRF guard (deny private IPs unless allow_private).
* [ ] MCP destructive tools (write_file, delete) flagged, optionally require confirm (v2).

## 13. Next Steps (Locked + MCP Added)

1. [DONE] Q4 locked to Option A (DB truth, prompt = pure body) + Q5 prompts=data.
2. [DONE] Q6 MCP support added as first-class pluggable tools.
3. v1 skips frontmatter parser at runtime; only used in `export/import` (interchange format), to be implemented in Phase 8.
4. Ready to scaffold Phase 0+1: `go mod init`, config paths, SQLite store with migrations, basic CLI job CRUD.
5. Naming decision for prompt files locked: `${name}--${ulid}.md`.
6. Next design after scaffold: confirm `mcp-go` library choice and whether to eager-start MCP servers or lazy per-job.

---

*Last updated: 2026-07-23. Q1 (LLM provider) still open, no scaffold yet per request. Ready to scaffold when you say go.*
