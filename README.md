# Conduit

<p align="center">
  <a href="https://github.com/Oranda-IO/Conduit/stargazers"><img src="https://img.shields.io/github/stars/Oranda-IO/Conduit?style=social" alt="GitHub Stars" /></a>&nbsp;
  <img src="https://img.shields.io/github/license/Oranda-IO/Conduit" alt="License" />&nbsp;
  <img src="https://img.shields.io/badge/version-0.9.0-blue" alt="Version" />&nbsp;
  
</p>

<p align="center">
  <img src="./conduit.png" alt="Conduit" width="80" />
</p>

<h1 align="center">Conduit</h1>

<p align="center">
  <strong>Quick Automatic Port Forwarding for Docker Containers</strong>
  <br/>
  Use a single exposed port to access any service running on your container.
</p>

---

`conduit` watches local listening TCP ports and forwards HTTP requests by path:

- Incoming: `http://<host>:<public_port>/<internal_port>/<path...>`
- Forwarded to: `http://127.0.0.1:<internal_port>/<path...>`

Example:

- `http://myserver.com:9000/3000/api/users` -> `http://127.0.0.1:3000/api/users`

## Install

1. Install Go (1.19+).
2. Build:

```bash
go build -o conduit .
```

## Run

```bash
./conduit -public-host 0.0.0.0 -public-port 9000
```

Useful flags:

- `-public-host` bind host (default `0.0.0.0`)
- `-public-port` bind port (default `9000`)
- `-target-host` upstream host (default `127.0.0.1`)
- `-poll-interval` rescan interval (default `2s`)

## Example

**Continaer Terminal**

Run the demo service on internal port `3000`:

```bash
go run ./demo/echo -port 3000
```

**Continaer Terminal**

Run the `conduit` daemon:

```bash
./conduit -public-host 0.0.0.0 -public-port 9000
```

**Host Terminal**

Access the container service from the host:
```bash
curl "http://localhost:9000/3000/ping?name=alice"
```

## API

### `GET /health`

Health check.

Response:

```json
{"status":"ok"}
```

### `GET /ports`

Returns currently discovered listening local ports.

Response example:

```json
{
  "ports": [22, 3000, 5432],
  "count": 3,
  "updated_at": "2026-02-27T03:45:00Z"
}
```

### Proxy Route `/<internal_port>/<path...>`

Conduit validates `internal_port` is currently listening, then proxies request/response.

Examples:

- `GET http://myserver.com:9000/8080/` -> `http://127.0.0.1:8080/`
- `GET http://myserver.com:9000/8080/api/ping` -> `http://127.0.0.1:8080/api/ping`

If internal port is not active, conduit returns:

- `502 Bad Gateway`

## Testing

```bash
go test ./...
```

## Notes

- Current implementation routes HTTP traffic only.
- Linux-oriented: uses `/proc/net/tcp` and `/proc/net/tcp6` for discovery.
- In containers, publish/forward the public port (for example `9000:9000`) so host requests can reach conduit.
