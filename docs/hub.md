# Hub mode

In hub mode, the VarMQ instances connect to vartop. vartop does not need their addresses, and the instances need no inbound port. Use it when pods are short-lived, or when vartop cannot reach them.

A hub server has `"hub": true` and a `token` of at least 16 characters. Two hub servers cannot share a token, because the token selects the server.

```json
{
  "servers": {
    "jobs": {
      "name": "jobs",
      "hub": true,
      "token": "a-long-random-secret-of-16-or-more-characters"
    }
  }
}
```

`vartop server add` asks for the token. If you leave it empty, it makes a random token and prints it. If you type a token, it does not print it.

Start vartop web with a hub listener. The hub listener can bind any address, but the dashboard stays on loopback.

```bash
vartop web --hub-listen :7071 --hub-tls-cert hub.crt --hub-tls-key hub.key
```

vartop web prints the hub address when it starts. If a hub server is configured and `--hub-listen` is not given, vartop web says so, and the server's snapshots show the cause.

In your app, connect each VarMQ instance to the hub with the server's token:

```go
go func() {
	err := varmq.ConnectHub(ctx, "https://vartop.internal:7071", token,
		varmq.WithHubErrHandler(func(err error) { log.Printf("hub: %v", err) }),
	)
	log.Printf("hub connection stopped: %v", err) // a bad config returns at once
}()
```

`WithHubErrHandler` receives the connection errors, such as a rejected token. VarMQ refuses plain `http://` to a host that is not loopback, because the token would travel in clear text. To allow it on a trusted network, add `varmq.WithHubInsecure()`.

Two more options limit what vartop can do with an instance:

| Option | Effect |
| --- | --- |
| `varmq.WithHubReadOnly()` | The instance refuses every request but `GET` with status 403. vartop can show it, but every action on it fails. |
| `varmq.WithHubNoPayloads()` | The job routes send each job with payload `null` and no result. Job error texts and workflow task reasons are still sent, so keep application data out of them. |

`ConnectHub` needs VarMQ v1.11.0, which is not released yet. Until then, use `go get github.com/goptics/varmq@main`. The wire protocol is [`spec/hub.md`](https://github.com/goptics/varmq/blob/main/spec/hub.md) in the VarMQ repository.

Each instance keeps one stream open to vartop. vartop sends its usual API requests down the stream, and the instance posts the replies back. The pages, actions and history work as for other servers.

Each instance is one unit, and vartop never treats it as a load balancer. An action reaches the right instance on the first try. vartop lists an instance after its first heartbeat, which the instance sends at once. An instance that disconnects leaves the fleet at once. An instance that stops its heartbeats leaves after 3 to 4 heartbeats of 5 seconds each. You cannot change this period. The pages show each change at their next poll.

vartop web follows changes to the config while it runs. A new hub server accepts instances within a few seconds. A removed server, or a changed token, disconnects its instances. If the config cannot be read, the hub servers keep their last tokens, and vartop web logs a warning.

`vartop action` and `vartop concurrency` run in their own process, so they cannot reach hub instances. For a hub server, they fail and point to the dashboard.

Limits:

- Each hub server accepts at most 1000 instances. It accepts a burst of 200 new instance streams, and then 20 each second.
- The hub listener holds at most 10 connections for each instance that the hub servers accept, plus 256. With one hub server, that is 10,256 connections. Each hub server has its own share of 10 authenticated connections per instance. Past its share, the hub closes new connections of that server, and other servers are not affected.
- The hub listener holds at most 64 tokenless connections per source IP. A connection is tokenless until it sends a request with a valid token. Connections that sent a valid token do not count, so many instances can share one proxy or NAT address. When a 65th tokenless connection arrives, the hub closes the oldest tokenless connection of that IP. At the listener's connection limit, a new connection closes the oldest tokenless connection of any IP, so tokenless clients of many addresses cannot lock out the instances. Only when every connection is authenticated is a new one refused. Set `--hub-max-conns-per-ip` to change the limit, or to `0` for no limit.
- Set `--hub-max-conns-per-ip` to `0` behind a reverse proxy that opens a new upstream connection for each request. If you are not sure, set `0`.
- Known limit: a client on the same source address as the instances can still open connections very fast. The same host, a NAT or a proxy address counts as one source address. Such a client can disturb new connections from that address while their TLS handshake or request headers are in progress. Live streams and authenticated connections are not affected.
- A connection is authenticated when it carries an accepted stream, heartbeat or reply. A valid token alone is not enough. The hub closes a connection that is not authenticated within 10 seconds. A request with a valid token that is still in progress at that time delays the close until the request ends. An idle connection closes after 30 seconds.
- Each heartbeat and reply must arrive in full within 10 seconds.
- Each hub server reads at most 256 MiB of replies at once, and each reply is at most 16 MiB. A reply of unknown length counts against this limit in steps of 64 KiB as it arrives. A reply over 16 MiB gets status 413, and one that does not fit in the 256 MiB gets status 503; either way its request fails.
- At the instance limit or the new-stream rate, the hub refuses the stream with status 503, and the instance tries again later.
- vartop reads at most 2000 workers of one instance. An instance that lists more shows the error *too many workers*. This limit applies to every server, not only to hub servers. vartop fetches the workers of a hub instance 8 at a time, and the workers of other instances 64 at a time. Behind a load balancer, vartop fetches all listed workers at once.

Security:

- **Whoever answers at the hub URL controls the instances.** That party can read the data of the instances and stop their workers. It is the vartop host, or, without TLS, any party on the network path.
- **The token proves the instances, not vartop.** A holder of the token can connect false instances to its server, and can hold an instance ID that is not in use. It cannot read or control a real instance. Keep the token secret, and use a different token for each server.
- **Each stream has its own session.** The hub gives each stream a secret session, and heartbeats and replies must carry it. So an instance cannot disconnect another instance, or answer for it.
- **Use TLS or a trusted network.** Without TLS, the token travels in clear text. Use `--hub-tls-cert` and `--hub-tls-key`, or TLS at a proxy. vartop web warns when a hub listener on a non-loopback address has no TLS.
- **The token stays out of URLs and logs.** vartop reads the token only from the `Authorization` header, and never writes it to its log.
- **Run one vartop replica.** An instance connects to one vartop, so a second replica cannot reach it.
- **The dashboard stays on loopback.** It has no login, so only the hub listener can bind a public address. Every request to the hub listener needs a token.
- **Proxies must not buffer the stream.** A proxy between the instances and vartop must pass each line at once. It must also keep idle connections open for longer than a heartbeat.
