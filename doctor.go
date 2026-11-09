package main

// Checks: everything that keeps a site from working, on one page, each with a fix where DevPanel can do it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Check struct {
	Level  string   `json:"level"` // error, warn, info
	Title  string   `json:"title"`
	Detail string   `json:"detail"`
	Fix    string   `json:"fix"`    // button label, "" = nothing to click
	Action string   `json:"action"` // for FixCheck
	Args   []string `json:"args"`
}

func (a *App) Checks() []Check {
	out := []Check{}
	add := func(c Check) { out = append(out, c) }

	list := a.Sites()
	if list.Error != "" {
		add(Check{Level: "error", Title: "Hostname problems", Detail: list.Error + ". Fix them in Sites (hostname or naming rules)."})
	}
	if list.Missing > 0 {
		add(Check{Level: "warn", Title: fmt.Sprintf("%d hostname(s) not in the Windows hosts file", list.Missing),
			Detail: "Those sites don't open in the browser yet.", Fix: "Add to hosts (admin)", Action: "hosts"})
	}
	if !a.SSL().Trusted {
		add(Check{Level: "info", Title: "HTTPS certificate is not trusted", Detail: "https://*.test shows a browser warning until the DevPanel CA is trusted.", Fix: "Trust certificate", Action: "trust"})
	}
	for _, s := range a.Status() {
		if !s.Enabled {
			continue
		}
		if s.Owner != "" {
			add(Check{Level: "warn", Title: fmt.Sprintf("Port %d of %s is used by %s", s.Port, serviceLabel(s.Name), s.Owner),
				Detail: "DevPanel won't start a second copy. Stop it there, or change the port in Settings → Ports."})
		}
		if s.Version == "" {
			add(Check{Level: "info", Title: serviceLabel(s.Name) + " is ticked but not installed", Detail: "Install it in Services → Manage services, or untick it."})
		}
	}
	for _, s := range list.Sites {
		if s.AppURL != "" && !s.URLOK && strings.HasSuffix(urlHost(s.AppURL), "."+a.cfg.TLD) {
			add(Check{Level: "warn", Title: s.Folder + ": APP_URL is " + s.AppURL, Detail: "The site is served as " + s.Host + ".",
				Fix: "Fix APP_URL", Action: "appurl", Args: []string{s.Folder}})
		}
		out = append(out, a.extChecks(s)...)
	}
	for _, svcDir := range globDirs(a.path("bin")) {
		for _, f := range globDirs(svcDir) {
			if _, err := os.Stat(f); err != nil && isLink(f) {
				add(Check{Level: "error", Title: "Broken link " + filepath.Base(svcDir) + `\` + filepath.Base(f),
					Detail: "Its folder in the other environment is gone.", Fix: "Remove link", Action: "unlink", Args: []string{filepath.Base(svcDir), filepath.Base(f)}})
			}
		}
	}
	return out
}

// extChecks: PHP extensions a project requires (composer.json ext-*) that its PHP doesn't load.
func (a *App) extChecks(s Site) []Check {
	dir, _ := a.siteDir(s.Folder)
	b, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		return nil
	}
	var cj struct {
		Require map[string]string `json:"require"`
	}
	if json.Unmarshal(b, &cj) != nil {
		return nil
	}
	php := a.svcDir("php")
	if s.PHP != "" {
		php = a.phpDir(s.PHP)
	}
	mods := phpModules(php)
	if mods == nil {
		return nil
	}
	var out []Check
	for req := range cj.Require {
		ext, ok := strings.CutPrefix(strings.ToLower(req), "ext-")
		if !ok || mods[ext] || (ext == "zend-opcache" && mods["zend opcache"]) {
			continue
		}
		c := Check{Level: "warn", Title: fmt.Sprintf("%s needs PHP extension %s", s.Folder, ext), Detail: "Not loaded by " + filepath.Base(php) + "."}
		if exists(filepath.Join(php, `ext\php_`+ext+`.dll`)) {
			c.Fix, c.Action, c.Args = "Enable "+ext, "ext", []string{filepath.Base(php), ext}
		} else {
			c.Detail += " It isn't shipped with this PHP build (install it or use another PHP version)."
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y Check) int { return strings.Compare(x.Title, y.Title) })
	return out
}

var phpModCache sync.Map // dir -> phpMods

type phpMods struct {
	ini  time.Time
	mods map[string]bool
}

// phpModules: lower-case names from php -m (built-in and enabled), cached until php.ini changes.
func phpModules(dir string) map[string]bool {
	st, err := os.Stat(filepath.Join(dir, "php.ini"))
	var mt time.Time
	if err == nil {
		mt = st.ModTime()
	}
	if c, ok := phpModCache.Load(dir); ok && c.(phpMods).ini.Equal(mt) {
		return c.(phpMods).mods
	}
	out, err := output(filepath.Join(dir, "php.exe"), "-m")
	if err != nil || out == "" {
		return nil
	}
	mods := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" && !strings.HasPrefix(l, "[") {
			mods[l] = true
		}
	}
	phpModCache.Store(dir, phpMods{mt, mods})
	return mods
}

// FixCheck runs the fix of a check; only the listed actions, with their arguments validated by the callee.
func (a *App) FixCheck(action string, args []string) (string, error) {
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	switch action {
	case "hosts":
		return a.SyncVhosts()
	case "trust":
		return a.TrustCA()
	case "appurl":
		return a.FixAppURL(arg(0))
	case "unlink":
		return "link removed", a.UnlinkExternal(arg(0), arg(1))
	case "ext":
		v := arg(0)
		if !slices.Contains(a.PHPVersions(), v) {
			return "", fmt.Errorf("no PHP %q", v)
		}
		return arg(1) + " enabled in " + v, a.setPHPExtIn(a.phpDir(v), arg(1), true)
	}
	return "", fmt.Errorf("unknown fix %q", action)
}
