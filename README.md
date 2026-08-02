# redirect

`redirect` selects a configured proxy/cache server and returns an HTTP redirect
to the original resource path. Existing clients can continue to use the legacy
cookie, IP-country group, and `main` group selection behavior.

Phase 1 adds an optional route-resolution API. A client can ask `redirect` to
select a backend before loading a resource and receive a resource-bound signed
route token. The token pins subsequent video/image requests to that backend
without changing the resource pathname.

## Build and test

The module currently targets Go 1.14 syntax and builds with newer Go releases.

```bash
go mod verify
go test ./...
go test -race ./...
go build -o build/redirect .
```

The executable flags remain unchanged:

```text
-c ./config.json  configuration path
-d ./data.ipx     IPIP DATX city database
-p 1080           listen port
-verbose          verbose request logging
```

## Legacy API

| Request | Behavior |
| --- | --- |
| `GET /*` | Select a backend and redirect to it. |
| `HEAD /*` | Select a backend and redirect to it. |
| `POST /*` | Return legacy runtime status. |
| `PUT /:group` | Store a group or direct server ID in the `group` cookie. |
| `DELETE /` | Delete the `group` cookie. |

Tokenless selection precedence remains:

1. `group` cookie
2. Group matching the IP country lookup
3. `main` group

When routing is omitted or has `"Enabled": false`, route APIs are not
registered and `__mira_route` has no special meaning. Existing request URLs are
forwarded unchanged.

## Routing configuration

Routing is additive to the existing `Servers`, `Groups`, and `RedirectType`
configuration:

```json
{
  "RedirectType": 307,
  "Servers": {
    "jp-oracle": {
      "URL": "https://jp-cache.example",
      "Label": "Japan - Oracle",
      "Region": "JP",
      "Selectable": true,
      "Check": true
    }
  },
  "Groups": {
    "main": {
      "Type": "random",
      "Servers": {
        "jp-oracle": 1.0
      }
    }
  },
  "Routing": {
    "Enabled": true,
    "QueryParameter": "__mira_route",
    "FreshTTL": "24h",
    "MaxTTL": "168h",
    "ActiveSigningKeyID": "2026-08",
    "SigningKeyEnvironments": {
      "2026-08": "MIRA_ROUTE_SIGNING_KEY_2026_08"
    },
    "PublicOrigin": "https://media.example",
    "AllowedOrigins": [
      "https://mira.example"
    ],
    "AllowedResourcePrefixes": [
      "/video/",
      "/pic/"
    ],
    "RequestsPerMinute": 120
  }
}
```

### Server metadata

| Field | Default | Meaning |
| --- | --- | --- |
| `Label` | Server ID | Safe user-facing backend name. |
| `Region` | Empty | Optional region code shown to clients. |
| `Selectable` | `true` | Whether the server may be listed or selected by the route API. |

`URL` is never returned by the routing API.

### Routing fields

| Field | Default | Meaning |
| --- | --- | --- |
| `Enabled` | `false` | Enable route APIs and token-aware redirects. |
| `QueryParameter` | `__mira_route` | Reserved route-token query parameter. Only RFC 3986 unreserved characters are accepted. |
| `FreshTTL` | `24h` | Informational freshness time returned to clients. |
| `MaxTTL` | `168h` | Hard token lifetime. Must be at least `FreshTTL`. |
| `ActiveSigningKeyID` | None | Key ID used to sign new tokens. Required when enabled. |
| `SigningKeyEnvironments` | None | Map of accepted key IDs to environment-variable names. |
| `PublicOrigin` | Request origin | Public redirect origin used in absolute playback URLs. Configure this when TLS or host rewriting occurs before `redirect`. |
| `AllowedOrigins` | Same origin only | Exact additional browser origins allowed to call the routing API. |
| `AllowedResourcePrefixes` | `/video/`, `/pic/` | Resource paths for which routes may be issued. |
| `RequestsPerMinute` | `120` | Combined route API limit per direct network peer. |

`PublicOrigin` and every `AllowedOrigins` value must contain only an `http` or
`https` scheme and host. Paths, query strings, credentials, and fragments are
rejected at startup.

The current rate limit intentionally uses the direct peer address so an
untrusted client cannot bypass it with `X-Forwarded-For`. If `redirect` is
behind one shared reverse proxy, choose a suitable limit for that deployment.
Trusted-proxy-aware client extraction is planned with geographic/ASN routing.

## Signing keys

Generate at least 32 random bytes and store the Base64 value outside the
configuration file:

```bash
export MIRA_ROUTE_SIGNING_KEY_2026_08="$(openssl rand -base64 32)"
```

Every `redirect` replica must receive the same active key set. To rotate keys:

1. Add a new key ID/environment entry to every replica.
2. Deploy the new key set while retaining the old entry.
3. Change `ActiveSigningKeyID` to the new ID.
4. Keep the old verification key for at least `MaxTTL` after the final old token
   was issued.
5. Remove the old entry and secret.

The token contains a version, key ID, random route ID, backend ID, resource
hash, issue time, freshness time, and expiration. It contains no account ID,
client IP, backend URL, or resource authorization.

## Routing API

### List backends

```http
GET /_mira/routing/v1/backends
```

```json
{
  "version": 1,
  "backends": [
    {
      "id": "jp-oracle",
      "label": "Japan - Oracle",
      "region": "JP",
      "availability": "healthy",
      "selectable": true
    }
  ]
}
```

### Resolve automatically

```http
POST /_mira/routing/v1/routes
Content-Type: application/json
```

```json
{
  "resource": "/video/bangumi-id/episode.mp4?quality=source",
  "preference": {
    "mode": "auto"
  },
  "excludeBackendIds": []
}
```

### Prefer a backend

```json
{
  "resource": "/video/bangumi-id/episode.mp4?quality=source",
  "preference": {
    "mode": "backend",
    "backendId": "jp-oracle"
  },
  "excludeBackendIds": []
}
```

`excludeBackendIds` supports up to 16 server IDs and is intended for
playback-local failover. A manual backend is preferred, not forced: when it is
offline or excluded, route resolution selects a healthy legacy fallback and
returns reason `preferred-backend-unavailable`.

Example response:

```json
{
  "routeToken": "payload.signature",
  "playbackUrl": "https://media.example/video/bangumi-id/episode.mp4?quality=source&__mira_route=payload.signature",
  "selectedBackend": {
    "id": "jp-oracle",
    "label": "Japan - Oracle",
    "region": "JP",
    "availability": "healthy",
    "selectable": true
  },
  "selection": {
    "mode": "auto",
    "reason": "main-group"
  },
  "issuedAt": "2026-08-02T12:00:00Z",
  "freshUntil": "2026-08-03T12:00:00Z",
  "expiresAt": "2026-08-09T12:00:00Z"
}
```

Route requests accept only same-origin absolute paths under an allowed prefix;
they do not fetch a submitted URL. Unknown JSON fields, multiple JSON values,
oversized bodies, invalid preference modes, and excessive exclusions are
rejected.

## Token-aware redirect behavior

For this request:

```text
/video/bangumi-id/episode.mp4?quality=source&__mira_route=TOKEN
```

`redirect` verifies that the token is signed, unexpired, and bound to the
normalized resource. It then removes every reserved route parameter before
constructing the redirect:

```http
HTTP/1.1 307 Temporary Redirect
Location: https://jp-cache.example/video/bangumi-id/episode.mp4?quality=source
Cache-Control: private, no-store
X-Mira-Backend: jp-oracle
X-Mira-Route: pinned
```

Non-routing query data is preserved in its original order and encoding. The
route token never reaches the proxy/cache or origin.

Media-path token failures are deliberately tolerant:

- A tampered, malformed, resource-mismatched, expired, or removed-backend token
  falls back to the legacy selection chain.
- The reserved query parameter is still removed before redirecting.
- An expired routing hint never makes a public resource inaccessible.

The `X-Mira-Route` response value is `pinned`, `fallback`, or `legacy`.

## CORS

Routing endpoints accept requests with no `Origin`, same-origin requests, and
origins listed exactly in `AllowedOrigins`. Supported preflight methods are
`GET`, `POST`, and `OPTIONS`; the only required request header is
`Content-Type`.

No cookie or OAuth credential is required. The signed query token allows the
media element to remain anonymous in a cross-origin deployment. Proxy/cache
nodes must continue to provide whatever CORS headers the final media response
already requires.

## Phase 1 boundary

Phase 1 makes a selected route visible and stable, but automatic route creation
still chooses from the existing cookie, country group, and `main` group using
their current `random` or `fallback` algorithms. It does not yet implement:

- Country plus ASN cohorts
- Weighted rendezvous hashing
- Proxy-to-origin throughput scoring
- Client playback event ingestion
- Signed ordered fallback lists
- Shared metrics between redirect replicas

Those can be added behind the route API without changing canonical media paths
or breaking clients that do not send a token.