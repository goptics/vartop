# Security policy

## Report a vulnerability

Do not open a public issue for a security problem. Report it privately through [GitHub security advisories](https://github.com/goptics/vartop/security/advisories/new). Give the steps that show the problem and what an attacker can do with it.

## Supported versions

vartop has no release yet. Security fixes go to `main`.

## Security model

vartop can stop, resize and cancel the work of every VarMQ instance that it reaches, and it holds the bearer tokens for them. These rules protect that access:

- **The dashboard listens on loopback only.** It has no login, so vartop refuses any `--host` that is not a loopback address, and it answers only requests addressed to its own loopback host name.
- **Tokens stay in vartop.** vartop calls VarMQ for the page, and it never sends a token to the browser. Tokens are in the config file (`0600`), never in the data directory. vartop sends a token to VarMQ only in the `Authorization` header.
- **Actions use only current data.** vartop refuses an action, and sends nothing, when the data is stale or was collected under another configuration. So a token never goes to an endpoint that the config no longer lists.
- **The hub listener needs a token for every request.** It is the only listener that can bind a public address. Use TLS on it.

Details: [Safety](docs/how-it-works.md#safety) for the dashboard, and [Hub mode](docs/hub.md) for the hub listener, its limits and its trust model.

VarMQ's REST API itself has no authentication. Protect it as the [VarMQ docs](https://github.com/goptics/varmq#rest-api) say.
