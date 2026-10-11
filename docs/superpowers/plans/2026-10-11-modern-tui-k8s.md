# Modern TUI + Kubernetes Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Modernize the vartop TUI (screen-stack architecture, discoverable keybindings) and replace static pod endpoint lists with live Kubernetes EndpointSlice discovery.

**Architecture:** Config gains a `kubernetes` block as an alternative to static `endpoints`. A new `internal/discovery` package exposes a `Source` interface with `static` and `kubernetes` (client-go EndpointSlice informer) implementations feeding an in-memory ready-endpoint registry. The API layer becomes a client pool reconciled from the registry each poll tick. The TUI is refactored from one flag-heavy model into a screen stack, then gets a keymap/help footer and status bar.

**Tech Stack:** Go 1.26 (forced by k8s.io v0.37.1), bubbletea/bubbles/lipgloss/huh (existing), cobra (existing), k8s.io/client-go + k8s.io/api + k8s.io/apimachinery (new, Task 2 only).

**Spec:** none — this plan is the spec; decisions were agreed with the project owner in conversation (XDG config path, no backward compatibility, direct pod-IP dialing, no port-forward).

## Global Constraints

- **No backward compatibility.** The tool was never released. Delete the `~/.vartop.json` → `~/.vartop/settings.json` migration in `config.Load`, the old `~/.vartop` directory handling, and the `endpoint`→`endpoints` `UnmarshalJSON` shim on `Server`. Do not write new migration code.
- **Config path:** `os.UserConfigDir()/vartop/config.json` (i.e. `~/.config/vartop/config.json` on Linux), created `0755`, file `0600`.
- **Direct dialing only.** Pod IPs are assumed routable. No port-forward, no `kubectl` subprocesses, no API-server proxy.
- **New dependencies:** only `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` (Task 2). No other new deps.
- `gofmt` clean, `go vet ./...` clean, `go build ./...` passes after every task.
- Conventional commit messages (`feat:`, `refactor:`, etc.).
- Do NOT commit these pre-existing untracked files: `AGENTS.md`, `CLAUDE.md`, `README.md`, `.claude/`, `docs/superpowers/`, `.superpowers/`.
- Follow the repo's `AGENTS.md` (GitNexus impact checks before editing symbols) where those tools are available to you.

## Review Focus

1. Unreachable/wrong kube context → `NewKubernetes` returns an error promptly; the TUI shows it instead of hanging. (Task 2 test.)
2. EndpointSlice churn concurrent with a poll tick → registry snapshots are consistent. (Task 2 test: concurrent add/remove/read.)
3. Zero ready endpoints (all pods down) → pool reconciles to empty; dashboard renders its empty/unhealthy state, no panic. (Task 3 test.)
4. Config with both `endpoints` and `kubernetes` set → `Validate` rejects with a clear message. (Task 1 test.)
5. Terminal resize while switcher/detail screen is open → layout clamps, no panic. (Task 4 manual check.)

---

### Task 1: Config schema + XDG path

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/prompt.go`
- Modify: `cmd/server.go` (the `server add` flow)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (Task 2 relies on these exact types):
```go
type KubernetesRef struct {
    Context   string `json:"context"`
    Namespace string `json:"namespace"`
    Service   string `json:"service,omitempty"`
    Selector  string `json:"selector,omitempty"`
    Port      int    `json:"port,omitempty"` // 0 = first port from EndpointSlice
    Path      string `json:"path,omitempty"` // default "/varmq"
}

type Server struct {
    Name       string         `json:"name"`
    Endpoints  []string       `json:"endpoints,omitempty"`
    Kubernetes *KubernetesRef `json:"kubernetes,omitempty"`
    Token      string         `json:"token,omitempty"`
    Interval   int            `json:"interval,omitempty"`
}

func (s *Server) Validate() error  // exactly one of Endpoints/Kubernetes; within Kubernetes: Context and Namespace required, Service or Selector (not both)
func Load() (*Config, error)       // reads os.UserConfigDir()/vartop/config.json; missing file → ErrConfigNotFound
func Save(cfg *Config) error
```
`GetEndpoints`, `DisplayEndpoint`, `ShortOrigin`, `Config`, `GetActiveServer`, `ErrConfigNotFound` keep their current signatures.

- [ ] **Step 1: Write failing tests** in `internal/config/config_test.go`: (a) `Validate` accepts endpoints-only and kubernetes-only servers; (b) rejects both-set and neither-set; (c) rejects Kubernetes missing Context or Namespace; (d) rejects Service+Selector both set; (e) `Save`+`Load` round-trip through a temp `XDG_CONFIG_HOME` preserves a KubernetesRef.
- [ ] **Step 2:** Run `go test ./internal/config/ -run TestValidate -v` and `... -run TestRoundTrip` — expect FAIL (types don't exist).
- [ ] **Step 3:** Rewrite `config.go`: new `Server`/`KubernetesRef` types, `Validate`, XDG path, delete migration + `UnmarshalJSON` shim. `Load` calls `Validate` per server and errors on the first invalid one.
- [ ] **Step 4:** Update `prompt.go` onboarding and `cmd/server.go`: ask "static endpoints or kubernetes?"; k8s path prompts for context, namespace, service-or-selector. Writes the final schema only.
- [ ] **Step 5:** Run tests — expect PASS. Then `gofmt -l .`, `go vet ./...`, `go build ./...` all clean.
- [ ] **Step 6:** Commit: `feat!: new config schema with XDG path and kubernetes block`

### Task 2: Discovery package (static + kubernetes EndpointSlice informer)

**Files:**
- Create: `internal/discovery/discovery.go` (Source interface + registry)
- Create: `internal/discovery/static.go`
- Create: `internal/discovery/kubernetes.go`
- Test: `internal/discovery/discovery_test.go`

**Interfaces:**
- Consumes: `config.KubernetesRef` (Task 1).
- Produces (Task 3 relies on these):
```go
// Source is a live set of ready endpoint URLs, safe for concurrent use.
type Source interface {
    Endpoints() []string // sorted snapshot of current ready endpoints
    Close() error        // static: no-op nil; kubernetes: stops informer
}

func NewStatic(endpoints []string) Source
func NewKubernetes(ref config.KubernetesRef) (Source, error)
```
`NewKubernetes` builds a client from the kubeconfig `ref.Context` (clientcmd loading rules + context override), starts a `discovery.k8s.io/v1` EndpointSlice informer in `ref.Namespace` filtered by label selector (`kubernetes.io/service-name=<ref.Service>`, or `ref.Selector`), waits for first cache sync with a 10s timeout, and returns. Endpoint URL construction: `http://<address>:<port><path>` where address comes from ready slice endpoints only, port is `ref.Port` or the slice's first port, path is `ref.Path` or `/varmq`. Errors (bad context, sync timeout) return promptly — never hang.

- [ ] **Step 1: Write failing tests**: (a) registry add/remove/Endpoints snapshot under 100 concurrent goroutines (`go test -race`); (b) `NewStatic` returns its input sorted; (c) `NewKubernetes` with a nonexistent context returns an error within ~15s (no hang).
- [ ] **Step 2:** Run `go test -race ./internal/discovery/` — expect FAIL (package doesn't exist).
- [ ] **Step 3:** `go get k8s.io/client-go k8s.io/api k8s.io/apimachinery`, then implement the three files. Registry: `sync.RWMutex` + `map[string]struct{}`, `Endpoints()` returns sorted keys.
- [ ] **Step 4:** Run `go test -race ./internal/discovery/ -v` — PASS. `gofmt`, `go vet`, `go build ./...` clean.
- [ ] **Step 5:** Commit: `feat: add discovery package with kubernetes EndpointSlice source`

### Task 3: API client pool

**Files:**
- Create: `internal/api/pool.go`
- Modify: `internal/api/client.go` (only if `Client` doesn't already expose its endpoint URL — add `Origin() string`)
- Modify: `internal/tui/commands.go`, `internal/tui/handlers.go` (iterate pool instead of a fixed endpoint list)
- Modify: `internal/tui/model.go`, `cmd/root.go` (construct pool from `discovery.Source`; call `Close` on quit)
- Test: `internal/api/pool_test.go`

**Interfaces:**
- Consumes: `discovery.Source` (Task 2), existing `api.Client`.
- Produces (Task 4's TUI relies on this):
```go
// Pool reconciles api.Clients against a discovery.Source.
type Pool struct { /* ... */ }

func NewPool(source discovery.Source, token string) *Pool
func (p *Pool) Clients() []*Client // reconciles from source snapshot on each call: create clients for new endpoints, drop removed ones
func (p *Pool) Close() error       // delegates to source.Close()
```
Semantics: `Clients()` is cheap and idempotent; repeated calls with an unchanged snapshot return the same client instances (keyed by endpoint URL).

- [ ] **Step 1: Write failing tests**: (a) reconcile adds clients for new endpoints and drops removed ones across successive snapshots; (b) unchanged snapshot returns identical client pointers; (c) empty snapshot → zero clients, no panic.
- [ ] **Step 2:** Run `go test ./internal/api/ -v` — expect FAIL.
- [ ] **Step 3:** Implement `pool.go` (+ `Origin()` if needed). Rewire `tui/commands.go`/`handlers.go` to range over `pool.Clients()` where they currently range over server endpoints; construct the source + pool in `cmd/root.go` (`discovery.NewStatic` for endpoints servers, `NewKubernetes` for kubernetes servers; surface `NewKubernetes` errors into the TUI's existing error display) and close on quit.
- [ ] **Step 4:** Tests PASS; `gofmt`, `go vet`, `go build ./...` clean; manual smoke `go run . --help`.
- [ ] **Step 5:** Commit: `feat: reconcile api client pool from discovery source`

### Task 4: TUI screen-stack refactor (behaviorally identical)

**Files:**
- Create: `internal/tui/screen.go` (screen type + stack)
- Modify: `internal/tui/model.go` (root model: shared state + stack only)
- Create: `internal/tui/dashboard.go` (from `views.go`/`sections.go` dashboard parts + dashboard key handling)
- Create: `internal/tui/detail.go` (worker detail screen)
- Create: `internal/tui/switcher.go` (server/pod switcher screen)
- Modify/delete: `internal/tui/views.go`, `internal/tui/sections.go`, `internal/tui/handlers.go` as the split dictates
- Test: `internal/tui/screen_test.go`

**Interfaces:**
- Consumes: `api.Pool` (Task 3).
- Produces: Task 5 adds keys/help per screen; screens must own their key handling (each screen's `Update` handles its own `tea.KeyMsg`).

Approach: root `Model` holds config, pool, health maps, window size, and a stack of screens (`[]screen`, top = active). Each screen owns its state and key handling; pushing/popping replaces today's `showSwitcher`/`showWorkerDetail`/`inActionSelection`/`inWorkerDetailAction` flag matrix. **No visual or keybinding changes** — same views, same keys, same flows. This is a pure refactor.

- [ ] **Step 1: Write failing test** in `screen_test.go`: push dashboard→detail→switcher, pop twice, assert the active screen type at each step and that pop-to-empty quits (or whatever terminal behavior the current flag logic has — mirror it).
- [ ] **Step 2:** Run test — FAIL.
- [ ] **Step 3:** Do the split. Move code, don't rewrite it. Delete the flag matrix from `model.go`.
- [ ] **Step 4:** Test PASS; `gofmt`, `go vet`, `go build ./...` clean. Manual check: `go run .` (with a `~/.config/vartop/config.json` pointing at any static endpoint, even unreachable) — dashboard renders, `tab` opens switcher, `esc` returns, resize doesn't panic.
- [ ] **Step 5:** Commit: `refactor: split tui model into screen stack`

### Task 5: Keymap, help footer, status bar

**Files:**
- Create: `internal/tui/keys.go` (per-screen `bubbles/key` keymaps implementing `help.KeyMap`)
- Create: `internal/tui/help.go` (`bubbles/help` footer wiring)
- Modify: `internal/tui/dashboard.go`, `detail.go`, `switcher.go` (use keymaps, render footer)
- Modify: `internal/tui/styles.go`, `internal/tui/dashboard.go` (status bar)
- Test: none (presentational; build + manual verify)

**Interfaces:**
- Consumes: Task 4 screens.
- Produces: nothing downstream.

- [ ] **Step 1:** Define per-screen keymaps from the keys each screen handles today (dashboard: `q` quit, `r` refresh, `a` actions, `tab` switch, `enter` detail; detail: `esc`, `a`, `r`, `j/k/g/G/pgup/pgdown`; switcher: `esc`, `enter`, `h/backspace`). Replace string-switch key handling with `key.Matches`.
- [ ] **Step 2:** Render a `bubbles/help` footer at the bottom of every screen showing that screen's short help.
- [ ] **Step 3:** Status bar at the top of the dashboard: server name, kube context+namespace (kubernetes servers) or endpoint (static servers), healthy pods/total pods, last refresh time. Style via existing `styles.go` palette.
- [ ] **Step 4:** `gofmt`, `go vet`, `go build ./...` clean; manual run: footer visible on all three screens, status bar correct, every keybinding still works via the new keymap.
- [ ] **Step 5:** Commit: `feat: discoverable keybindings, help footer, and status bar`
