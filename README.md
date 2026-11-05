# DevPanel

A local web development stack manager for Windows, in the spirit of Laragon, Laravel Herd and FlyEnv:
start/stop services, `*.test` sites with trusted HTTPS, PHP per site, one-click installs of PHP, databases
and runtimes, and an MCP server so AI coding tools can set up a project for you.

It is made for people who **don't want to move their environment again**: it runs on top of an existing
Laragon folder, or standalone and **links what XAMPP, WampServer, Laravel Herd, FlyEnv or Laragon already
installed** (PHP versions, MySQL/MariaDB, PostgreSQL, Apache, Nginx, Redis, Node, ...), using their binaries,
databases and projects as they are. Installer or portable.

> Status: v1.0.0, early. Tested on Windows 11. Not affiliated with Laragon, Laravel Herd or FlyEnv
> (see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)).

---

## Features

**Services** (grouped like FlyEnv; tick the ones you use, install missing ones in place)
- Web & app servers: Apache, Nginx, Caddy, FrankenPHP (one runs at a time; they share ports 80/443)
- Databases: PostgreSQL, MySQL / MariaDB, MongoDB
- Cache & messaging: Redis, Memcached, RabbitMQ (with Erlang)
- Search: Meilisearch, Elasticsearch · AI: Ollama · Mail: Mailpit
- Start / Stop / Start All, groups ("PG + Apache"), watchdog restart, Windows notifications
- Per-service detail: version switch, install other versions, config & log files, tools (`httpd -t`, `psql`, ...)

**Languages & runtimes**
PHP, Node.js, Go, Python, Java (Temurin), .NET SDK, Ruby, Bun, Deno, Erlang: install, switch versions,
put them on the DevPanel terminal / Windows PATH (with backup). PHP: extensions on/off, quick `php.ini`
settings, Xdebug in one click, `mail()` to Mailpit.

**Sites**
- One site per folder in `www`, hostname `<folder>.test`, auto-synced every 5 s
- Naming rules (prefix/suffix rewrite, e.g. `app_shop` -> `shop.test`), custom hostnames, aliases, custom document root
- **PHP version per site** (Apache via `FcgidCmdOptions`, Nginx/Caddy via php-cgi pools)
- **Reverse proxy per site** to a dev server (Vite, Next, Go, ...) with websockets
- Trusted HTTPS from a local CA that is name-constrained to `.test`, `localhost` and `127.0.0.1`
- Framework detection, `APP_URL` check, Quick App (Laravel, WordPress, Symfony, CodeIgniter 4, CakePHP, Drupal, blank)
- Share a site through ngrok, rename / delete projects (delete = Recycle Bin)

**Databases**: list, create, backup, restore, drop (drop/restore need the name typed again),
dump/restore all for PostgreSQL major upgrades.

**Processes**: queue workers, `npm run dev`, Reverb, ... per project, with live logs and autostart.

**Install**: Laragon's `packages.conf` when present, built-in catalog, extra sources, update check
for PHP/Node with SHA-256 verification, `.zip`/`.tar.xz`/`.7z` (via Windows' own `tar.exe`).

**AI (MCP)**: `devpanel.exe mcp` exposes 34 tools (status, start/stop, sites, PHP per site, databases,
processes, requirements check, installs, ...). See [MCP](#mcp-for-ai-coding-tools).

Also: tray icon, start with Windows, CLI, export/import settings, light/dark theme, English / Bahasa Indonesia UI.

---

## Install

Download from the [Releases](../../releases) page:

| File | Use |
|---|---|
| `DevPanel-Setup-<version>.exe` | Installer: per user, no admin. Adds Start Menu/Desktop shortcuts and an *Apps & features* entry. If Laragon is found in `C:\laragon`, it offers to install next to it. |
| `DevPanel-<version>-portable.zip` | Portable: unzip anywhere (also a USB drive) and run `devpanel.exe`. |

Uninstall from *Apps & features* (or `devpanel.exe --uninstall`). Projects (`www`), databases (`data`)
and installed services (`bin`) are **kept**.

### Two modes

| `devpanel.exe` lives in | Mode | Uses |
|---|---|---|
| a Laragon folder, e.g. `C:\laragon\devpanel\` | Laragon-compatible | `C:\laragon\bin`, `www`, `etc`, `data`; Laragon's own config files are left alone |
| any other folder, e.g. `C:\DevPanel\` | Standalone | its own `bin`, `www`, `data`, `etc`, `backup` next to the exe; it writes its own Apache/Nginx/Caddy config |

Don't run Laragon and DevPanel at the same time on the same folder: both would manage the same services.

### Use what you already have (XAMPP, Herd, FlyEnv, WampServer, Laragon)

**Settings → Other environments on this PC → Scan** finds their installs by their binaries:

| Environment | Looked up in |
|---|---|
| Laragon | `C:\laragon\bin` (projects `C:\laragon\www`) |
| XAMPP | `C:\xampp` (projects `htdocs`) |
| WampServer | `C:\wamp64\bin`, `C:\wamp\bin` (projects `www`) |
| Laravel Herd | `%USERPROFILE%\.config\herd\bin` (projects `%USERPROFILE%\Herd`) |
| FlyEnv | its data folder `app` (`%APPDATA%\FlyEnv`, portable `FlyEnv-Data`) and extra binary folders it knows |

**Link** creates a folder link (junction) under DevPanel's `bin\<service>\`: nothing is copied or moved,
and their files stay theirs. A linked Apache runs with DevPanel's own config (`httpd -f`), so e.g. XAMPP's
`httpd.conf` is never edited; Nginx/Caddy always use DevPanel's config and log folders; a linked MySQL or
PostgreSQL keeps its existing data folder and databases. **Add as projects folder** also serves e.g. XAMPP's
`htdocs` or `~/Herd` as `*.test` sites. Unlinking removes only the link.

This works in both modes (standalone DevPanel can link Laragon's installs too), and several environments
can be used at once, e.g. PHP from Herd, MySQL from XAMPP and Redis installed by DevPanel. Each service
runs one selected version at a time. Projects can come from several folders at once (Settings → *Also serve*,
e.g. `C:\laragon\www` + `C:\xampp\htdocs`); if two folders have a project with the same name, the main one is used.

Installed somewhere else (e.g. `D:\xampp`, a portable Laragon)? Type the path or **Browse…** next to
*Other folder* and **Add**: the folder is scanned the same way and its name guessed from its control panel
exe. On first start DevPanel lists what it found and offers to open this list; the installer mentions them too.

Notes: PHP settings and extensions changed in DevPanel are written to that PHP's own `php.ini`, so the
other tool sees them too. Ports of linked databases are changed in the other tool. Don't run the other
tool's control panel and DevPanel on the same services at the same time.

The AI tools follow the same rule: `check_requirement` also looks at these environments and suggests
`link_external` before any install.

### First steps (standalone)
1. Open DevPanel → **Services → ⚙ Manage services**, tick and **Install** what you need (e.g. Apache, PostgreSQL, Redis).
2. **Services → PHP → Version**: install a PHP version (from windows.php.net).
3. Put a project in `www\myapp` → it is served at `http://myapp.test`. Click **Add to hosts** once (asks for admin).
4. **Settings → Trust certificate** for `https://*.test`.

---

## MCP for AI coding tools

```
claude mcp add devpanel -s user -- C:\path\to\devpanel.exe mcp
```

Then ask e.g. *"Set up this project so it runs"*. The assistant reads the project's requirements and must
call `check_requirement` for every runtime/service first:

- an installed version fits → it uses it (`set_site_php`, `set_version`, `enable_service`)
- nothing fits → it suggests a version and **asks you**; `install` is refused if something installed
  already fits, and DevPanel itself shows a **confirmation dialog** before installing anything

Destructive actions (dropping/restoring databases, deleting projects, uninstalling, passwords, ports,
the hosts file) are not available over MCP.

## CLI

```
devpanel status | groups | version
devpanel start <service|group|all>
devpanel stop <service|all>
devpanel sync
```

---

## How it works

- **Two processes**: a small background *agent* (tray, watchers, services; ~54 MB) and the *window*
  (WebView2). Closing the window ends its process and frees its memory; the agent keeps running.
  The window talks to the agent on `127.0.0.1` with a random per-start token.
- DevPanel only stops processes that run from its own install folder (and their children), never another
  Apache/MySQL on the same port.
- Configuration lives in `devpanel.json` next to the exe (keep it private: it may hold the MySQL root password).

## Build from source

Requirements: Windows 10/11, Go 1.26+. No Wails CLI or Node.js needed (the UI is plain HTML/JS embedded in the exe).

```powershell
go test -tags desktop,production .          # unit tests
$env:DEVPANEL_RUNTIME = "1"; go test -tags desktop,production -run Runtime .   # starts real Apache/Nginx/Caddy on random ports
.\build.ps1                                  # dist\: devpanel.exe, DevPanel-Setup-<ver>.exe, portable zip, SHA256SUMS.txt
```

The app icon comes from `winres/` (`rsrc_windows_*.syso`, generated with
[go-winres](https://github.com/tc-hib/go-winres): `go-winres make --in winres/winres.json`).

| File | What |
|---|---|
| `main.go`, `agent.go` | entry points, agent ⇄ window bridge |
| `app.go` | config, services start/stop, sites and vhosts |
| `services.go`, `categories.go`, `runtimes.go` | service selection, categories, languages |
| `webserver.go`, `modules.go`, `standalone.go` | Nginx, Caddy/FrankenPHP, proxy, standalone setup |
| `install.go`, `online.go`, `requirements.go` | catalog, downloads, version requirements |
| `mcp.go` | MCP server |
| `setup.go` | installer / uninstaller |
| `frontend/index.html` | the whole UI |

## License

MIT, see [LICENSE](LICENSE). Bundled Go modules: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

---

## Bahasa Indonesia

DevPanel adalah pengelola stack web lokal untuk Windows (mirip Laragon / Laravel Herd / FlyEnv).
Bisa dipakai **di atas folder Laragon yang sudah ada** atau **mandiri** di foldernya sendiri, lewat
**installer** atau **portable**.

- **Installer**: jalankan `DevPanel-Setup-<versi>.exe` dari halaman Releases. Tanpa hak admin; kalau Laragon
  ditemukan, Anda ditanya mau dipasang di sebelahnya atau mandiri.
- **Portable**: ekstrak zip ke folder mana saja lalu jalankan `devpanel.exe`.
- Uninstall lewat *Apps & features*. Project, database dan service yang sudah diinstal **tidak dihapus**.
- Bahasa tampilan bisa diganti ke Bahasa Indonesia di **Settings → Language**.
- Untuk AI (Claude Code): `claude mcp add devpanel -s user -- C:\path\devpanel.exe mcp`. Instalasi apa pun
  selalu dicek dulu dengan versi yang sudah ada dan wajib Anda setujui lewat dialog konfirmasi.
