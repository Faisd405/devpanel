# Third-party notices

DevPanel is MIT licensed (see `LICENSE`). The `devpanel.exe` binary includes the Go modules below,
each under its own license. Full license texts are in each module's source
(e.g. `go mod download -json <module>` then see its `LICENSE` file).

| Module | Version | License |
|---|---|---|
| github.com/wailsapp/wails/v2 | v2.17.1 | MIT |
| github.com/wailsapp/go-webview2 | v1.0.22 | MIT |
| fyne.io/systray | v1.12.2 | Apache-2.0 |
| github.com/tkrajina/go-reflector | v0.5.8 | Apache-2.0 |
| git.sr.ht/~jackmordaunt/go-toast/v2 | v2.0.3 | MIT |
| github.com/bep/debounce | v1.2.1 | MIT |
| github.com/gabriel-vasile/mimetype | v1.4.16 | MIT |
| github.com/go-ole/go-ole | v1.3.0 | MIT |
| github.com/leaanthony/go-ansi-parser | v1.6.1 | MIT |
| github.com/leaanthony/slicer | v1.6.0 | MIT |
| github.com/leaanthony/u | v1.1.1 | MIT |
| github.com/rivo/uniseg | v0.4.7 | MIT |
| github.com/samber/lo | v1.49.1 | MIT |
| github.com/godbus/dbus/v5 | v5.1.0 | BSD-2-Clause |
| github.com/pkg/browser | v0.0.0-20240102092130 | BSD-2-Clause |
| github.com/pkg/errors | v0.9.1 | BSD-2-Clause |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| golang.org/x/net | v0.60.0 | BSD-3-Clause |
| golang.org/x/sys | v0.48.0 | BSD-3-Clause |
| golang.org/x/text | v0.42.0 | BSD-3-Clause |
| Go standard library | 1.26 | BSD-3-Clause |

The window uses the Microsoft Edge **WebView2 Runtime** that ships with Windows 10/11; it is not bundled.

## Software DevPanel downloads

DevPanel does not bundle or redistribute web servers, databases or runtimes. When you click Install, it
downloads them from their official publishers (php.net, Apache Lounge, nginx.org, EnterpriseDB, MySQL,
nodejs.org, GitHub releases of the respective projects, ...). Each of them stays under its own license
(PHP License, Apache-2.0, BSD, GPL, SSPL, Elastic License, ...), which applies to how you use it.
The database GUI, [Adminer](https://www.adminer.org/) by Jakub Vrána (Apache-2.0 or GPL-2.0), is downloaded
from its GitHub releases the first time you open it; DevPanel adds only a small `index.php` that loads it.

## Not affiliated

DevPanel is an independent project. It is not affiliated with or endorsed by Laragon, Laravel Herd /
Laravel, or FlyEnv. It contains no code from them; it can use the folder layout of an existing Laragon
installation, and it reads Laragon's own `usr\packages.conf` catalog when present. "Laragon", "Laravel",
"Herd" and "FlyEnv" are names of their respective owners.
