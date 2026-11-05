package main

// Use what's already installed: Laragon, XAMPP, WampServer, Laravel Herd and FlyEnv installs are found by
// their binaries and *linked* (directory junctions) into DevPanel's bin\<service>\. Nothing is copied or
// moved; their config files are not rewritten (Apache gets its own -f config, Nginx/Caddy always use ours).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// ---- Junction helpers ----

// isLink reports whether p is a junction or symlink (Go reports junctions as irregular files).
func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// realPath resolves junctions/symlinks to the real folder (filepath.EvalSymlinks doesn't follow junctions).
func realPath(p string) string {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	h, err := windows.CreateFile(u, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return p
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH*4)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil || n == 0 {
		return p
	}
	s := windows.UTF16ToString(buf[:n])
	s = strings.TrimPrefix(s, `\\?\UNC\`)
	return strings.TrimPrefix(s, `\\?\`)
}

// resolveDir: the folder processes really run from (junction target for linked installs).
func resolveDir(d string) string {
	if d != "" && isLink(d) {
		return realPath(d)
	}
	return d
}

func makeJunction(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		return err
	}
	out, err := hidden(exec.Command("cmd", "/c", "mklink", "/J", link, target)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isAdopted: the selected version of a service is linked from another environment.
func (a *App) isAdopted(name string) bool {
	d := a.svcDir(name)
	return d != "" && isLink(d)
}

// ---- Discovery ----

type ExternalItem struct {
	Service  string `json:"service"`
	Label    string `json:"label"`
	Dir      string `json:"dir"`
	Version  string `json:"version"`
	LinkName string `json:"linkName"`
	Linked   bool   `json:"linked"`
}

type ExternalEnv struct {
	Name   string         `json:"name"`
	Root   string         `json:"root"`
	WWW    string         `json:"www"` // its projects folder, if any
	Items  []ExternalItem `json:"items"`
	Custom bool           `json:"custom"` // a folder the user added (ExternalPaths)
}

// markers: which file proves an install, and the version folder relative to it.
var markers = []struct{ Service, Rel string }{
	{"php", "php-cgi.exe"}, {"apache", `bin\httpd.exe`}, {"nginx", "nginx.exe"}, {"mysql", `bin\mysqld.exe`},
	{"postgresql", `bin\pg_ctl.exe`}, {"redis", "redis-server.exe"}, {"mongodb", `bin\mongod.exe`},
	{"memcached", "memcached.exe"}, {"nodejs", "node.exe"}, {"mailpit", "mailpit.exe"}, {"caddy", "caddy.exe"},
}

// skipDirs are never descended into while scanning (projects, data, caches).
var skipDirs = map[string]bool{"node_modules": true, "www": true, "htdocs": true, "data": true, "logs": true, "tmp": true,
	"temp": true, "cache": true, "vendor": true, ".git": true, "backup": true, "sites": true}

// scanInstalls finds install folders under root by their marker binaries. Unlike filepath.WalkDir it also
// follows junctions/symlinks (e.g. a PHP folder moved to D:\ and linked back), with a depth limit and loop guard.
func scanInstalls(root string, depth int) map[string][]string {
	found := map[string][]string{}
	seen := map[string]bool{}
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		real := strings.ToLower(resolveDir(dir))
		if level > depth || seen[real] {
			return
		}
		seen[real] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			isDir := e.IsDir()
			if !isDir && e.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 { // junction or symlink to a folder
				st, err := os.Stat(p)
				isDir = err == nil && st.IsDir()
			}
			if isDir {
				if !skipDirs[strings.ToLower(e.Name())] {
					walk(p, level+1)
				}
				continue
			}
			for _, m := range markers {
				if strings.EqualFold(e.Name(), filepath.Base(m.Rel)) && strings.HasSuffix(strings.ToLower(p), strings.ToLower(`\`+m.Rel)) {
					d := p[:len(p)-len(m.Rel)-1]
					if !slices.Contains(found[m.Service], d) {
						found[m.Service] = append(found[m.Service], d)
					}
				}
			}
		}
	}
	walk(root, 0)
	return found
}

// flyEnvRoots: FlyEnv keeps downloads in <data>\app; portable builds keep <data> next to the exe.
func flyEnvRoots() []string {
	var roots []string
	add := func(p string) {
		if exists(p) && !slices.Contains(roots, p) {
			roots = append(roots, p)
		}
	}
	home := os.Getenv("USERPROFILE")
	for _, d := range []string{filepath.Join(os.Getenv("APPDATA"), "FlyEnv"), filepath.Join(home, `Downloads\FlyEnv-Data`), filepath.Join(home, `Desktop\FlyEnv-Data`)} {
		add(filepath.Join(d, "app"))
		// extra binary folders the user registered in FlyEnv (user.json setup.<module>.dirs)
		if b, err := os.ReadFile(filepath.Join(d, "user.json")); err == nil {
			var u struct {
				Setup map[string]json.RawMessage `json:"setup"`
			}
			json.Unmarshal(b, &u)
			for _, raw := range u.Setup {
				var m struct {
					Dirs []string `json:"dirs"`
				}
				if json.Unmarshal(raw, &m) == nil {
					for _, x := range m.Dirs {
						add(x)
					}
				}
			}
		}
	}
	return roots
}

var linkSafe = regexp.MustCompile(`[^a-z0-9._-]+`)

type extSource struct {
	name, tag, www string
	roots          []string
	depth          int
}

// externalSources lists where other environments live by default (replaced in tests).
var externalSources = func() []extSource {
	home := os.Getenv("USERPROFILE")
	return []extSource{
		{"Laragon", "laragon", `C:\laragon\www`, []string{`C:\laragon\bin`}, 4},
		{"XAMPP", "xampp", `C:\xampp\htdocs`, []string{`C:\xampp`}, 3},
		{"WampServer", "wamp", `C:\wamp64\www`, []string{`C:\wamp64\bin`, `C:\wamp\bin`}, 4},
		{"Laravel Herd", "herd", filepath.Join(home, "Herd"), []string{filepath.Join(home, `.config\herd\bin`)}, 3},
		{"FlyEnv", "flyenv", "", flyEnvRoots(), 4},
	}
}

// customSource scans a folder the user picked; its name is guessed from the control panel exe it contains.
func customSource(dir string) extSource {
	name := "Folder"
	for exe, n := range map[string]string{"xampp-control.exe": "XAMPP", "laragon.exe": "Laragon", "wampmanager.exe": "WampServer", "FlyEnv.exe": "FlyEnv", "Herd.exe": "Laravel Herd"} {
		if exists(filepath.Join(dir, exe)) {
			name = n
		}
	}
	www := ""
	for _, w := range []string{"htdocs", "www"} {
		if exists(filepath.Join(dir, w)) {
			www = filepath.Join(dir, w)
			break
		}
	}
	// the tag carries the drive so C:\xampp and D:\xampp don't get the same link names
	tag := strings.TrimSuffix(filepath.VolumeName(dir), ":") + "-" + filepath.Base(dir)
	return extSource{name + " (" + dir + ")", linkSafe.ReplaceAllString(strings.ToLower(tag), "-"), www, []string{dir}, 5}
}

// knownRoot: dir is inside or around a place the default scan already covers.
func knownRoot(dir string) string {
	d := strings.ToLower(filepath.Clean(dir)) + `\`
	for _, s := range externalSources() {
		for _, r := range s.roots {
			r := strings.ToLower(filepath.Clean(r)) + `\`
			if exists(r) && (strings.HasPrefix(d, r) || strings.HasPrefix(r, d)) {
				return s.name
			}
		}
	}
	return ""
}

// ExternalEnvironments finds other dev environments on this PC and what they have installed.
func (a *App) ExternalEnvironments() []ExternalEnv {
	sources := externalSources()
	custom := len(sources)
	for _, p := range slices.Clone(a.cfg.ExternalPaths) {
		sources = append(sources, customSource(p))
	}
	ownBin := strings.ToLower(filepath.Clean(a.path("bin"))) + `\`
	out := []ExternalEnv{}
	for i, s := range sources {
		env := ExternalEnv{Name: s.name, Items: []ExternalItem{}, Custom: i >= custom}
		if exists(s.www) {
			env.WWW = s.www
		}
		for _, root := range s.roots {
			if !exists(root) {
				continue
			}
			if env.Root == "" {
				env.Root = root
			}
			for svcName, dirs := range scanInstalls(root, s.depth) {
				for _, d := range dirs {
					if strings.HasPrefix(strings.ToLower(d)+`\`, ownBin) { // already DevPanel's own (e.g. running inside Laragon)
						continue
					}
					link := linkSafe.ReplaceAllString(strings.ToLower(s.tag+"-"+filepath.Base(d)), "-")
					ver := ""
					if v, ok := a.installedVersionAt(svcName, d); ok {
						ver = verString(v)
					}
					linked := exists(filepath.Join(a.path(`bin\`+svcName), link))
					env.Items = append(env.Items, ExternalItem{svcName, serviceLabel(svcName), d, ver, link, linked})
				}
			}
		}
		if env.Custom && env.Root == "" {
			env.Root = s.roots[0] // added by the user but gone (e.g. a USB drive): still listed so it can be removed
		}
		if len(env.Items) > 0 || env.WWW != "" || env.Custom {
			slices.SortFunc(env.Items, func(x, y ExternalItem) int { return strings.Compare(x.Service+x.Dir, y.Service+y.Dir) })
			out = append(out, env)
		}
	}
	return out
}

// installedVersionAt reads the version of an install folder that may live anywhere.
func (a *App) installedVersionAt(name, dir string) (semver, bool) {
	if v, parts, ok := parseVer(filepath.Base(dir)); ok && parts == 3 {
		return v, true
	}
	if c, has := versionCmds[name]; has {
		if out, _ := output(filepath.Join(dir, c[0]), c[1:]...); out != "" {
			if v, p, ok := parseVer(dottedVer.FindString(out)); ok && p >= 2 {
				return v, true
			}
		}
	}
	return semver{}, false
}

// LinkExternal makes an install from another environment usable in DevPanel (a junction, nothing copied).
func (a *App) LinkExternal(service, dir string) (string, error) {
	var item *ExternalItem
	for _, e := range a.ExternalEnvironments() { // only what the scan found, never an arbitrary path
		for _, it := range e.Items {
			if it.Service == service && strings.EqualFold(it.Dir, dir) {
				it := it
				item = &it
			}
		}
	}
	if item == nil {
		return "", fmt.Errorf("%s at %s was not found by the scan", service, dir)
	}
	link := filepath.Join(a.path(`bin\`+service), item.LinkName)
	if exists(link) {
		return item.LinkName + " is already linked", nil
	}
	if err := makeJunction(link, item.Dir); err != nil {
		return "", err
	}
	if svc(service).Name != "" {
		a.SetServiceEnabled(service, true)
	}
	return fmt.Sprintf("linked %s (%s) as %s; pick it in Environment or the service's Version", item.Dir, item.Label, item.LinkName), nil
}

// UnlinkExternal removes only the junction; the other environment's files are untouched.
func (a *App) UnlinkExternal(service, linkName string) error {
	if strings.ContainsAny(linkName, `\/`) || linkName == "" || linkName == ".." {
		return errors.New("invalid link name")
	}
	p := filepath.Join(a.path(`bin\`+service), linkName)
	if !isLink(p) {
		return errors.New("not a linked install")
	}
	if folderName(a.svcDir(service)) == linkName && svc(service).Name != "" && a.running(service) {
		return fmt.Errorf("stop %s first", service)
	}
	return os.Remove(p) // a junction: removes the link only
}

// AddExternalPath adds a folder to the scan, e.g. XAMPP on D:\ or a portable Laragon.
func (a *App) AddExternalPath(dir string) (string, error) {
	dir = strings.Trim(strings.TrimSpace(dir), `"`)
	if !filepath.IsAbs(dir) {
		return "", errors.New(`use a full path, e.g. D:\xampp`)
	}
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("folder %s does not exist", dir)
	}
	own, d := strings.ToLower(filepath.Clean(a.cfg.Root))+`\`, strings.ToLower(dir)+`\`
	if strings.HasPrefix(d, own) || strings.HasPrefix(own, d) {
		return "", errors.New("that is DevPanel's own folder")
	}
	if n := knownRoot(dir); n != "" {
		return "", fmt.Errorf("%s is already scanned (%s)", dir, n)
	}
	if slices.ContainsFunc(a.cfg.ExternalPaths, func(p string) bool { return strings.EqualFold(p, dir) }) {
		return "", errors.New("already added")
	}
	src := customSource(dir)
	n := 0
	for _, dirs := range scanInstalls(dir, src.depth) {
		n += len(dirs)
	}
	if n == 0 && src.www == "" {
		return "", fmt.Errorf("no PHP, web server, database or runtime found in %s", dir)
	}
	a.mu.Lock()
	a.cfg.ExternalPaths = append(a.cfg.ExternalPaths, dir)
	err := a.saveConfig()
	a.mu.Unlock()
	return fmt.Sprintf("%s: %d install(s) found", src.name, n), err
}

// RemoveExternalPath takes a folder off the scan; its linked installs must be unlinked first.
func (a *App) RemoveExternalPath(dir string) error {
	for _, e := range a.ExternalEnvironments() {
		if e.Custom && strings.EqualFold(e.Root, dir) {
			for _, it := range e.Items {
				if it.Linked {
					return fmt.Errorf("unlink %s first", it.LinkName)
				}
			}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.ExternalPaths = slices.DeleteFunc(a.cfg.ExternalPaths, func(p string) bool { return strings.EqualFold(p, dir) })
	return a.saveConfig()
}

// ExternalHint returns the other environments once (first start), so the window can offer to link them.
func (a *App) ExternalHint() []ExternalEnv {
	a.mu.Lock()
	if a.cfg.ExternalHinted {
		a.mu.Unlock()
		return nil
	}
	a.cfg.ExternalHinted = true
	a.saveConfig()
	a.mu.Unlock()
	return slices.DeleteFunc(a.ExternalEnvironments(), func(e ExternalEnv) bool { return len(e.Items) == 0 })
}

// UseExternalWWW serves another environment's projects folder too (e.g. XAMPP's htdocs, ~/Herd).
func (a *App) UseExternalWWW(name string) error {
	for _, e := range a.ExternalEnvironments() {
		if e.Name == name && e.WWW != "" {
			return a.AddWWW(e.WWW)
		}
	}
	return fmt.Errorf("%s has no projects folder", name)
}

// ---- Apache from another environment: own config file, theirs stays untouched ----

func (a *App) adoptedApacheConf() string { return a.path(`etc\apache2\httpd-devpanel.conf`) }

// apacheArgs are the extra httpd arguments: -f DevPanel's config for a linked Apache.
func (a *App) apacheArgs() []string {
	if a.isAdopted("apache") {
		return []string{"-f", a.adoptedApacheConf()}
	}
	return nil
}

// writeAdoptedApacheConf renders DevPanel's httpd config for a linked Apache (ServerRoot = its real folder).
func (a *App) writeAdoptedApacheConf(dir string) error {
	fwd := filepath.ToSlash
	_, _, crt, key := a.sslFiles()
	vals := map[string]string{"SRV": fwd(resolveDir(dir)), "PORT": fmt.Sprint(port("apache")), "SSLPORT": fmt.Sprint(port("apache_ssl")),
		"WWW": fwd(a.www()), "ETC": fwd(a.path(`etc\apache2`)), "CERT": fwd(crt), "KEY": fwd(key)}
	conf := fillTpl(standaloneHttpdConf, vals)
	conf = strings.Replace(conf, "# Generated by DevPanel for a standalone Apache. The original is kept as httpd.conf.dist.",
		"# Generated by DevPanel for an Apache linked from another environment. Its own httpd.conf is not used or changed.", 1)
	conf = strings.ReplaceAll(conf, `ErrorLog "logs/error.log"`, `ErrorLog "`+fwd(a.path(`data\apache\error.log`))+`"`)
	conf = strings.ReplaceAll(conf, `CustomLog "logs/access.log" combined`, `CustomLog "`+fwd(a.path(`data\apache\access.log`))+`" combined`)
	conf += "PidFile \"" + fwd(a.path(`data\apache\httpd.pid`)) + "\"\n"
	os.MkdirAll(a.path(`data\apache`), 0755)
	_, err := writeIfChanged(a.adoptedApacheConf(), conf)
	return err
}

// findMyIni: Laragon/DevPanel keep my.ini in the version folder, XAMPP in bin\.
func findMyIni(dir string) string {
	for _, p := range []string{filepath.Join(dir, "my.ini"), filepath.Join(dir, `bin\my.ini`)} {
		if exists(p) {
			return p
		}
	}
	return filepath.Join(dir, "my.ini")
}
