package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Package struct {
	Name      string `json:"name"`
	Type      string `json:"type"`  // php, apache, ... or "" when guessed from the name
	Group     string `json:"group"` // target folder under root, e.g. bin\php
	Folder    string `json:"folder"`
	URL       string `json:"url"`
	Source    string `json:"source"`           // "Laragon", "Custom", "Updates" or the source URL/path
	SHA256    string `json:"sha256,omitempty"` // verified after download when known
	Installed bool   `json:"installed"`
}

// CustomPkg is a single package added from the UI (stored in devpanel.json).
type CustomPkg struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

// Package types and where they live. Order matters for name guessing ("phpmyadmin" before "php").
var pkgTypes = []struct{ Type, Dir, NamePrefix string }{
	{"phpmyadmin", `etc\apps`, "phpmyadmin"},
	{"php", `bin\php`, "php"},
	{"apache", `bin\apache`, "apache"},
	{"nginx", `bin\nginx`, "nginx"},
	{"nodejs", `bin\nodejs`, "node"},
	{"mysql", `bin\mysql`, "mysql"},
	{"mariadb", `bin\mysql`, "mariadb"},
	{"postgresql", `bin\postgresql`, "postgresql"},
	{"redis", `bin\redis`, "redis"},
	{"mailpit", `bin\mailpit`, "mailpit"},
	{"go", `bin\go`, "go-"},
	{"python", `bin\python`, "python"},
	{"caddy", `bin\caddy`, "caddy"},
	{"frankenphp", `bin\frankenphp`, "frankenphp"},
	{"meilisearch", `bin\meilisearch`, "meilisearch"},
	{"ollama", `bin\ollama`, "ollama"},
	{"elasticsearch", `bin\elasticsearch`, "elasticsearch"},
	{"rabbitmq", `bin\rabbitmq`, "rabbitmq"},
	{"erlang", `bin\erlang`, "erlang"},
	{"java", `bin\java`, "java"},
	{"dotnet", `bin\dotnet`, "dotnet"},
	{"ruby", `bin\ruby`, "ruby"},
	{"bun", `bin\bun`, "bun"},
	{"deno", `bin\deno`, "deno"},
	{"ngrok", `bin\ngrok`, "ngrok"},
	{"memcached", `bin\memcached`, "memcached"},
	{"mongodb", `bin\mongodb`, "mongodb"},
}

func (a *App) PackageTypes() []string {
	out := []string{}
	for _, t := range pkgTypes {
		out = append(out, t.Type)
	}
	return out
}

// pkgTarget maps a catalog entry to where Laragon keeps it. typ "" guesses from the name.
func pkgTarget(name, rawURL, typ string) (group, folder string) {
	n := strings.ToLower(strings.TrimPrefix(name, "*"))
	folder = n
	if u, err := url.Parse(rawURL); err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		folder = strings.TrimSuffix(strings.TrimSuffix(path.Base(u.Path), ".zip"), ".windows-amd64")
	}
	for _, t := range pkgTypes {
		if t.Type == typ || (typ == "" && strings.HasPrefix(n, t.NamePrefix)) {
			if t.Type == "phpmyadmin" {
				return t.Dir, "phpMyAdmin"
			}
			return t.Dir, folder
		}
	}
	return `bin\` + n, folder
}

// parseCatalog reads Laragon's packages.conf format: `name=url`, `#` comments, `---` separators.
func (a *App) parseCatalog(r io.Reader, source string) []Package {
	out := []Package{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		name, u, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !strings.HasPrefix(strings.TrimSpace(u), "http") {
			continue
		}
		out = append(out, a.pkg(strings.TrimPrefix(strings.TrimSpace(name), "*"), strings.TrimSpace(u), "", source))
	}
	return out
}

func (a *App) pkg(name, u, typ, source string) Package {
	g, folder := pkgTarget(name, u, typ)
	_, err := os.Stat(a.path(filepath.Join(g, folder)))
	return Package{Name: name, Type: typ, Group: g, Folder: folder, URL: u, Source: source, Installed: err == nil}
}

// readSource returns a catalog from a URL (cached until RefreshSources) or a local file.
func (a *App) readSource(src string) (string, error) {
	a.srcMu.Lock()
	defer a.srcMu.Unlock()
	if s, ok := a.srcCache[src]; ok {
		return s, nil
	}
	var b []byte
	var err error
	if validURL(src) {
		c := http.Client{Timeout: 15 * time.Second}
		var resp *http.Response
		if resp, err = c.Get(src); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			} else {
				b, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			}
		}
	} else {
		b, err = os.ReadFile(src)
	}
	if err != nil {
		return "", err
	}
	if a.srcCache == nil {
		a.srcCache = map[string]string{}
	}
	a.srcCache[src] = string(b)
	return string(b), nil
}

type PackageList struct {
	Packages []Package `json:"packages"`
	Sources  []string  `json:"sources"`
	Errors   []string  `json:"errors"`
}

// Packages merges Laragon's usr\packages.conf, custom packages and extra sources.
// A name already seen earlier in that order is skipped.
func (a *App) Packages() PackageList {
	r := PackageList{Packages: []Package{}, Sources: append([]string{}, a.cfg.Sources...), Errors: []string{}}
	var all []Package
	if f, err := os.Open(a.path(`usr\packages.conf`)); err == nil {
		all = append(all, a.parseCatalog(f, "Laragon")...)
		f.Close()
	} else {
		if !os.IsNotExist(err) { // a standalone install simply has no Laragon catalog
			r.Errors = append(r.Errors, `usr\packages.conf: `+err.Error())
		}
	}
	for _, c := range a.cfg.Packages {
		all = append(all, a.pkg(c.Name, c.URL, c.Type, "Custom"))
	}
	for _, src := range a.cfg.Sources {
		s, err := a.readSource(src)
		if err != nil {
			r.Errors = append(r.Errors, src+": "+err.Error())
			continue
		}
		all = append(all, a.parseCatalog(strings.NewReader(s), src)...)
	}
	a.srcMu.Lock()
	all = append(all, a.updates...)
	a.srcMu.Unlock()
	for _, c := range builtinPackages { // last: catalog entries with the same name win
		all = append(all, a.pkg(c.Name, c.URL, c.Type, "Built-in"))
	}
	seen := map[string]bool{}
	for _, p := range all {
		if k := strings.ToLower(p.Name); !seen[k] {
			seen[k] = true
			r.Packages = append(r.Packages, p)
		}
	}
	return r
}

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (a *App) AddPackage(name, u, typ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	name, u = strings.TrimSpace(name), strings.TrimSpace(u)
	if name == "" || strings.ContainsAny(name, `=\/:*?"<>|`) {
		return errors.New(`package name is required and cannot contain = \ / : * ? " < > |`)
	}
	if !validURL(u) {
		return errors.New("URL must start with http:// or https://")
	}
	if typ != "" && !slices.Contains(a.PackageTypes(), typ) {
		return fmt.Errorf("unknown type %q", typ)
	}
	for _, p := range a.Packages().Packages {
		if strings.EqualFold(p.Name, name) {
			return fmt.Errorf("a package named %q already exists (%s)", name, p.Source)
		}
	}
	a.cfg.Packages = append(a.cfg.Packages, CustomPkg{name, u, typ})
	return a.saveConfig()
}

func (a *App) RemovePackage(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Packages = slices.DeleteFunc(a.cfg.Packages, func(p CustomPkg) bool { return p.Name == name })
	return a.saveConfig()
}

// AddSource adds a catalog in packages.conf format: an http(s) URL or a local file path.
func (a *App) AddSource(src string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	src = strings.Trim(strings.TrimSpace(src), `"`)
	if !validURL(src) {
		if st, err := os.Stat(src); err != nil || st.IsDir() {
			return errors.New("source must be an http(s) URL or an existing file")
		}
	}
	if slices.Contains(a.cfg.Sources, src) {
		return errors.New("source already added")
	}
	if _, err := a.readSource(src); err != nil {
		return fmt.Errorf("cannot read source: %w", err)
	}
	a.cfg.Sources = append(a.cfg.Sources, src)
	return a.saveConfig()
}

func (a *App) RemoveSource(src string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Sources = slices.DeleteFunc(a.cfg.Sources, func(s string) bool { return s == src })
	return a.saveConfig()
}

// EditCatalog opens usr\packages.conf, or a local source file, in Laragon's Notepad++ (or Notepad).
// Only catalog files are allowed, not arbitrary paths. Click Refresh after saving.
func (a *App) EditCatalog(file string) error {
	laragon := a.path(`usr\packages.conf`)
	if file == "" {
		file = laragon
	}
	if file != laragon && (validURL(file) || !slices.Contains(a.cfg.Sources, file)) {
		return errors.New("only usr\\packages.conf and local source files can be edited")
	}
	return a.openInEditor(file)
}

// RefreshSources drops cached remote catalogs so the next Packages() call refetches them.
func (a *App) RefreshSources() {
	a.srcMu.Lock()
	a.srcCache = nil
	a.srcMu.Unlock()
}

func (a *App) emit(name string, done, total int64) {
	a.emitEvent("progress", name, done, total)
}

type progress struct {
	a           *App
	name        string
	done, total int64
	last        time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) > 200*time.Millisecond {
		p.a.emit(p.name, p.done, p.total)
		p.last = time.Now()
	}
	return len(b), nil
}

func (a *App) download(name, rawURL string) (string, error) {
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) DevPanel")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", "devpanel-*.download")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	p := &progress{a: a, name: name, total: resp.ContentLength}
	if _, err := io.Copy(tmp, io.TeeReader(resp.Body, p)); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	a.emit(name, p.done, p.done)
	return tmp.Name(), nil
}

// archiveKind sniffs the file's magic bytes: zip, xz, gz, 7z or "".
func archiveKind(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 6)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	switch {
	case bytes.HasPrefix(b, []byte("PK\x03\x04")):
		return "zip"
	case bytes.HasPrefix(b, []byte("MZ")):
		return "exe"
	case bytes.HasPrefix(b, []byte{0xFD, '7', 'z', 'X', 'Z', 0}):
		return "xz"
	case bytes.HasPrefix(b, []byte{0x1F, 0x8B}):
		return "gz"
	case bytes.HasPrefix(b, []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}):
		return "7z"
	}
	return ""
}

// extractAny handles .zip natively and .tar.xz / .tar.gz / .7z with Laragon's bundled 7-Zip.
func (a *App) extractAny(file, dest string) error {
	kind := archiveKind(file)
	if kind == "zip" {
		return extract(file, dest)
	}
	if kind == "" {
		return errors.New("unknown archive format (supported: .zip, .tar.xz, .tar.gz, .7z)")
	}
	// Windows' own tar.exe (libarchive) reads .tar.xz, .tar.gz and .7z: no dependency on Laragon's 7-Zip.
	stage, err := os.MkdirTemp("", "devpanel-x-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	files := filepath.Join(stage, "files")
	if err := tarExtract(file, files); err != nil {
		return err
	}
	return flattenInto(files, dest)
}

// flattenInto moves dir to dest; if dir holds exactly one folder, that folder's contents become dest
// (same rule as zip extraction: loose top-level files are dropped then).
// isDocFile: loose files that may be dropped when unwrapping a single top folder (Apache Lounge's
// "ReadMe.txt" and "-- Win64 VS18 --"). Anything else (mod_fcgid.so, an .exe) means: keep the archive whole.
func isDocFile(name string) bool {
	n := strings.ToLower(name)
	ext := filepath.Ext(n)
	return ext == "" || ext == ".txt" || ext == ".md" || ext == ".html" || strings.HasPrefix(n, "readme") ||
		strings.HasPrefix(n, "license") || strings.HasPrefix(n, "changes") || strings.HasPrefix(n, "notice") ||
		strings.HasPrefix(n, "--")
}

func flattenInto(dir, dest string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var dirs []string
	docsOnly := true
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		} else if !isDocFile(e.Name()) {
			docsOnly = false
		}
	}
	if len(dirs) == 1 && docsOnly {
		dir = filepath.Join(dir, dirs[0])
	}
	if err := os.Rename(dir, dest); err == nil {
		return nil
	}
	// Rename fails across drives (temp on another disk): copy instead.
	return os.CopyFS(dest, os.DirFS(dir))
}

// extract unzips into dest. If the archive has exactly one top-level folder
// (nginx-1.27.4/, Apache24/, pgsql/ ...) its contents become dest and loose top-level files are dropped.
func extract(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("not a zip archive (only .zip is supported): %w", err)
	}
	defer r.Close()
	tops := map[string]bool{}
	docsOnly := true // loose top-level files are just ReadMe/License: safe to drop
	for _, f := range r.File {
		first, _, nested := strings.Cut(strings.ReplaceAll(f.Name, `\`, "/"), "/")
		if nested {
			tops[first] = true
		} else if !isDocFile(first) {
			docsOnly = false
		}
	}
	strip := ""
	if len(tops) == 1 && docsOnly {
		for t := range tops {
			strip = t + "/"
		}
	}
	for _, f := range r.File {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if strip != "" {
			if !strings.HasPrefix(name, strip) {
				continue
			}
			name = strings.TrimPrefix(name, strip)
		}
		if name == "" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := writeZipFile(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := os.Create(target)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, rc)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

func (a *App) Install(name string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pkgs := a.Packages().Packages
	var p *Package
	for i := range pkgs {
		if pkgs[i].Name == name {
			p = &pkgs[i]
		}
	}
	if p == nil {
		return "", fmt.Errorf("no package %q", name)
	}
	if p.Installed {
		return "", fmt.Errorf("%s already installed", name)
	}
	zipPath, err := a.download(name, p.URL)
	if err != nil {
		return "", err
	}
	defer os.Remove(zipPath)
	if p.SHA256 != "" {
		if sum, err := fileSHA256(zipPath); err != nil || !strings.EqualFold(sum, p.SHA256) {
			return "", fmt.Errorf("checksum mismatch for %s: download is corrupt or was tampered with; nothing installed", name)
		}
	}

	dest := a.path(filepath.Join(p.Group, p.Folder))
	partial := dest + ".partial"
	os.RemoveAll(partial)
	if archiveKind(zipPath) == "exe" { // single-binary download (e.g. meilisearch.exe)
		os.MkdirAll(partial, 0755)
		u, _ := url.Parse(p.URL)
		err = os.Rename(zipPath, filepath.Join(partial, path.Base(u.Path)))
	} else {
		err = a.extractAny(zipPath, partial)
	}
	if err != nil {
		os.RemoveAll(partial)
		return "", err
	}
	if err := os.Rename(partial, dest); err != nil {
		os.RemoveAll(partial)
		return "", err
	}
	if err := a.postInstall(p.Group, dest); err != nil {
		return "", fmt.Errorf("installed to %s but setup failed: %w", dest, err)
	}
	a.notify("Installed "+name, dest)
	return "installed to " + dest, nil
}

// postInstall does the minimum each package type needs to actually run.
func (a *App) postInstall(group, dest string) error {
	switch group {
	case `bin\php`:
		if err := setupPHPIni(dest); err != nil {
			return err
		}
		if !exists(a.path(`etc\apache2\fcgid.conf`)) { // first PHP of a standalone install becomes Apache's PHP
			return a.writeFcgidConf(filepath.Base(dest))
		}
		return nil
	case `bin\apache`:
		// Reuse the current httpd.conf (Laragon includes, fcgid, SSL) with the new ServerRoot.
		cur := a.svcDir("apache")
		if b, err := os.ReadFile(filepath.Join(cur, `conf\httpd.conf`)); err == nil && cur != dest {
			conf := strings.ReplaceAll(string(b), filepath.ToSlash(cur), filepath.ToSlash(dest))
			if err := os.WriteFile(filepath.Join(dest, `conf\httpd.conf`), []byte(conf), 0644); err != nil {
				return err
			}
		}
		// Standalone (nothing to copy from): DevPanel writes its own httpd.conf and support files.
		return a.ensureApacheConfig(dest)
	case `etc\apps`:
		return a.ensurePhpMyAdminAlias()
	case `bin\mysql`:
		data := filepath.ToSlash(a.path(`data\` + filepath.Base(dest)))
		ini := fmt.Sprintf("[client]\nport=%[2]d\n\n[mysqld]\ndatadir=\"%[1]s\"\nport=%[2]d\nmax_allowed_packet=512M\nsecure-file-priv=\"\"\nexplicit_defaults_for_timestamp=1\n", data, port("mysql"))
		if err := os.WriteFile(filepath.Join(dest, "my.ini"), []byte(ini), 0644); err != nil {
			return err
		}
		if _, err := os.Stat(data); err == nil {
			return nil
		}
		// MariaDB has its own installer tool; MySQL initializes with an empty root password (Laragon's default).
		for _, tool := range []string{`bin\mariadb-install-db.exe`, `bin\mysql_install_db.exe`} {
			if exists(filepath.Join(dest, tool)) {
				return run(filepath.Join(dest, tool), "--datadir="+data, "--port="+strconv.Itoa(port("mysql")))
			}
		}
		return run(filepath.Join(dest, `bin\mysqld.exe`), "--defaults-file="+filepath.Join(dest, "my.ini"), "--initialize-insecure")
	case `bin\postgresql`:
		data := a.pgDataFor(filepath.Base(dest))
		if _, err := os.Stat(data); err == nil {
			return nil
		}
		return run(filepath.Join(dest, `bin\initdb.exe`), "-D", data, "-U", "postgres", "-A", "trust", "-E", "UTF8")
	}
	return nil
}

// Extensions enabled in a fresh php.ini (Laravel + PostgreSQL/MySQL basics).
var phpExts = []string{"curl", "fileinfo", "gd", "intl", "mbstring", "openssl", "pdo_mysql", "pdo_pgsql", "pdo_sqlite", "pgsql", "sodium", "zip"}

func setupPHPIni(dir string) error {
	ini := filepath.Join(dir, "php.ini")
	if _, err := os.Stat(ini); err == nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "php.ini-development"))
	if err != nil {
		return errors.New("php.ini-development not found")
	}
	// Absolute, like Laragon: php-cgi may run with another working dir (Nginx, PHP per site).
	s := strings.Replace(string(b), `;extension_dir = "ext"`, `extension_dir = "`+filepath.ToSlash(filepath.Join(dir, "ext"))+`"`, 1)
	for _, e := range phpExts {
		s = strings.Replace(s, "\n;extension="+e, "\nextension="+e, 1)
	}
	return os.WriteFile(ini, []byte(s), 0644)
}
