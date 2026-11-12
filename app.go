package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Services in start order. Port is used both for status and for waiting on start/stop.
var services = []struct {
	Name  string
	Port  int
	Image string // process name to force-kill if graceful stop fails
}{
	{"apache", 80, "httpd.exe"},
	{"nginx", 80, "nginx.exe"},
	{"caddy", 80, "caddy.exe"},
	{"frankenphp", 80, "frankenphp.exe"},
	{"postgresql", 5432, "postgres.exe"},
	{"mysql", 3306, "mysqld.exe"},
	{"redis", 6379, "redis-server.exe"},
	{"mailpit", 8025, "mailpit.exe"},
	{"memcached", 11211, "memcached.exe"},
	{"mongodb", 27017, "mongod.exe"},
	{"meilisearch", 7700, "meilisearch.exe"},
	{"elasticsearch", 9200, "java.exe"},
	{"rabbitmq", 5672, "erl.exe"},
	{"ollama", 11434, "ollama.exe"},
}

type Config struct {
	Root   string              `json:"root"`
	TLD    string              `json:"tld"`
	Groups map[string][]string `json:"groups"`
	// Custom ports (service name or apache_ssl / mailpit_smtp); missing = default.
	Ports map[string]int `json:"ports"`
	// Per-site options keyed by www folder.
	SiteRoots   map[string]string   `json:"siteRoots"`   // document root relative to the folder ("." = folder itself)
	SiteAliases map[string][]string `json:"siteAliases"` // extra hostnames
	SitePHP     map[string]string   `json:"sitePhp"`     // PHP version per site ("" = default)
	SiteProxy   map[string]string   `json:"siteProxy"`   // reverse proxy target per site, e.g. http://127.0.0.1:3000
	// Services ticked in "Manage services" (null = every installed one, like before).
	Services []string `json:"services"`
	WWW      string   `json:"www,omitempty"` // projects folder, "" = <root>\www
	// Folders the user added to the environment scan (other installs outside the known places).
	ExternalPaths []string `json:"externalPaths,omitempty"`
	// More projects folders served next to WWW (e.g. XAMPP's htdocs, ~/Herd).
	ExtraWWW []string `json:"extraWww,omitempty"`
	// AutoLink: environments whose new versions are linked automatically; ShareLaragon: own versions shown in Laragon.
	AutoLink     []string `json:"autoLink,omitempty"`
	ShareLaragon bool     `json:"shareLaragon,omitempty"`
	// WebServer: the one picked last (apache, nginx, caddy, frankenphp); Start All starts only this one.
	WebServer string `json:"webServer,omitempty"`
	// WizardDone: the first-run setup wizard was finished or skipped.
	WizardDone bool `json:"wizardDone,omitempty"`
	// Backups: scheduled database dumps.
	Backups []BackupJob `json:"backups,omitempty"`
	// ExternalHinted: the first-run "other environments found" hint was shown.
	ExternalHinted bool   `json:"externalHinted,omitempty"`
	Terminal       string `json:"terminal,omitempty"` // "cmd" (default) or "cmder"
	// Pinned folder per service under bin\<service>, so installing a newer version doesn't switch silently.
	Versions map[string]string `json:"versions"`
	// Rewrite rules for default hostnames, e.g. prefix "app_" -> "" makes app_webshop -> webshop.test.
	HostRules []HostRule `json:"hostRules"`
	// Legacy (v1): migrated into HostRules on load.
	StripPrefixes []string `json:"stripPrefixes,omitempty"`
	// Custom hostname per www folder, overriding the default.
	Hostnames map[string]string `json:"hostnames"`
	// Install catalog additions: single packages and extra packages.conf-style sources.
	Packages []CustomPkg `json:"packages"`
	Sources  []string    `json:"sources"`
	// ManualSync turns off the www watcher (zero value = auto sync on).
	ManualSync bool   `json:"manualSync"`
	StartGroup string `json:"startGroup"`
	Procs      []Proc `json:"procs"`
	// Watchdog restarts services that die on their own; NoNotify turns Windows toasts off.
	Watchdog bool `json:"watchdog"`
	NoNotify bool `json:"noNotify"`
	// WebviewNoGPU turns WebView2 GPU acceleration off (~64 MB less while the window is open).
	WebviewNoGPU bool `json:"webviewNoGpu,omitempty"`
	// MySQLPassword is the root password set from DevPanel (local machine only; passed via MYSQL_PWD).
	MySQLPassword string `json:"mysqlPassword,omitempty"`
}

type App struct {
	mu      sync.Mutex
	ctx     context.Context
	cfg     Config
	cfgPath string

	srcMu    sync.Mutex
	srcCache map[string]string

	procMu sync.Mutex
	procs  map[string]*procRun
	trayOn bool

	shareMu  sync.Mutex
	share    Share
	shareCmd *exec.Cmd

	updates []Package       // from CheckUpdates, guarded by srcMu
	wanted  map[string]bool // services started via DevPanel, for the watchdog (guarded by mu)

	events   *eventLog // agent mode: events for the window process
	quitCh   chan struct{}
	quitOnce sync.Once
}

type Status struct {
	Name     string   `json:"name"`
	Port     int      `json:"port"`
	Running  bool     `json:"running"`
	Version  string   `json:"version"`
	Versions []string `json:"versions"`
	Enabled  bool     `json:"enabled"` // ticked in Manage services
	Category string   `json:"category"`
	Owner    string   `json:"owner"` // port held by another program / environment, e.g. "Laravel Herd (nginx.exe)"
}

func NewApp() *App {
	exe, _ := os.Executable()
	a := &App{cfgPath: filepath.Join(filepath.Dir(exe), "devpanel.json")}
	a.cfg = Config{Root: detectRoot(filepath.Dir(exe)), TLD: "test", Groups: map[string][]string{
		"PG + Apache":         {"apache", "postgresql"},
		"PG + Redis + Apache": {"apache", "postgresql", "redis"},
	}}
	if b, err := os.ReadFile(a.cfgPath); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if strings.TrimSpace(a.cfg.Root) == "" {
		a.cfg.Root = detectRoot(filepath.Dir(exe))
	}
	if a.cfg.TLD == "" {
		a.cfg.TLD = "test"
	}
	if a.cfg.Versions == nil {
		a.cfg.Versions = map[string]string{}
	}
	setPortOverrides(a.cfg.Ports)
	setMySQLPassword(a.cfg.MySQLPassword)
	if a.cfg.HostRules == nil {
		a.cfg.HostRules = []HostRule{}
		p := a.cfg.StripPrefixes
		if p == nil {
			p = []string{"app_"}
		}
		for _, x := range p {
			a.cfg.HostRules = append(a.cfg.HostRules, HostRule{"prefix", x, ""})
		}
	}
	a.cfg.StripPrefixes = nil
	if a.cfg.Versions["php"] == "" {
		a.cfg.Versions["php"] = a.CurrentPHP()
	}
	if a.cfg.Hostnames == nil {
		a.cfg.Hostnames = map[string]string{}
	}
	for _, s := range services {
		if v := a.cfg.Versions[s.Name]; v == "" || v == "." {
			a.cfg.Versions[s.Name] = folderName(a.svcDir(s.Name)) // "" while not installed
		}
	}
	if before, _ := os.ReadFile(a.cfgPath); string(before) != string(a.configJSON()) {
		a.saveConfig()
	}
	return a
}

func (a *App) configJSON() []byte {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	return b
}

func (a *App) saveConfig() error {
	return os.WriteFile(a.cfgPath, a.configJSON(), 0644)
}

func (a *App) path(rel string) string { return filepath.Join(a.cfg.Root, rel) }

// versions lists installed folders under bin\<service>, sorted.
func (a *App) versions(name string) []string {
	entries, _ := os.ReadDir(a.path(`bin\` + name))
	out := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".partial") {
			continue
		}
		// Junctions/symlinks to a folder count too (os.Stat follows them).
		if st, err := os.Stat(filepath.Join(a.path(`bin\`+name), e.Name())); err == nil && st.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// svcDir is the pinned version folder, or the highest installed one.
func (a *App) svcDir(name string) string {
	vs := a.versions(name)
	if v := a.cfg.Versions[name]; slices.Contains(vs, v) {
		return a.path(`bin\` + name + `\` + v)
	}
	if len(vs) == 0 {
		return ""
	}
	return a.path(`bin\` + name + `\` + vs[len(vs)-1])
}

// Laragon's original PostgreSQL lives in bin\postgresql\postgresql with data\postgresql;
// other versions get their own data dir since data dirs are tied to a major version.
func (a *App) pgDataFor(folder string) string {
	if folder == "postgresql" {
		return a.path(`data\postgresql`)
	}
	return a.path(`data\` + folder)
}

func (a *App) pgData() string {
	if d := a.svcDir("postgresql"); d != "" && isLink(d) {
		if data := linkedPgData(realPath(d)); data != "" { // keep using the other environment's databases
			return data
		}
	}
	return a.pgDataFor(filepath.Base(a.svcDir("postgresql")))
}

// linkedPgData finds the existing cluster of a PostgreSQL linked from another environment:
// next to the binaries (…\data) or Laragon-style (<root>\bin\postgresql\<v> -> <root>\data\postgresql or \<v>).
func linkedPgData(real string) string {
	folder := filepath.Base(real)
	root := filepath.Dir(filepath.Dir(filepath.Dir(real))) // <root>\bin\postgresql\<folder> -> <root>
	for _, c := range []string{filepath.Join(real, "data"), filepath.Join(root, "data", folder), filepath.Join(root, `data\postgresql`)} {
		if exists(filepath.Join(c, "PG_VERSION")) {
			return c
		}
	}
	return ""
}

func (a *App) SetVersion(name, v string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Contains(a.versions(name), v) {
		return fmt.Errorf("%s %q not installed", name, v)
	}
	if s := svc(name); s.Name != "" && a.running(name) {
		return fmt.Errorf("stop %s before switching version", name)
	}
	if name == "php" {
		if err := a.setPHP(v); err != nil {
			return err
		}
	}
	a.cfg.Versions[name] = v
	if err := a.saveConfig(); err != nil {
		return err
	}
	return a.writeEnvCmd()
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.Dir = filepath.Dir(cmd.Path)
	return cmd
}

// spawn starts a long-running process and leaves it running after the panel exits.
func spawn(exe string, args ...string) error {
	cmd := hidden(exec.Command(exe, args...))
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// run executes a short command. Output is not captured on purpose: pg_ctl's child
// inherits the pipes and would block us forever.
func run(exe string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return withMySQLEnv(hidden(exec.CommandContext(ctx, exe, args...))).Run()
}

func portOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		c.Close()
	}
	return err == nil
}

func waitPort(port int, open bool, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(300 * time.Millisecond) {
		if portOpen(port) == open {
			return true
		}
	}
	return false
}

// running: the service's port is open AND a process from this install's folder is alive.
// Port alone is not enough: Apache and Nginx share :80, and other installs may use the same ports.
func (a *App) running(name string) bool {
	return portOpen(port(name)) && a.ownedIn(processPaths(), name)
}

func (a *App) Status() []Status {
	out := []Status{}
	paths := processPaths()
	var pids map[int]uint32
	for _, s := range services {
		p := port(s.Name)
		d := a.svcDir(s.Name)
		st := Status{s.Name, p, false, folderName(d), a.versions(s.Name), a.enabled(s.Name), categoryOf(s.Name), ""}
		if portOpen(p) {
			if st.Running = a.ownedIn(paths, s.Name); !st.Running && !a.ownPortHolder(paths, s.Name, p) {
				if pids == nil {
					pids = listenPIDs()
				}
				st.Owner = a.ownerOf(exePath(pids[p]))
			}
		}
		out = append(out, st)
	}
	return out
}

func (a *App) start(name string) error {
	d := a.svcDir(name)
	if d == "" {
		return fmt.Errorf("%s is not installed", name)
	}
	if portOpen(port(name)) {
		return fmt.Errorf("%s: port %d is already in use by %s (stop it there first, or change the port)", name, port(name), a.portOwner(port(name)))
	}
	var err error
	switch name {
	case "nginx":
		err = a.startNginx()
	case "apache":
		if a.isAdopted("apache") { // linked from XAMPP/Herd/...: run with DevPanel's own config, theirs untouched
			if err = a.ensureApacheSupport(); err == nil { // SSL, default vhost, PHP handler: DevPanel's own files
				err = a.writeAdoptedApacheConf(d)
			}
			if err == nil {
				err = spawn(filepath.Join(d, `bin\httpd.exe`), a.apacheArgs()...)
			}
		} else if err = a.ensureApacheConfig(d); err == nil { // standalone: own httpd.conf, fcgid, SSL files
			a.ensureApacheInclude()
			err = spawn(filepath.Join(d, `bin\httpd.exe`))
		}
	case "postgresql":
		if !exists(filepath.Join(a.pgData(), "PG_VERSION")) { // e.g. a linked PostgreSQL without a data folder: new empty cluster
			if err = run(filepath.Join(d, `bin\initdb.exe`), "-D", a.pgData(), "-U", "postgres", "-A", "trust", "-E", "UTF8"); err != nil {
				return fmt.Errorf("postgresql: initdb: %w", err)
			}
		}
		err = run(filepath.Join(d, `bin\pg_ctl.exe`), "start", "-D", a.pgData(), "-l", filepath.Join(a.pgData(), "devpanel.log"))
	case "mysql":
		err = spawn(filepath.Join(d, `bin\mysqld.exe`), "--defaults-file="+findMyIni(d))
	case "redis":
		if conf := filepath.Join(d, "redis.windows.conf"); exists(conf) {
			err = spawn(filepath.Join(d, "redis-server.exe"), conf)
		} else { // some builds ship without a conf: defaults + our port
			err = spawn(filepath.Join(d, "redis-server.exe"), "--port", strconv.Itoa(port("redis")), "--bind", "127.0.0.1")
		}
	case "memcached":
		exe := findExe(d, "memcached.exe")
		if exe == "" {
			return errors.New("memcached.exe not found in " + d)
		}
		err = spawn(exe, "-p", strconv.Itoa(port("memcached")), "-l", "127.0.0.1", "-m", "64")
	case "mongodb":
		data := a.mongoData()
		if err = os.MkdirAll(data, 0755); err == nil {
			// No graceful stop on Windows besides a service/shell; WiredTiger's journal covers a kill.
			err = spawn(filepath.Join(d, `bin\mongod.exe`), "--dbpath", data, "--port", strconv.Itoa(port("mongodb")),
				"--bind_ip", "127.0.0.1", "--logpath", filepath.Join(data, "mongod.log"), "--logappend")
		}
	case "mailpit":
		err = spawn(filepath.Join(d, "mailpit.exe"), "--listen", fmt.Sprintf("127.0.0.1:%d", port("mailpit")), "--smtp", fmt.Sprintf("127.0.0.1:%d", port("mailpit_smtp")))
	default:
		var ok bool
		if ok, err = a.startExtra(name); !ok {
			return fmt.Errorf("unknown service %q", name)
		}
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if !waitPort(svc(name).Port, true, startTimeout(name)) {
		return fmt.Errorf("%s: started but port %d not open after %s", name, svc(name).Port, startTimeout(name))
	}
	if a.wanted == nil {
		a.wanted = map[string]bool{}
	}
	a.wanted[name] = true
	return nil
}

func (a *App) stop(name string) error {
	s := svc(name)
	d := a.svcDir(name)
	delete(a.wanted, name) // stopped on purpose: the watchdog must not bring it back
	// Never touch a server that runs from somewhere else (another Laragon/XAMPP/Docker on the same port).
	if portOpen(s.Port) && !a.ownedIn(processPaths(), name) {
		return fmt.Errorf("%s: port %d is used by a program outside %s; not stopping it", name, s.Port, d)
	}
	graceful := true
	switch name {
	case "nginx":
		a.stopNginx()
	case "postgresql":
		run(filepath.Join(d, `bin\pg_ctl.exe`), "stop", "-D", a.pgData(), "-m", "fast")
	case "mysql":
		run(filepath.Join(d, `bin\mysqladmin.exe`), "-uroot", "-P"+strconv.Itoa(s.Port), "-h127.0.0.1", "shutdown") // fails if root has a password; force-kill below
	case "redis":
		run(filepath.Join(d, "redis-cli.exe"), "-p", strconv.Itoa(port("redis")), "shutdown")
	default:
		a.stopExtra(name)
		graceful = false
	}
	if graceful && waitPort(s.Port, false, 10*time.Second) {
		return nil
	}
	a.killOwned(name) // only processes started from this install's folders
	if !waitPort(s.Port, false, 5*time.Second) {
		return fmt.Errorf("%s: port %d still in use (another program?)", name, s.Port)
	}
	return nil
}

func svc(name string) (s struct {
	Name  string
	Port  int
	Image string
}) {
	for _, x := range services {
		if x.Name == name {
			x.Port = port(name)
			return x
		}
	}
	return
}

func (a *App) Start(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running(name) {
		return nil
	}
	return a.start(name)
}

func (a *App) Stop(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stop(name)
}

// ---- Groups ----

func (a *App) Groups() map[string][]string { return a.cfg.Groups }

func (a *App) SaveGroup(name string, svcs []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(svcs) == 0 {
		return errors.New("group needs a name and at least one service")
	}
	for _, s := range svcs {
		if svc(s).Name == "" {
			return fmt.Errorf("unknown service %q", s)
		}
	}
	a.cfg.Groups[name] = svcs
	a.refreshTray()
	return a.saveConfig()
}

func (a *App) DeleteGroup(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cfg.Groups, name)
	a.refreshTray()
	return a.saveConfig()
}

// StartGroup starts the group's services and stops everything else.
func (a *App) StartGroup(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.cfg.Groups[name]
	if !ok {
		return fmt.Errorf("no group %q", name)
	}
	var errs []error
	for _, s := range services {
		want, running := slices.Contains(g, s.Name), a.running(s.Name)
		if want && !running {
			errs = append(errs, a.start(s.Name))
		} else if !want && running {
			errs = append(errs, a.stop(s.Name))
		}
	}
	return errors.Join(errs...)
}

// ---- PHP ----

var phpRe = regexp.MustCompile(`bin/php/[^/";]+`)

func (a *App) PHPVersions() []string {
	m, _ := filepath.Glob(a.path(`bin\php\*\php-cgi.exe`))
	out := []string{}
	for _, p := range m {
		out = append(out, filepath.Base(filepath.Dir(p)))
	}
	return out
}

// CurrentPHP is Apache's PHP (fcgid.conf); without one yet (standalone, no Apache), the pinned or newest PHP.
func (a *App) CurrentPHP() string {
	b, _ := os.ReadFile(a.path(`etc\apache2\fcgid.conf`))
	if v := strings.TrimPrefix(phpRe.FindString(string(b)), "bin/php/"); v != "" {
		return v
	}
	vs := a.PHPVersions()
	if slices.Contains(vs, a.cfg.Versions["php"]) {
		return a.cfg.Versions["php"]
	}
	if len(vs) == 0 {
		return ""
	}
	return vs[len(vs)-1]
}

// setPHP points Apache's fcgid at another PHP folder and restarts Apache if running.
func (a *App) setPHP(v string) error {
	if !slices.Contains(a.PHPVersions(), v) {
		return fmt.Errorf("unknown PHP version %q", v)
	}
	f := a.path(`etc\apache2\fcgid.conf`)
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f, []byte(phpRe.ReplaceAllString(string(b), "bin/php/"+v)), 0644); err != nil {
		return err
	}
	return a.restartWeb()
}

func (a *App) restartApache() error {
	if !portOpen(port("apache")) {
		return nil
	}
	// Port taken but not by this install's Apache (Nginx, another Laragon/XAMPP): leave it alone.
	if !a.running("apache") {
		if a.running("nginx") {
			return nil
		}
		return fmt.Errorf("apache: port %d belongs to another program; restart it yourself to apply changes", port("apache"))
	}
	// Check the new config first: a broken vhost/template must not take the running Apache down.
	if out, err := output(filepath.Join(a.svcDir("apache"), `bin\httpd.exe`), append(a.apacheArgs(), "-t")...); err != nil || !strings.Contains(out, "Syntax OK") {
		return fmt.Errorf("apache config error, kept the running Apache unchanged: %s", out)
	}
	if err := a.stop("apache"); err != nil {
		return err
	}
	return a.start("apache")
}

// ---- Virtual hosts ----

type Site struct {
	Folder  string   `json:"folder"`
	Host    string   `json:"host"`
	Default string   `json:"default"` // hostname without a custom override
	Root    string   `json:"root"`
	RootRel string   `json:"rootRel"` // custom document root relative to the folder, "" = auto (public/ if present)
	Source  string   `json:"source"`  // projects folder it comes from when not the main www, e.g. "htdocs", "Herd"
	Aliases []string `json:"aliases"`
	PHP     string   `json:"php"`   // pinned PHP version, "" = default
	Proxy   string   `json:"proxy"` // reverse proxy target, "" = serve files/PHP
	// Filled for the UI only (Sites()).
	Framework string `json:"framework"`
	AppURL    string `json:"appUrl"`
	URLOK     bool   `json:"urlOk"` // APP_URL points at this site's hostname
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_.-]*[a-z0-9])?$`)

// normHost lowercases a hostname and appends the TLD when it has no dot ("customs" -> "customs.test").
func (a *App) normHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h != "" && !strings.Contains(h, ".") {
		h += "." + a.cfg.TLD
	}
	return h
}

type HostRule struct {
	Kind string `json:"kind"` // "prefix" or "suffix"
	From string `json:"from"`
	To   string `json:"to"`
}

// defaultHost applies the first matching prefix rule and the first matching suffix rule, then adds the TLD.
func (a *App) defaultHost(folder string) string {
	n := strings.ToLower(folder)
	pre, suf := false, false
	for _, r := range a.cfg.HostRules {
		f := strings.ToLower(r.From)
		if f == "" || len(n) <= len(f) {
			continue
		}
		if r.Kind == "prefix" && !pre && strings.HasPrefix(n, f) {
			n, pre = strings.ToLower(r.To)+n[len(f):], true
		} else if r.Kind == "suffix" && !suf && strings.HasSuffix(n, f) {
			n, suf = n[:len(n)-len(f)]+strings.ToLower(r.To), true
		}
	}
	return n + "." + a.cfg.TLD
}

type SiteList struct {
	Sites   []Site `json:"sites"`
	Error   string `json:"error"`
	Missing int    `json:"missing"` // hostnames not yet in the Windows hosts file
}

// Sites is the UI view: the list is returned even when hostnames collide, so they can be fixed.
func (a *App) Sites() SiteList {
	list, err := a.sites()
	r := SiteList{Sites: list}
	if err != nil {
		r.Error = err.Error()
	}
	a.enrich(r.Sites)
	r.Missing = missingHostCount(allHosts(list))
	return r
}

// sites lists www folders with their hostname. Errors if two folders map to the same hostname.
func (a *App) sites() ([]Site, error) {
	type entry struct{ www, name string }
	var entries []entry
	have := map[string]bool{}
	for i, www := range a.wwwDirs() {
		list, err := os.ReadDir(www)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue // an extra folder that is gone (e.g. removed drive)
		}
		for _, e := range list {
			n := e.Name()
			if !e.IsDir() || strings.HasPrefix(n, ".") || n == "node_modules" || have[strings.ToLower(n)] {
				continue // same folder name in two projects folders: the first one wins (settings are keyed by name)
			}
			have[strings.ToLower(n)] = true
			entries = append(entries, entry{www, n})
		}
	}
	out := []Site{}
	seen := map[string]string{}
	var dup []string
	for _, e := range entries {
		n := e.name
		s := Site{Folder: n, Default: a.defaultHost(n), Root: filepath.Join(e.www, n)}
		if e.www != a.www() {
			s.Source = filepath.Base(e.www)
		}
		s.Host = s.Default
		if h := a.cfg.Hostnames[n]; h != "" {
			s.Host = h
		}
		if rel := a.cfg.SiteRoots[n]; rel != "" {
			s.RootRel = rel
			if rel != "." {
				s.Root = filepath.Join(s.Root, filepath.FromSlash(rel))
			}
		} else if st, err := os.Stat(filepath.Join(s.Root, "public")); err == nil && st.IsDir() {
			s.Root = filepath.Join(s.Root, "public")
		}
		s.Aliases = a.cfg.SiteAliases[n]
		s.Proxy = a.cfg.SiteProxy[n]
		if v := a.cfg.SitePHP[n]; v != "" && exists(a.phpDir(v)) {
			s.PHP = v
		}
		for _, h := range append([]string{s.Host}, s.Aliases...) {
			if !hostRe.MatchString(h) {
				dup = append(dup, fmt.Sprintf("%s (%s) is not a valid hostname", h, n))
			}
			if other, ok := seen[h]; ok {
				dup = append(dup, fmt.Sprintf("%s is used by both %s and %s", h, other, n))
			}
			seen[h] = n
		}
		out = append(out, s)
	}
	if len(dup) > 0 {
		return out, fmt.Errorf("hostname problems: %s", strings.Join(dup, "; "))
	}
	return out, nil
}

// SetHostname sets a custom hostname for a www folder; empty resets to the default.
func (a *App) SetHostname(folder, host string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.siteDir(folder); err != nil {
		return fmt.Errorf("no folder %q in www", folder)
	}
	host = a.normHost(host)
	if host == "" || host == a.defaultHost(folder) {
		delete(a.cfg.Hostnames, folder)
		return a.saveConfig()
	}
	if !hostRe.MatchString(host) {
		return fmt.Errorf("invalid hostname %q (use a-z, 0-9, '-', '_', '.')", host)
	}
	prev, had := a.cfg.Hostnames[folder]
	a.cfg.Hostnames[folder] = host
	if _, err := a.sites(); err != nil {
		if had {
			a.cfg.Hostnames[folder] = prev
		} else {
			delete(a.cfg.Hostnames, folder)
		}
		return err
	}
	return a.saveConfig()
}

var ruleRe = regexp.MustCompile(`^[a-z0-9_.-]*$`)

func (a *App) HostRules() []HostRule { return a.cfg.HostRules }

// SetHostRules replaces the naming rules; rejected if the result has invalid or duplicate hostnames.
func (a *App) SetHostRules(rules []HostRule) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	clean := []HostRule{}
	for _, r := range rules {
		r.From, r.To = strings.ToLower(strings.TrimSpace(r.From)), strings.ToLower(strings.TrimSpace(r.To))
		if r.From == "" && r.To == "" {
			continue
		}
		if r.Kind != "prefix" && r.Kind != "suffix" {
			return fmt.Errorf("rule kind must be prefix or suffix, got %q", r.Kind)
		}
		if r.From == "" || !ruleRe.MatchString(r.From) || !ruleRe.MatchString(r.To) {
			return fmt.Errorf("invalid rule %s %q -> %q (use a-z, 0-9, '-', '_', '.')", r.Kind, r.From, r.To)
		}
		clean = append(clean, r)
	}
	prev := a.cfg.HostRules
	a.cfg.HostRules = clean
	if _, err := a.sites(); err != nil {
		a.cfg.HostRules = prev
		return err
	}
	return a.saveConfig()
}

// syncVhostFiles writes one Apache vhost per site and removes stale auto.* ones.
// Returns all hostnames that should exist.
func (a *App) syncVhostFiles() (hosts []string, changed int, err error) {
	sitesDir := a.path(`etc\apache2\sites-enabled`)
	fwd := filepath.ToSlash
	list, err := a.sites()
	if err != nil {
		return nil, 0, err
	}
	hosts = allHosts(list)
	// One certificate for all sites, signed by the local CA; reissued only when hosts change.
	leafChanged, err := a.writeLeaf(hosts)
	if err != nil {
		return nil, 0, fmt.Errorf("ssl: %w", err)
	}
	if leafChanged {
		changed++
	}
	// PHP per site (Apache) and the Nginx config follow the same site list.
	if c, err := writeIfChanged(a.path(`etc\apache2\devpanel-php.conf`), a.apachePHPConf(list)); err != nil {
		return nil, changed, err
	} else if c {
		changed++
		a.ensureApacheInclude()
	}
	if c, err := a.writeNginxConf(list); err != nil {
		return nil, changed, err
	} else if c {
		changed++
	}
	for _, w := range []string{"caddy", "frankenphp"} {
		if c, err := a.writeCaddyConf(w, list); err != nil {
			return nil, changed, err
		} else if c {
			changed++
		}
	}
	if len(a.cfg.SiteProxy) > 0 {
		a.ensureApacheModules()
	}
	_, _, crt, key := a.sslFiles()
	keep := map[string]bool{}
	tpl := userTpl(a.apacheTplPath(), apacheTplDefault)
	for _, s := range list {
		alias := ""
		for _, al := range s.Aliases {
			alias += " " + al + " *." + al
		}
		conf := fillTpl(tpl, map[string]string{"ROOT": fwd(s.Root), "HOST": s.Host, "ALIASES": alias, "HTTP_PORT": strconv.Itoa(port("apache")),
			"SSL_PORT": strconv.Itoa(port("apache_ssl")), "CERT": fwd(crt), "KEY": fwd(key), "PROXY": apacheProxy(a.cfg.SiteProxy[s.Folder])})
		file := filepath.Join(sitesDir, "auto."+s.Host+".conf")
		keep[file] = true
		if old, _ := os.ReadFile(file); string(old) != conf {
			if err := os.WriteFile(file, []byte(conf), 0644); err != nil {
				return nil, changed, err
			}
			changed++
		}
	}
	stale, _ := filepath.Glob(filepath.Join(sitesDir, "auto.*.conf"))
	for _, f := range stale {
		if !keep[f] {
			os.Remove(f)
			changed++
		}
	}
	return hosts, changed, nil
}

// missingHosts returns hostnames not yet present in the hosts file content.
func missingHosts(content string, want []string) []string {
	have := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, f := range strings.Fields(line) {
			have[strings.ToLower(f)] = true
		}
	}
	var out []string
	for _, h := range want {
		if !have[strings.ToLower(h)] {
			out = append(out, h)
		}
	}
	return out
}

func (a *App) SyncVhosts() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hosts, changed, err := a.syncVhostFiles()
	if err != nil {
		return "", err
	}
	hostsFile := filepath.Join(os.Getenv("SystemRoot"), `System32\drivers\etc\hosts`)
	b, err := os.ReadFile(hostsFile)
	if err != nil {
		return "", err
	}
	missing := missingHosts(string(b), hosts)
	if len(missing) > 0 {
		// hosts is admin-only: append via an elevated helper (UAC prompt).
		var add strings.Builder
		if len(b) > 0 && b[len(b)-1] != '\n' {
			add.WriteString("\r\n")
		}
		for _, h := range missing {
			fmt.Fprintf(&add, "127.0.0.1      %s #devpanel\r\n", h)
		}
		tmp := filepath.Join(os.TempDir(), "devpanel-hosts.txt")
		os.WriteFile(tmp, []byte(add.String()), 0644)
		if err := a.elevated(fmt.Sprintf("@type \"%s\" >> \"%s\"\r\n", tmp, hostsFile)); err != nil {
			return "", fmt.Errorf("hosts update: %w", err)
		}
		if b, _ = os.ReadFile(hostsFile); len(missingHosts(string(b), missing)) > 0 {
			return "", errors.New("hosts file was not updated")
		}
	}
	if err := a.restartWeb(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d sites, %d vhost files changed, %d hosts entries added", len(hosts), changed, len(missing)), nil
}
