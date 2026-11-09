package main

// Living next to other environments: who owns a busy port, keeping links in step with what the other tools
// download (auto-link, dangling links removed, versions shared with Laragon), and pointing a project's .env
// at DevPanel's databases.

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- Port owners ----

var procGetExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// listenPIDs maps listening TCP ports (IPv4 and IPv6) to the owning process.
func listenPIDs() map[int]uint32 {
	out := map[int]uint32{}
	for _, af := range []struct {
		family        uintptr
		rowSize, port int // MIB_TCPROW_OWNER_PID / MIB_TCP6ROW_OWNER_PID: offset of dwLocalPort; PID is the last field
	}{{windows.AF_INET, 24, 8}, {windows.AF_INET6, 56, 20}} {
		var size uint32
		procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, af.family, 3 /* TCP_TABLE_OWNER_PID_LISTENER */, 0)
		if size == 0 {
			continue
		}
		buf := make([]byte, size+1024) // the table can grow between the two calls
		size = uint32(len(buf))
		if r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af.family, 3, 0); r != 0 {
			continue
		}
		n := int(*(*uint32)(unsafe.Pointer(&buf[0])))
		for i := 0; i < n && 4+(i+1)*af.rowSize <= len(buf); i++ {
			row := buf[4+i*af.rowSize:]
			p := int(row[af.port])<<8 | int(row[af.port+1]) // network byte order
			out[p] = *(*uint32)(unsafe.Pointer(&row[af.rowSize-4]))
		}
	}
	return out
}

// envHomes: folders whose processes belong to an environment (its binaries can live outside the scanned bin).
func (a *App) envHomes() [][2]string {
	var out [][2]string
	for _, s := range externalSources() {
		for _, r := range s.roots {
			out = append(out, [2]string{s.name, r})
			if strings.EqualFold(filepath.Base(r), "bin") {
				out = append(out, [2]string{s.name, filepath.Dir(r)})
			}
		}
	}
	for _, p := range a.cfg.ExternalPaths {
		out = append(out, [2]string{customSource(p).name, p})
	}
	pf, local := os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")
	return append(out, [2]string{"Laravel Herd", filepath.Join(pf, "Herd")}, // Herd's nginx ships inside the app
		[2]string{"FlyEnv", filepath.Join(pf, "FlyEnv")}, [2]string{"FlyEnv", filepath.Join(local, `Programs\FlyEnv`)})
}

// ownerOf names the environment an exe belongs to, else the exe's file name.
func (a *App) ownerOf(exe string) string {
	l := strings.ToLower(exe)
	for _, h := range a.envHomes() {
		if strings.HasPrefix(l, strings.ToLower(filepath.Clean(h[1]))+`\`) {
			return h[0] + " (" + filepath.Base(exe) + ")"
		}
	}
	if exe == "" {
		return "another program"
	}
	return filepath.Base(exe)
}

// ownPortHolder: another of DevPanel's own services holds the port (Apache vs Nginx on 80): not a foreign owner.
func (a *App) ownPortHolder(paths []string, name string, p int) bool {
	for _, s := range services {
		if s.Name != name && port(s.Name) == p && a.ownedIn(paths, s.Name) {
			return true
		}
	}
	return false
}

// portOwner: who listens on a port DevPanel's own service doesn't hold ("" = free).
func (a *App) portOwner(p int) string {
	pid, ok := listenPIDs()[p]
	if !ok {
		return ""
	}
	return a.ownerOf(exePath(pid))
}

// ---- Versions: cached, so scans don't run php -v again and again ----

var verCache sync.Map // lower(dir) -> string ("" = unknown)

func (a *App) versionOf(name, dir string) string {
	key := name + "|" + strings.ToLower(resolveDir(dir))
	if v, ok := verCache.Load(key); ok {
		return v.(string)
	}
	v := ""
	if sv, ok := a.installedVersionAt(name, dir); ok {
		v = verString(sv)
	}
	verCache.Store(key, v)
	return v
}

// ownVersions: version -> folder of DevPanel's own (not linked) installs of a service.
func (a *App) ownVersions(name string) map[string]string {
	out := map[string]string{}
	for _, f := range a.versions(name) {
		d := a.path(`bin\` + name + `\` + f)
		if !isLink(d) {
			if v := a.versionOf(name, d); v != "" {
				out[v] = f
			}
		}
	}
	return out
}

// markSame flags installs whose exact version is already available (own install, an existing link, or an
// earlier environment in the list), so the same PHP 8.5.12 isn't linked twice.
func (a *App) markSame(envs []ExternalEnv) {
	have := map[string]string{} // service|version -> where
	for _, e := range envs {
		for _, it := range e.Items {
			if _, ok := have[it.Service]; !ok {
				have[it.Service] = ""
				for v, f := range a.ownVersions(it.Service) {
					have[it.Service+"|"+v] = f
				}
			}
			if it.Linked && it.Version != "" {
				have[it.Service+"|"+it.Version] = it.LinkName
			}
		}
	}
	for i := range envs {
		for j := range envs[i].Items {
			it := &envs[i].Items[j]
			if it.Linked || it.Version == "" {
				continue
			}
			k := it.Service + "|" + it.Version
			if w := have[k]; w != "" {
				it.Same = w
			} else {
				have[k] = envs[i].Name + " " + filepath.Base(it.Dir)
			}
		}
	}
}

// ---- Keeping in step ----

// SetAutoLink: new versions downloaded in that environment are linked automatically (and removed ones unlinked).
func (a *App) SetAutoLink(env string, on bool) error {
	a.mu.Lock()
	a.cfg.AutoLink = slices.DeleteFunc(a.cfg.AutoLink, func(n string) bool { return n == env })
	if on {
		a.cfg.AutoLink = append(a.cfg.AutoLink, env)
	}
	err := a.saveConfig()
	a.mu.Unlock()
	if err == nil {
		_, err = a.SyncExternal()
	}
	return err
}

// SetShareLaragon: DevPanel's own versions show up in Laragon too (folder links in Laragon's bin).
func (a *App) SetShareLaragon(on bool) error {
	if on && !a.Standalone() {
		return errors.New("DevPanel already uses Laragon's folders")
	}
	a.mu.Lock()
	a.cfg.ShareLaragon = on
	err := a.saveConfig()
	a.mu.Unlock()
	if err == nil {
		_, err = a.SyncExternal()
	}
	return err
}

// laragonBin: Laragon's bin folder when Laragon is installed and DevPanel is not inside it.
func (a *App) laragonBin() string {
	for _, s := range externalSources() {
		if s.name == "Laragon" && len(s.roots) > 0 && exists(s.roots[0]) && a.Standalone() {
			return s.roots[0]
		}
	}
	return ""
}

// Laragon lists any folder under bin\<service>; these are the ones it knows.
var laragonServices = []string{"php", "apache", "nginx", "mysql", "postgresql", "redis", "nodejs", "memcached", "mongodb", "mailpit"}

const sharePrefix = "devpanel-"

// SyncExternal brings links up to date; returns a short summary of what changed.
func (a *App) SyncExternal() (string, error) {
	var notes []string
	var errs []error
	if len(a.cfg.AutoLink) > 0 {
		envs := a.ExternalEnvironments()
		tags := map[string]bool{}
		for _, e := range envs {
			if !slices.Contains(a.cfg.AutoLink, e.Name) {
				continue
			}
			tags[e.Tag] = true
			for _, it := range e.Items {
				if it.Linked || it.Same != "" {
					continue
				}
				if err := makeJunction(filepath.Join(a.path(`bin\`+it.Service), it.LinkName), it.Dir); err != nil {
					errs = append(errs, err)
					continue
				}
				notes = append(notes, "linked "+it.LinkName)
			}
		}
		// versions the other tool removed: drop the dead links (only of auto-linked environments, never a running one)
		for _, svcDir := range globDirs(a.path("bin")) {
			for _, f := range globDirs(svcDir) {
				if !isLink(f) || !slices.ContainsFunc(slices.Collect(maps.Keys(tags)), func(t string) bool { return strings.HasPrefix(filepath.Base(f), t+"-") }) {
					continue
				}
				if _, err := os.Stat(f); err != nil && os.Remove(f) == nil { // Stat follows the link: error = target gone
					notes = append(notes, "unlinked "+filepath.Base(f)+" (removed in its environment)")
				}
			}
		}
	}
	if lb := a.laragonBin(); lb != "" {
		own := strings.ToLower(filepath.Clean(a.path("bin"))) + `\`
		for _, name := range laragonServices {
			// ours that are gone or no longer wanted
			for _, f := range globDirs(filepath.Join(lb, name)) {
				if strings.HasPrefix(filepath.Base(f), sharePrefix) && isLink(f) {
					target := strings.ToLower(realPath(f))
					_, err := os.Stat(f)
					if !a.cfg.ShareLaragon || err != nil || !strings.HasPrefix(target+`\`, own) {
						if os.Remove(f) == nil {
							notes = append(notes, "removed "+filepath.Base(f)+" from Laragon")
						}
					}
				}
			}
			if !a.cfg.ShareLaragon {
				continue
			}
			for _, f := range a.versions(name) {
				src := a.path(`bin\` + name + `\` + f)
				dst := filepath.Join(lb, name, sharePrefix+f)
				if isLink(src) || exists(dst) || a.laragonHas(lb, name, a.versionOf(name, src)) {
					continue // linked from elsewhere, already shared, or Laragon has that version itself
				}
				if err := makeJunction(dst, src); err != nil {
					errs = append(errs, err)
					continue
				}
				notes = append(notes, "shared "+f+" with Laragon")
			}
		}
	}
	return strings.Join(notes, "; "), errors.Join(errs...)
}

// laragonHas: Laragon has its own install of that exact version.
func (a *App) laragonHas(lb, name, ver string) bool {
	if ver == "" {
		return false
	}
	for _, f := range globDirs(filepath.Join(lb, name)) {
		if !isLink(f) && a.versionOf(name, f) == ver {
			return true
		}
	}
	return false
}

func globDirs(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() || e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// watchExternal keeps links in step while the agent runs (cheap when nothing is switched on).
func (a *App) watchExternal() {
	for {
		if len(a.cfg.AutoLink) > 0 || a.cfg.ShareLaragon || a.laragonShared() {
			if msg, err := a.SyncExternal(); msg != "" || err != nil {
				if err != nil {
					msg += " " + err.Error()
				}
				a.emitEvent("log", "Environments: "+strings.TrimSpace(msg), err != nil)
			}
		}
		time.Sleep(time.Minute)
	}
}

// laragonShared: leftover links in Laragon to clean up after sharing was turned off.
func (a *App) laragonShared() bool {
	lb := a.laragonBin()
	if lb == "" {
		return false
	}
	for _, name := range laragonServices {
		for _, f := range globDirs(filepath.Join(lb, name)) {
			if strings.HasPrefix(filepath.Base(f), sharePrefix) {
				return true
			}
		}
	}
	return false
}

// ---- Projects using DevPanel's databases (e.g. a Herd site) ----

// projectDir accepts a site folder name or a full path inside a projects folder (DevPanel's or another env's).
func (a *App) projectDir(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return a.siteDir(p)
	}
	p = filepath.Clean(p)
	roots := a.wwwDirs()
	for _, e := range a.ExternalEnvironments() {
		roots = append(roots, e.WWW)
	}
	for _, r := range roots {
		if r != "" && strings.EqualFold(filepath.Dir(p), filepath.Clean(r)) && exists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not a project in a projects folder", p)
}

// ProjectsIn lists the project folders of a projects folder (for another environment's www).
func (a *App) ProjectsIn(www string) []string {
	out := []string{}
	for _, d := range globDirs(www) {
		if exists(filepath.Join(d, ".env")) || exists(filepath.Join(d, ".env.example")) {
			out = append(out, filepath.Base(d))
		}
	}
	return out
}

// UseDevPanelDB points a project's .env at DevPanel's MySQL or PostgreSQL (engine "" = keep the project's
// choice, else the running one). The old values are returned so nothing is lost.
func (a *App) UseDevPanelDB(project, engine string) (string, error) {
	dir, err := a.projectDir(project)
	if err != nil {
		return "", err
	}
	f := filepath.Join(dir, ".env")
	b, err := os.ReadFile(f)
	if err != nil {
		if b, err = os.ReadFile(filepath.Join(dir, ".env.example")); err != nil {
			return "", errors.New("project has no .env or .env.example")
		}
	}
	env := string(b)
	if engine == "" {
		switch cur := envValue(env, "DB_CONNECTION"); {
		case cur == "pgsql" || cur == "mysql" || cur == "mariadb":
			engine = cur
		case a.running("postgresql") && !a.running("mysql"):
			engine = "pgsql"
		default:
			engine = "mysql"
		}
	}
	svcName, user, pw := "mysql", "root", a.cfg.MySQLPassword
	switch engine {
	case "pgsql":
		svcName, user, pw = "postgresql", "postgres", ""
	case "mysql", "mariadb":
	default:
		return "", fmt.Errorf("engine must be mysql or pgsql, not %q", engine)
	}
	if a.svcDir(svcName) == "" {
		return "", fmt.Errorf("%s is not installed in DevPanel", svcName)
	}
	db := envValue(env, "DB_DATABASE")
	if db == "" || strings.Contains(db, "database.sqlite") || strings.ContainsAny(db, `\/`) {
		db = dbNameFor(filepath.Base(dir))
	}
	vals := [][2]string{{"DB_CONNECTION", engine}, {"DB_HOST", "127.0.0.1"}, {"DB_PORT", strconv.Itoa(port(svcName))},
		{"DB_DATABASE", db}, {"DB_USERNAME", user}, {"DB_PASSWORD", pw}}
	if envValue(env, "REDIS_HOST") != "" {
		vals = append(vals, [2]string{"REDIS_HOST", "127.0.0.1"}, [2]string{"REDIS_PORT", strconv.Itoa(port("redis"))})
	}
	var old []string
	for _, kv := range vals {
		if v := envValue(env, kv[0]); v != kv[1] {
			old = append(old, kv[0]+"="+v)
		}
		env = setEnv(env, kv[0], kv[1])
	}
	if err := os.WriteFile(f, []byte(env), 0644); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s now uses DevPanel's %s (database %s)", filepath.Base(dir), svcName, db)
	if len(old) > 0 {
		msg += "; previous: " + strings.Join(old, " ")
	}
	if !a.running(svcName) {
		msg += "; start " + svcName + " and create the database if needed"
	}
	return msg + " (php artisan config:clear if config is cached)", nil
}

// envValue reads an active KEY=value from .env content (quotes removed).
func envValue(env, key string) string {
	for _, l := range strings.Split(env, "\n") {
		l = strings.TrimSpace(l)
		if k, v, ok := strings.Cut(l, "="); ok && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

func (a *App) ShareLaragon() bool { return a.cfg.ShareLaragon }
