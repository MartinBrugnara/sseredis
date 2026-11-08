# sseredis

Proxies Redis channels as HTTP SSE endpoints (thus read only).

It's also provide a broker to authenticate against active django sessions.

## Running

```sh
go build && ./sseredis -redis redis:6379 -listen :80 -dmbcau http://dmbcau/authenticator/
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-redis` | `redis:6379` | Redis server address. |
| `-listen` | `:80` | Address to bind. |
| `-dmbcau` | `http://dmbcau/authenticator/` | Django authenticator API (see [Authentication](#authentication)). |
| `-auth-timeout` | `5s` | Timeout of a call to the authenticator API. |
| `-origin` | `*` | `Access-Control-Allow-Origin`. |
| `-log` | `INFO` | `DEBUG`, `INFO`, `WARNING`, `ERROR` or `FATAL`. |

Tests: `go test -race ./...` (Redis is simulated with miniredis, nothing to install).

## Design

A client opens `GET /?subscribe=chan1,chan2`. After [authentication](#authentication) it receives, as SSE
`data:` events, the messages published on those Redis channels, plus a `:heartbeat` comment every 10s.

- One goroutine (the `Hub`, `hub.go`) owns the clients and the Redis subscriptions; it subscribes to a channel
  when its first client arrives and unsubscribes when the last one leaves.
- It never waits for a client: each client has a queue of 64 messages, and a client whose queue is full
  (stalled or gone) is dropped. Its browser `EventSource` reconnects by itself.
- If the Redis connection fails, `redisLoop` (`redis.go`) reconnects with exponential backoff (1s, 2s, ... up
  to 30s) and subscribes again to all the channels in use. Messages published meanwhile are lost.

## Authentication

Authentication goes through the `Authenticator` interface (`auth.go`), so another backend can replace Django.
`DjangoAuth` reads the `sessionid` cookie and calls the authenticator API:

```
GET <-dmbcau>?sessionid=<cookie>&clientip=<client IP>
```

`200` accepts the client. A `4xx` refuses it with the same status; anything else (including redirects, which
are not followed, and timeouts) refuses it with `502`/`500`.

## Client IP

The client IP is logged and sent to Django (`clientip`). In production both Django and sseredis are behind
nginx, so the TCP peer is nginx and the client IP comes from `X-Forwarded-For`.

`X-Forwarded-For` is the de-facto standard for this, and it works with several proxies: each proxy appends
the address it got the connection from, so the header reads `client, proxy1, proxy2`. The catch is that the
client can send the header too, and a proxy that appends keeps what the client wrote as the first entry. Only
the entries written by our own proxies can be trusted.

The rule (`clientIP` in `ip.go`):

- The **public-facing nginx overwrites** `X-Forwarded-For` with the address it sees, dropping whatever the
  client sent. Proxies after it (if any) append. So the header holds only addresses written by our
  proxies, and its **first entry is the client**.
- sseredis takes the first entry. Without the header (or if it is not an IP) it uses the address of the peer
  that connected to it.
- Django must compute the client IP the same way, or the IP sseredis sends will not match the session's.

What nginx must do:

1. On the public-facing nginx, for **both** the sseredis and the Django locations:

   ```nginx
   proxy_set_header X-Forwarded-For $remote_addr;   # overwrite, never $proxy_add_x_forwarded_for
   ```

2. On any proxy between it and sseredis/Django: append.

   ```nginx
   proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
   ```

3. If a load balancer or CDN sits in front of the public nginx, `$remote_addr` is the balancer. Make nginx
   take the client from the balancer's header first ([realip module](https://nginx.org/en/docs/http/ngx_http_realip_module.html)),
   trusting only the balancer's addresses, then keep rule 1:

   ```nginx
   set_real_ip_from 10.0.0.0/8;          # the balancer's addresses only
   real_ip_header   X-Forwarded-For;
   real_ip_recursive on;
   ```

4. sseredis (and Django) must be reachable **only through nginx** (bind to the internal network, or
   firewall it). Anyone who can connect directly can send any `X-Forwarded-For`.
