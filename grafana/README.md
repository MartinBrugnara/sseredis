# Grafana dashboard

- `sseredis.json`: the dashboard (Dashboards > New > Import, then pick the Prometheus data source). See
  [Monitoring](../README.md#monitoring) for the metrics and the scrape setup.
- `alerts.yaml`: the alert rules (Grafana file provisioning): sseredis down, Redis disconnected or flapping,
  authentication errors (over 5%) or slow (p95 over 1s), slow clients dropped. Installing: the top of the file.
- `sseredis.png`: a screenshot of the dashboard, with the simulated workload below.

## Workload of `sseredis.png`

30 minutes (2026-11-08, 04:46 to 05:15) of a single sseredis on FreeBSD, scraped every 5s. A load generator
simulated:

- **Clients:** about 30 open streams. Each subscribed to each of `planner`, `reservations`, `invoices` and
  `stats` with a 50% chance (`planner` if none), so about 2 channels per stream. A stream stayed open 5s to
  3 minutes (1 in 10 only 1s); a new one opened every 50-350ms while there were fewer than 30.
- **Stalled clients and bad requests:** every 20-60s, one client that subscribes to `planner` and never reads
  (it hangs up after 2 minutes; the hub drops it during the next burst), and one request without channels
  (`no_channels`).
- **Messages:** one every 100-500ms, 3/7 on `planner`, 2/7 on `reservations`, 1/7 each on `invoices` and
  `stats`. Every 200 messages, a burst of 80 messages of 32 KiB on `planner`. 1 in 30 went to a channel
  nobody listens to, which Redis does not deliver to sseredis (so `Unrouted messages` stays at 0).
- **Authenticator (fake Django):** answered in 5-200ms (1 in 50 in 1.5s, the p95/p99 spikes): 85% `200`,
  13% `403` (refused), 2% `500` (auth errors).
- **Redis:** an in-process Redis ([miniredis](https://github.com/alicebob/miniredis)), stopped for 20s every
  10 minutes.

Things that come from the setup, not from sseredis:

- CPU and open file descriptors are empty: there are no `process_*` metrics on FreeBSD.
- `Dropped clients (1h)` also counts the drops of an earlier run within that hour.
