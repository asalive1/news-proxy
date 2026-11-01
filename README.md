# News-Proxy

A small, self-contained service that caches news feeds (RSS/Atom) so widgets read from one local cache instead of every client hitting the internet. One Go codebase, no runtime dependencies, free and open source tooling only.

| Endpoint | Purpose |
|---|---|
| `/health` | JSON health check (status, version, uptime, source counts, last/next refresh) |
| `/config` | Web UI: add/edit/remove sources, set refresh interval, optional admin password |
| `/sources.json` | Discovery document the widget downloads (id, name, url, group, categories, `proxyPath`) |
| `/feed/<id>` | Cached feed for a source (also `/<id>` and legacy aliases such as `/bbc`, `/npr`) |

Sources added in `/config` (or by editing `sources.json`) are live within seconds: **no restart**. The cache refreshes every 10 minutes by default (configurable in `/config`). Cached content survives restarts and is served stale if an upstream is down.

## Windows

Run `News-Proxy-Setup.exe` (self-contained). It asks for administrator rights and opens a setup page in your browser:

* **Not installed:** choose port, bind address, update interval, optional admin password, firewall rule, then **Install**. This registers the Windows service **News-Proxy** (automatic start, runs as Local Service, restarts on failure) in `C:\Program Files\News-Proxy`; data is in `C:\ProgramData\News-Proxy`.
* **Installed:** run the same EXE (or the *News-Proxy Setup* Start Menu shortcut) to change options, start/stop/restart, or **Uninstall** (optionally deleting settings and cache).

Command line (elevated): `News-Proxy-Setup.exe install --port 7999 --interval 10 [--password ...] [--bind ...] [--allow-private] [--no-firewall]`, `News-Proxy-Setup.exe uninstall [--remove-data]`.
A port or bind-address change restarts the service automatically; everything else is applied live.

## Linux / Docker (Ubuntu Server 24.04)

```
docker compose up -d --build
```

The compose file builds the **latest `main` from GitHub** (`https://github.com/asalive1/news-proxy.git#main`); run the same command again at any time to update. Then open `http://<server>:7999/config`. Data persists in the `news-proxy-data` volume. Environment (see `docker-compose.yml`): `NEWS_PROXY_PORT` (host port), `ADMIN_PASSWORD`, `REFRESH_INTERVAL_MINUTES` and `ALLOW_PRIVATE_FEEDS` (first run only), `TZ`.
To publish through Nginx Proxy Manager (TLS, e.g. `https://news.example.com`), uncomment the `nginx-proxy-manager_default` network in the compose file and point the proxy host at `news-proxy:7999`.

### Updating

`docker compose up -d` always re-fetches `main` from GitHub and rebuilds (`pull_policy: build`); unchanged code is served from the Docker build cache, so it is fast. The repository is public (it contains no secrets) so the server needs no credentials. If you make it private, the server must provide Git credentials to Docker (BuildKit `GIT_AUTH_TOKEN` secret), or clone the repo and set `context: .`.

## Version

The version lives in the `VERSION` file (currently 1.0.1), is built into both the Windows and Linux programs, and is shown in the `/config` header, the installer page, `/health` and `news-proxy version`. Bump it before publishing.

## Publishing the Linux version (developer)

`SendtoGit.ps1` (run from a PowerShell terminal in this folder, with `$env:GITHUB_TOKEN` set) creates the public `asalive1/news-proxy` repository (use `-Private` for private) if needed and pushes **only the Linux files** to `main`. Windows sources (`*_windows.go`, `web/installer.html`), `build.ps1`, `dist/` and the script itself are never published. Use `-DryRun` to see the file list without pushing.

## Widget

Set the widget catalog's `proxyBase` to the proxy's URL (e.g. `http://server:7999` or your HTTPS name). The existing `proxyPath` values keep working through legacy aliases for the original 8 nginx feeds.

## Security notes

* `/config` has no password by default. Set one (user `admin`, 8+ characters) when the service is reachable by others.
* Feeds on localhost/LAN addresses are blocked unless "Allow private/internal feeds" is enabled; link-local addresses are always blocked (SSRF protection). Only http/https URLs are accepted.
* Configuration changes require an `X-Requested-With` header (CSRF protection).

## Build

`powershell -ExecutionPolicy Bypass -File build.ps1` (needs Docker only) tests and produces `dist\News-Proxy-Setup.exe` and the `news-proxy` Docker image.
