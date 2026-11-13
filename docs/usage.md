# Usage

## `vartop web`

Serves the dashboard and opens it in your browser. While it runs it collects **every configured server** (see [Collection](how-it-works.md#collection)), so switching servers on the page is instant and their charts are already drawn.

```bash
vartop web                             # http://127.0.0.1:7070, default server
vartop web -s staging                  # a specific server first
vartop web --port 9000                 # another port
vartop web --host localhost --port 0   # any free port
vartop web --no-open                   # print the URL, don't launch a browser
```

| Flag | Description | Default |
| --- | --- | --- |
| `--host` | Host to listen on (must be a loopback address) | `127.0.0.1` |
| `--port` | Port to listen on; `0` picks any free port | `7070` |
| `--no-open` | Don't open the browser | *(opens it)* |
| `-s`, `--server` | Server to show first; the page can switch to any configured server | *(default server, else the first configured)* |
| `--hub-listen` | Address the hub listens on for VarMQ instances of hub servers, see [Hub mode](hub.md) | *(no hub)* |
| `--hub-tls-cert`, `--hub-tls-key` | TLS certificate and key files of the hub listener | *(plain HTTP)* |
| `--hub-max-conns-per-ip` | Connections from one source IP that have not sent a valid token yet; `0` for no limit. Use `0` behind a reverse proxy that opens a new upstream connection per request | `64` |

Press <kbd>Ctrl+C</kbd> to stop; collection stops with it.

## Command line

| Command | Description |
| --- | --- |
| `vartop` | Print help |
| `vartop web` | Serve the dashboard and collect every server while it runs |
| `vartop server list` | List all servers (static servers with their health status) |
| `vartop server add` | Register a new server via the interactive wizard (static, DNS or hub) |
| `vartop server use <name>` | Set the default server |
| `vartop server remove <name>` | Remove a server (`--all`: every server) |
| `vartop server set <name> endpoint\|token\|interval\|history <value>` | Update a server property (`endpoint` accepts a comma-separated list, static servers only; `history` is whole days, `1d` to `30d`) |
| `vartop action <worker> pause\|resume\|restart\|stop` | Act on a worker |
| `vartop concurrency <worker> <n>` | Set a worker's concurrency pool size |
| `-s`, `--server <name>` | The server `web`, `action` and `concurrency` use (overrides the default) |

`action` and `concurrency` cannot reach the instances of a [hub server](hub.md): for one, they fail and point to the dashboard. Otherwise they run against **every endpoint** of the server (each static endpoint, or each address DNS discovery finds) concurrently and print one result line per endpoint; the command fails if any endpoint fails. Behind a load-balancer URL that is one instance, the one the balancer picks: see [Load balancers](how-it-works.md#load-balancers). `concurrency` is per instance, so `concurrency orders 8` on 3 endpoints gives 24 workers in total. To target one endpoint, pass its URL:

```bash
vartop action orders pause                                        # every endpoint
vartop action orders pause --origin http://10.0.0.5:8080/varmq    # one endpoint
vartop -s staging concurrency orders 8
```

## URL parameters

A page's address says what it shows, by the names the page shows, so a reload, the refresh and a shared link keep it:

| Parameter | Meaning |
| --- | --- |
| `server=<name>` | The configured server to show, by the key it is listed under in the config (the default server when absent, else the first configured server by name) |
| `instance=<label>` | Only that instance, e.g. `?instance=instance-2` |
| `state=<state>`, `offset=<n>` | On the workflows list: only that state (`running`, `completed`, …), from that row. On a queue page: the job tab (`pending`, `failed`, …), from that row |
| `at=<ms>` | On a workflow page: which of several workflows of that ID (in-memory ones of different instances), by its creation time |
| `completed=<span>`, `failed=<span>` | On a worker page: the span of that chart, `24h`, `7d`, `30d` or the server's history (e.g. `14d`); without it, the hour |

Without `instance` the page shows every instance merged. An old `pod=` link opens the merged view. A label the server never had says so; one that is no longer reported says that, with a link to the merged view. A span the server's history does not offer (a link from before it was shortened, say) shows the hour.
