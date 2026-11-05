package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// ---- Quick App (#2) ----

var folderRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// dbNameFor turns a folder name into a database name: my-shop -> my_shop.
func dbNameFor(folder string) string { return strings.ToLower(strings.ReplaceAll(folder, "-", "_")) }

// streamCmd runs a command, sending each output line to the UI as a "quickapp" event.
func (a *App) streamCmd(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = a.procEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { cmd.Wait(); pw.Close() }()
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		a.emitEvent("quickapp", sc.Text())
	}
	// The pipe closes only after Wait returned, so ProcessState is set here.
	if cmd.ProcessState != nil && !cmd.ProcessState.Success() {
		return fmt.Errorf("%s failed (exit %d)", filepath.Base(name), cmd.ProcessState.ExitCode())
	}
	return nil
}

// CreateApp makes a new project in www: kind "laravel" (composer create-project) or "blank".
// engine "pgsql" / "mysql" creates a database and points .env at it; "sqlite" keeps Laravel's default.
func (a *App) CreateApp(kind, folder, engine string) (string, error) {
	if !folderRe.MatchString(folder) {
		return "", errors.New("folder name: letters, digits, - and _ (max 63)")
	}
	dir := filepath.Join(a.www(), folder)
	if exists(dir) {
		return "", fmt.Errorf("www\\%s already exists", folder)
	}
	notes := []string{}
	if engine == "sqlite" && kind == "wordpress" {
		engine = "mysql" // WordPress needs MySQL/MariaDB
	}
	php := filepath.Join(a.svcDir("php"), "php.exe")
	switch kind {
	case "blank":
		if err := os.MkdirAll(filepath.Join(dir, "public"), 0755); err != nil {
			return "", err
		}
		os.WriteFile(filepath.Join(dir, `public\index.php`), []byte("<?php\n\necho 'Hello from "+folder+"';\nphpinfo();\n"), 0644)
	case "wordpress":
		if engine != "mysql" {
			return "", errors.New("WordPress needs a MySQL database")
		}
		if err := a.installWordPress(dir, dbNameFor(folder)); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	default:
		tpl, ok := composerTemplates[kind]
		if !ok {
			return "", fmt.Errorf("unknown app type %q", kind)
		}
		if !exists(php) {
			return "", errors.New("PHP is not installed")
		}
		if err := a.ensureComposer(); err != nil {
			return "", err
		}
		a.emitEvent("quickapp", "composer create-project "+tpl.Package+" "+folder+" …")
		if err := a.streamCmd(a.www(), php, a.path(`bin\composer\composer.phar`), "create-project", tpl.Package, folder, "--prefer-dist", "--no-interaction"); err != nil {
			return "", err
		}
		if tpl.Root != "" {
			a.mu.Lock()
			if a.cfg.SiteRoots == nil {
				a.cfg.SiteRoots = map[string]string{}
			}
			a.cfg.SiteRoots[folder] = tpl.Root
			a.saveConfig()
			a.mu.Unlock()
		}
	}

	a.mu.Lock()
	list, _ := a.sites()
	a.mu.Unlock()
	host := folder + "." + a.cfg.TLD
	if i := slices.IndexFunc(list, func(s Site) bool { return s.Folder == folder }); i >= 0 {
		host = list[i].Host
	}

	// Database for every template except blank; only Laravel's .env and WordPress' config are filled in.
	if kind != "blank" && (engine == "pgsql" || engine == "mysql") {
		svcName := "postgresql"
		if engine == "mysql" {
			svcName = "mysql"
		}
		db := dbNameFor(folder)
		if err := a.CreateDatabase(svcName, db); err != nil {
			notes = append(notes, "database not created ("+err.Error()+"), create it in Databases")
		} else {
			notes = append(notes, "database "+db+" created")
		}
		if kind != "laravel" && kind != "wordpress" {
			notes = append(notes, "enter database "+db+" in the project's config/installer")
		}
	}
	if kind == "laravel" {
		envFile := filepath.Join(dir, ".env")
		b, err := os.ReadFile(envFile)
		if err != nil {
			return "", fmt.Errorf("project created but .env missing: %w", err)
		}
		env := setEnv(string(b), "APP_URL", "http://"+host)
		if engine == "pgsql" || engine == "mysql" {
			svcName, user, pw := "postgresql", "postgres", ""
			if engine == "mysql" {
				svcName, user, pw = "mysql", "root", a.cfg.MySQLPassword
			}
			for _, kv := range [][2]string{{"DB_CONNECTION", engine}, {"DB_HOST", "127.0.0.1"}, {"DB_PORT", strconv.Itoa(port(svcName))}, {"DB_DATABASE", dbNameFor(folder)}, {"DB_USERNAME", user}, {"DB_PASSWORD", pw}} {
				env = setEnv(env, kv[0], kv[1])
			}
			notes = append(notes, "run php artisan migrate when ready")
		}
		if err := os.WriteFile(envFile, []byte(env), 0644); err != nil {
			return "", err
		}
	}
	if kind == "wordpress" {
		notes = append(notes, "open the site to finish the WordPress installer")
	}
	a.mu.Lock()
	_, _, err := a.syncVhostFiles()
	if err == nil {
		err = a.restartWeb()
	}
	a.mu.Unlock()
	if err != nil {
		notes = append(notes, "vhost sync: "+err.Error())
	}
	msg := "created www\\" + folder + " → http://" + host
	a.notify("Project created", folder+" → http://"+host)
	if len(notes) > 0 {
		msg += " (" + strings.Join(notes, "; ") + ")"
	}
	return msg, nil
}

// ---- php.ini quick settings (#9) ----

var iniKeys = []struct{ Key, Pattern, Hint string }{
	{"memory_limit", `^(-1|\d+[KMG]?)$`, "e.g. 512M, -1 = unlimited"},
	{"upload_max_filesize", `^\d+[KMG]?$`, "e.g. 64M"},
	{"post_max_size", `^\d+[KMG]?$`, "≥ upload_max_filesize"},
	{"max_execution_time", `^\d+$`, "seconds, 0 = unlimited"},
	{"max_input_time", `^-?\d+$`, "seconds, -1 = use max_execution_time"},
	{"max_input_vars", `^\d+$`, "e.g. 5000"},
	{"display_errors", `^(?i:On|Off)$`, "On / Off"},
	{"error_reporting", `^[A-Z_&~| ]+$`, "e.g. E_ALL"},
	{"date.timezone", `^[A-Za-z_/+-]+$`, "e.g. Asia/Jakarta"},
}

type IniSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

func iniRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*(;?)[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=[ \t]*([^\r\n]*?)[ \t]*(\r?)$`)
}

// iniGet returns the active (uncommented) value of key.
func iniGet(ini, key string) string {
	for _, m := range iniRe(key).FindAllStringSubmatch(ini, -1) {
		if m[1] == "" {
			return strings.Trim(m[2], `"`)
		}
	}
	return ""
}

// iniSet replaces the last active line (PHP uses the last one), else uncomments the first commented one, else appends.
func iniSet(ini, key, value string) string {
	re := iniRe(key)
	all := re.FindAllStringSubmatchIndex(ini, -1)
	pick := -1
	for i, m := range all {
		if ini[m[2]:m[3]] == "" {
			pick = i
		}
	}
	if pick < 0 && len(all) > 0 {
		pick = 0
	}
	if pick < 0 {
		if !strings.HasSuffix(ini, "\n") {
			ini += "\r\n"
		}
		return ini + key + " = " + value + "\r\n"
	}
	m := all[pick]
	return ini[:m[0]] + key + " = " + value + ini[m[6]:m[7]] + ini[m[1]:]
}

func (a *App) PHPSettings() ([]IniSetting, error) {
	b, err := os.ReadFile(filepath.Join(a.svcDir("php"), "php.ini"))
	if err != nil {
		return nil, fmt.Errorf("php.ini: %w", err)
	}
	out := []IniSetting{}
	for _, k := range iniKeys {
		out = append(out, IniSetting{k.Key, iniGet(string(b), k.Key), k.Hint})
	}
	return out, nil
}

// SetPHPSettings validates and writes the given values, then restarts Apache if running.
func (a *App) SetPHPSettings(vals map[string]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := filepath.Join(a.svcDir("php"), "php.ini")
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	ini := string(b)
	for _, k := range iniKeys {
		v, ok := vals[k.Key]
		if !ok || strings.TrimSpace(v) == iniGet(ini, k.Key) {
			continue
		}
		v = strings.TrimSpace(v)
		if !regexp.MustCompile(k.Pattern).MatchString(v) {
			return fmt.Errorf("%s: invalid value %q (%s)", k.Key, v, k.Hint)
		}
		if k.Key == "date.timezone" {
			if _, err := time.LoadLocation(v); err != nil {
				return fmt.Errorf("unknown timezone %q", v)
			}
		}
		ini = iniSet(ini, k.Key, v)
	}
	if err := os.WriteFile(f, []byte(ini), 0644); err != nil {
		return err
	}
	return a.restartWeb()
}

// ---- Uninstall a version (#22) ----

// UninstallVersion deletes bin\<tool>\<version>. Refused for the selected/active version or a running service.
// Data folders (data\postgresql..., data\mysql-...) are never deleted.
func (a *App) UninstallVersion(tool, version, confirm string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	known := false
	for _, t := range envTools {
		known = known || t.Name == tool
	}
	if !known {
		return fmt.Errorf("unknown tool %q", tool)
	}
	if !slices.Contains(a.versions(tool), version) || strings.ContainsAny(version, `\/`) || version == ".." {
		return fmt.Errorf("%s %q is not installed", tool, version)
	}
	if confirm != version {
		return errors.New("confirmation does not match; nothing deleted")
	}
	active := filepath.Base(a.svcDir(tool))
	if tool == "php" {
		active = a.CurrentPHP()
	}
	if version == active {
		return errors.New("this is the selected version; switch to another one first")
	}
	p := filepath.Join(a.path(`bin\`+tool), version)
	if isLink(p) { // linked from XAMPP/Herd/...: remove the junction only, never the other environment's files
		return os.Remove(p)
	}
	return os.RemoveAll(p)
}

// ---- Export / import config (#24) ----

func (a *App) ExportConfig() (string, error) {
	if a.ctx == nil {
		return "", errors.New("no window")
	}
	f, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title: "Export DevPanel settings", DefaultFilename: "devpanel-settings.json",
		Filters: []runtime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}},
	})
	if err != nil || f == "" {
		return "", err
	}
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return "exported to " + f, os.WriteFile(f, b, 0644)
}

// ImportConfig loads groups, naming rules, hostnames, site options, packages, sources and processes.
// Root, pinned versions and ports stay those of this machine (ports live in service config files). The current config is backed up first.
func (a *App) ImportConfig() (string, error) {
	if a.ctx == nil {
		return "", errors.New("no window")
	}
	f, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Import DevPanel settings", Filters: []runtime.FileFilter{{DisplayName: "JSON", Pattern: "*.json"}},
	})
	if err != nil || f == "" {
		return "", err
	}
	return a.importFrom(f)
}

func (a *App) importFrom(f string) (string, error) {
	b, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	var in Config
	if err := json.Unmarshal(b, &in); err != nil {
		return "", fmt.Errorf("not a DevPanel settings file: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, _ := json.MarshalIndent(a.cfg, "", "  ")
	backup := strings.TrimSuffix(a.cfgPath, ".json") + "-before-import-" + time.Now().Format("20060102-150405") + ".json"
	if err := os.WriteFile(backup, cur, 0644); err != nil {
		return "", fmt.Errorf("could not back up current settings: %w", err)
	}
	keep := a.cfg
	a.cfg.TLD = keep.TLD
	if in.Groups != nil {
		a.cfg.Groups = in.Groups
	}
	if in.HostRules != nil {
		a.cfg.HostRules = in.HostRules
	}
	for _, m := range []*map[string]string{&in.Hostnames, &in.SiteRoots} {
		if *m == nil {
			*m = map[string]string{}
		}
	}
	a.cfg.Hostnames, a.cfg.SiteRoots, a.cfg.SiteAliases = in.Hostnames, in.SiteRoots, in.SiteAliases
	a.cfg.Packages, a.cfg.Sources, a.cfg.Procs = in.Packages, in.Sources, in.Procs
	a.cfg.ManualSync, a.cfg.StartGroup = in.ManualSync, in.StartGroup
	if _, err := a.sites(); err != nil {
		a.cfg = keep
		return "", fmt.Errorf("imported names don't fit this machine's www: %w", err)
	}
	a.refreshTray()
	return "imported " + filepath.Base(f) + " (previous settings: " + filepath.Base(backup) + ")", a.saveConfig()
}
