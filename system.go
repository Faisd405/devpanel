package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows/registry"
)

// ---- Settings ----

type Settings struct {
	AutoSync   bool   `json:"autoSync"`
	Autostart  bool   `json:"autostart"`
	StartGroup string `json:"startGroup"` // group started when launched by Windows at login
	Watchdog   bool   `json:"watchdog"`
	Notify     bool   `json:"notify"`
}

func (a *App) Settings() Settings {
	return Settings{!a.cfg.ManualSync, autostartEnabled(), a.cfg.StartGroup, a.cfg.Watchdog, !a.cfg.NoNotify}
}

func (a *App) SaveSettings(s Settings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.StartGroup != "" {
		if _, ok := a.cfg.Groups[s.StartGroup]; !ok {
			return errors.New("unknown group " + s.StartGroup)
		}
	}
	if err := setAutostart(s.Autostart); err != nil {
		return err
	}
	a.cfg.ManualSync, a.cfg.StartGroup = !s.AutoSync, s.StartGroup
	a.cfg.Watchdog, a.cfg.NoNotify = s.Watchdog, !s.Notify
	return a.saveConfig()
}

// ---- Windows autostart (HKCU\...\Run, no admin) ----

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("DevPanel")
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue("DevPanel"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue("DevPanel", `"`+exe+`" --minimized`)
}

func launchedMinimized() bool { return slices.Contains(os.Args[1:], "--minimized") }

// ---- Auto sync: keep vhosts in step with www every few seconds ----

func (a *App) watchSites() {
	for range time.Tick(5 * time.Second) {
		if a.cfg.ManualSync || !a.mu.TryLock() {
			continue
		}
		_, changed, err := a.syncVhostFiles()
		if err == nil && changed > 0 {
			err = a.restartWeb()
		}
		a.mu.Unlock()
		if changed > 0 || err != nil {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			a.emitEvent("sites-synced", changed, msg)
		}
	}
}

func (a *App) emitEvent(name string, data ...interface{}) {
	if a.events != nil {
		a.events.add(name, data) // agent: delivered to the window process
	} else if a.ctx != nil {
		runtime.EventsEmit(a.ctx, name, data...)
	}
}

// missingHostCount counts site hostnames not yet in the Windows hosts file (adding them needs admin).
func missingHostCount(hosts []string) int {
	b, err := os.ReadFile(filepath.Join(os.Getenv("SystemRoot"), `System32\drivers\etc\hosts`))
	if err != nil {
		return 0
	}
	return len(missingHosts(string(b), hosts))
}

// ---- Tray ----

func (a *App) runTray() {
	go func() {
		goruntime.LockOSThread() // the tray's message loop must stay on one thread
		systray.Run(func() {
			systray.SetIcon(trayIcon())
			systray.SetTooltip("DevPanel")
			systray.SetOnTapped(a.showWindow)
			a.buildTrayMenu()
			a.trayOn = true
		}, nil)
	}()
}

func (a *App) showWindow() {
	if a.events != nil { // agent: the window is a separate process
		openUI()
		return
	}
	if a.ctx != nil {
		runtime.WindowShow(a.ctx)
		runtime.WindowUnminimise(a.ctx)
	}
}

func (a *App) refreshTray() {
	if a.trayOn {
		go a.buildTrayMenu()
	}
}

func (a *App) buildTrayMenu() {
	systray.ResetMenu()
	onClick := func(m *systray.MenuItem, f func()) {
		go func() {
			for range m.ClickedCh {
				f()
			}
		}()
	}
	onClick(systray.AddMenuItem("Open DevPanel", ""), a.showWindow)
	systray.AddSeparator()
	names := make([]string, 0, len(a.cfg.Groups))
	for n := range a.cfg.Groups {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		onClick(systray.AddMenuItem("Start "+n, strings.Join(a.cfg.Groups[n], " + ")), func() {
			a.trayResult("Start "+n, a.StartGroup(n))
		})
	}
	onClick(systray.AddMenuItem("Start all", "Every service ticked in Manage services"), func() {
		_, err := a.StartAll()
		a.trayResult("Start all", err)
	})
	onClick(systray.AddMenuItem("Stop all services", ""), func() { a.trayResult("Stop all", a.StopAll()) })
	systray.AddSeparator()
	onClick(systray.AddMenuItem("Quit DevPanel", "Services keep running"), a.quitAgent)
}

func (a *App) trayResult(label string, err error) {
	msg := label + ": ok"
	if err != nil {
		msg = label + ": " + err.Error()
	}
	systray.SetTooltip("DevPanel – " + msg)
	a.emitEvent("log", msg, err != nil)
}

// trayIcon draws a small blue ring and wraps the PNG in an ICO container (Windows tray wants ICO).
func trayIcon() []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	blue := color.RGBA{0x4f, 0x8c, 0xff, 0xff}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			d := dx*dx + dy*dy
			if d <= 15*15 && (d >= 9*9 || d <= 4*4) {
				img.Set(x, y, blue)
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)
	ico := []byte{0, 0, 1, 0, 1, 0, n, n, 0, 0, 1, 0, 32, 0}
	size, off := p.Len(), 22
	ico = append(ico, byte(size), byte(size>>8), byte(size>>16), byte(size>>24), byte(off), 0, 0, 0)
	return append(ico, p.Bytes()...)
}
