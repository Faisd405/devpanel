package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend
var assets embed.FS

// devpanel.exe               -> make sure the agent runs, then open the window
// devpanel.exe --ui          -> window only (started by the tray)
// devpanel.exe --agent       -> background agent (tray, watchers, services)
// devpanel.exe --minimized   -> agent started by Windows at login (also runs the start group)
// devpanel.exe mcp           -> MCP server on stdio for AI coding tools
// DevPanel-Setup.exe / --install, --uninstall -> installer and uninstaller (see setup.go)
// devpanel.exe <command>     -> CLI (status, start, stop, groups, sync)
func main() {
	// The installer must not create devpanel.json next to itself (e.g. in Downloads): handle it first.
	if isSetupExe() || (len(os.Args) > 1 && os.Args[1] == "--install") {
		os.Exit(runSetup())
	}
	app := NewApp()
	arg := ""
	if len(os.Args) > 1 {
		arg = os.Args[1]
	}
	switch arg {
	case "--agent", "--minimized":
		os.Exit(app.runAgent())
	case "mcp":
		os.Exit(runMCP(app))
	case "--uninstall":
		os.Exit(runUninstall(app))
	case "", "--ui":
		runUI(app)
	default:
		os.Exit(app.cli(os.Args[1:]))
	}
}

func runUI(app *App) {
	p := &Proxy{cfgDir: filepath.Dir(app.cfgPath)}
	if err := p.ensureAgent(); err != nil {
		println(err.Error())
		os.Exit(1)
	}
	err := wails.Run(&options.App{
		Title:       "DevPanel",
		Width:       1000,
		Height:      740,
		AssetServer: &assetserver.Options{Assets: assets},
		OnStartup:   p.startup,
		// Closing the window ends this process and frees WebView2's memory; the agent stays in the tray.
		// A second launch of the same install just brings the open window to front.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               instanceID(),
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { p.show() },
		},
		// GPU rendering on for smoothness; "webviewNoGpu": true in devpanel.json saves ~64 MB instead.
		// Own WebView2 data folder per install: WebView2 refuses to share one folder between
		// processes started with different options (error 8007139f), e.g. an old build still running.
		Windows: &windows.Options{
			WebviewGpuIsDisabled: app.cfg.WebviewNoGPU,
			WebviewUserDataPath:  filepath.Join(os.Getenv("LOCALAPPDATA"), "DevPanel", instanceID()),
		},
		Bind: []interface{}{p},
	})
	if err != nil {
		println(err.Error())
	}
}

// instanceID is unique per install folder, so separate copies of DevPanel don't hijack each other's window.
func instanceID() string {
	exe, _ := os.Executable()
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Dir(exe))))
	return "devpanel-" + hex.EncodeToString(sum[:8])
}
