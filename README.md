# Redirect

Redirect is a small Go service that routes HTTP requests to healthy upstream
servers. It chooses a routing group from a cookie or the client's country,
selects an available server from that group, and redirects the client while
preserving the original request URI.

## Architecture

The service is a single process with two main execution paths:

- The Echo HTTP server handles routing, status, and group-selection requests.
- Background goroutines periodically check the health of configured upstream
  servers.

```text
Client
  |
  v
Echo router (main.go)
  |
  +-- GET /* ------> group selection ------> load balancing ------> redirect
  +-- POST /* -----> runtime status
  +-- PUT /:group -> set group cookie
  +-- DELETE / ----> clear group cookie
                          |
                          v
                 groupManager (server.go)
                    |             |
                    v             v
               IP database    servers/groups
                                      |
                                      v
                              health-check goroutines
```

### Components

#### HTTP layer

`main.go` is the entry point. It reads command-line options and configuration,
initializes the shared `groupManager`, starts health monitoring, and registers
the Echo routes.

The redirect handler resolves a group in this order:

1. The `group` cookie, if it names a configured group or server.
2. A group or server whose name matches the client's country from the IP
   database.
3. The required `main` group.

It then asks the selected group for a healthy server and redirects to the
server's base URL plus the original request URI. The redirect status is chosen
from the server configuration, then the global configuration, and finally
defaults to `307`.

#### Group manager

`groupManager` in `server.go` owns the runtime registry of servers and groups.
It parses the top-level JSON configuration, loads the IPIP.net `datx` city
database, constructs configured objects, and performs cookie- and IP-based
lookups.

Both individual servers and groups implement the internal `balanceGroup`
interface. This lets groups refer to either servers or other groups.

#### Load balancing

A group supports two selection strategies:

- `fallback` (the default) tries members in descending weight order and returns
  the first healthy server.
- `random` performs weighted random selection among healthy members. If it
  cannot select one after repeated attempts, it tries the highest-weight member.

Selection within a group is protected by a mutex. Each successful server
selection increments that server's in-memory request count.

#### Health checks

Each configured server starts a watcher goroutine unless its `Check` setting is
disabled. Every ten seconds the watcher sends `GET <server URL>/generate_204`.
A server is considered healthy only when that request returns HTTP `204`; failed
or unexpected responses mark it offline, and later successful checks restore
it.

Health state, last successful check time, and redirect counts are held in
memory and are reset when the process restarts.

## Configuration

The service reads `config.json` by default. `Servers` defines redirect targets,
while `Groups` composes targets and assigns selection weights.

```json
{
  "RedirectType": 307,
  "Servers": {
    "primary": {
      "URL": "https://primary.example.com",
      "RedirectType": 308,
      "Check": true
    }
  },
  "Groups": {
    "main": {
      "Type": "fallback",
      "Servers": {
        "primary": 1.0
      }
    }
  }
}
```

Important configuration rules:

- A `main` group or server must exist as the final routing fallback.
- Group member names must refer to configured servers or groups.
- Country-based routing uses the country string returned by the IP database as
  a registry name.
- `Check` defaults to `true` when omitted.
- A server-level `RedirectType` overrides the top-level `RedirectType`.

## HTTP interface

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/*` | Select an upstream and redirect the request |
| `POST` | `/*` | Return JSON runtime status for all registered objects |
| `PUT` | `/:group` | Set the `group` cookie for one year |
| `DELETE` | `/` | Expire the `group` cookie |

## Startup and runtime

The executable accepts:

| Option | Default | Purpose |
| --- | --- | --- |
| `-c` | `./config.json` | JSON configuration path |
| `-d` | `./data.ipx` | IPIP.net database path |
| `-p` | `1080` | HTTP listen port |
| `-verbose` | `false` | Log redirect decisions |

Build the service with:

```sh
make
```

This creates `build/redirect`. At runtime, the configured IP database must be
available locally:

```sh
./build/redirect -c ./config.json -d ./data.ipx -p 1080
```

## Repository layout

| Path | Responsibility |
| --- | --- |
| `main.go` | Process startup, CLI flags, Echo routes, and HTTP handlers |
| `server.go` | Configuration model, group selection, server state, and health checks |
| `config.json` | Example server and group configuration |
| `Makefile` | Binary build |
| `go.mod` | Go module and dependency definitions |
