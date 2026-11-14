# Configuration

Configuration lives in your OS user config directory, created on first run (directory `0755`, file `0600` since it may hold tokens). A running `vartop web` watches the file and picks up changes to it (see [Collection](how-it-works.md#collection)):

| OS | Path |
| --- | --- |
| Linux | `$XDG_CONFIG_HOME/vartop/config.json` (default `~/.config/vartop/config.json`) |
| macOS | `~/Library/Application Support/vartop/config.json` |
| Windows | `%AppData%\vartop\config.json` |

If `config.json` is a symlink, say into a dotfiles repository, vartop saves through it and the link stays.

## Schema

Each server uses **exactly one** of `endpoints` (static list), `dns` (live discovery) or `hub` (the instances connect to vartop, see [Hub mode](hub.md)). Setting more than one, or none, is rejected when the config is loaded. A server with a `kubernetes` block (discovery removed) fails to load with *kubernetes discovery was removed; use endpoints, dns or hub*: replace the block with `endpoints`, `dns` or `hub`.

```json
{
  "default_server": "production",
  "servers": {
    "production": {
      "name": "production",
      "endpoints": ["http://pod-0:8080/varmq", "http://pod-1:8080/varmq"],
      "token": "optional-bearer-token",
      "interval": 5,
      "history": "30d"
    },
    "headless": {
      "name": "headless",
      "dns": {
        "host": "varmq-headless.workers.svc.cluster.local",
        "port": 8080,
        "path": "/varmq"
      }
    }
  }
}
```

| Field | Description | Default |
| --- | --- | --- |
| `default_server` | Server to use when no `-s` flag is given | *(none)* |
| `servers.<name>.endpoints` | Static list of mux endpoint URLs (one for a single instance, several for more) | *(one of endpoints / dns / hub)* |
| `servers.<name>.dns` | DNS discovery block, see [DNS](#dns) | *(one of endpoints / dns / hub)* |
| `servers.<name>.hub` | `true`: the instances connect to vartop web, see [Hub mode](hub.md) | *(one of endpoints / dns / hub)* |
| `servers.<name>.token` | Bearer token for authentication (sent as `Authorization` header). For a hub server, the token the instances connect with: required, at least 16 characters, unique among hub servers | *(none)* |
| `servers.<name>.interval` | How often vartop web polls the server, in seconds | `5` |
| `servers.<name>.history` | How far back a worker page's charts reach: whole days, `1d` to `30d`. Each worker's rates are kept that long, in 5-minute buckets (about 4–5 MB a day for 10 instances of 20 workers); lowering it drops the older ones within 5 minutes, and later buckets reuse their space (the file does not shrink) | `1d` |

## DNS

Resolves a hostname to **all** of its addresses and dials each one, re-resolving every 5 seconds so instances that scale out or move are picked up. Works anywhere a name returns one address per instance:

- a Kubernetes **headless** Service (`clusterIP: None`), handy when vartop runs inside the cluster
- a Docker Compose service name with several replicas (`tasks.<service>` on Swarm)
- Consul, Nomad, or AWS Cloud Map service DNS

| Field | Description | Default |
| --- | --- | --- |
| `host` | Hostname to resolve (A/AAAA records) | *(required)* |
| `port` | Port each instance serves the mux on | *(required)* |
| `path` | Mux API path on each instance | `/varmq` |

An unresolvable host fails at connect time with the lookup error; a failed refresh later keeps the last known instances. Instances are dialed directly over `http://`, so their addresses must be reachable from vartop. The `server add` wizard asks for host and port; edit the config file to change `path`.

## Environment

What vartop web can reach depends on the environment you start it in: the config path (`XDG_CONFIG_HOME` or `HOME`), proxies, `VARTOP_DATA_DIR`. Two `vartop web` that differ in config path share the data directory but not each other's configuration: see *Another configuration is refused* under [Safety](how-it-works.md#safety).
