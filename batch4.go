package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ---- www folder (Laragon: "Change Document Root") ----

func (a *App) www() string {
	if a.cfg.WWW != "" {
		return a.cfg.WWW
	}
	return a.path("www")
}

func (a *App) WWW() string { return a.www() }

// SetWWW moves where projects live (the folder itself is not moved). "" = <root>\www.
func (a *App) SetWWW(dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir = strings.TrimSpace(strings.Trim(dir, `"`))
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return errors.New("use a full path, e.g. D:\\projects")
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return fmt.Errorf("folder %s does not exist", dir)
		}
		dir = filepath.Clean(dir)
	}
	prev := a.cfg.WWW
	a.cfg.WWW = dir
	if _, err := a.sites(); err != nil {
		a.cfg.WWW = prev
		return fmt.Errorf("projects in that folder clash: %w", err)
	}
	return a.saveConfig()
}

// wwwDirs: the main projects folder first, then the extra ones (e.g. XAMPP's htdocs next to Laragon's www).
func (a *App) wwwDirs() []string { return append([]string{a.www()}, a.cfg.ExtraWWW...) }

func (a *App) ExtraWWW() []string { return append([]string{}, a.cfg.ExtraWWW...) }

// AddWWW serves the projects of another folder too; folders are matched by name, so the main one wins on a clash.
func (a *App) AddWWW(dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir = strings.TrimSpace(strings.Trim(strings.TrimSpace(dir), `"`))
	if !filepath.IsAbs(dir) {
		return errors.New(`use a full path, e.g. C:\xampp\htdocs`)
	}
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fmt.Errorf("folder %s does not exist", dir)
	}
	if slices.ContainsFunc(a.wwwDirs(), func(p string) bool { return strings.EqualFold(filepath.Clean(p), dir) }) {
		return errors.New("already a projects folder")
	}
	a.cfg.ExtraWWW = append(a.cfg.ExtraWWW, dir)
	if _, err := a.sites(); err != nil {
		a.cfg.ExtraWWW = a.cfg.ExtraWWW[:len(a.cfg.ExtraWWW)-1]
		return fmt.Errorf("projects in that folder clash: %w", err)
	}
	return a.saveConfig()
}

func (a *App) RemoveWWW(dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.ExtraWWW = slices.DeleteFunc(a.cfg.ExtraWWW, func(p string) bool { return strings.EqualFold(p, dir) })
	return a.saveConfig()
}

// ---- PHP mail() -> Mailpit ----

// On Windows PHP's mail() talks SMTP directly (sendmail_path is ignored), so SMTP/smtp_port decide where it goes.
func (a *App) MailCatch() bool {
	b, _ := os.ReadFile(filepath.Join(a.svcDir("php"), "php.ini"))
	return iniGet(string(b), "SMTP") == "127.0.0.1" && iniGet(string(b), "smtp_port") == strconv.Itoa(port("mailpit_smtp"))
}

// SetMailCatch points mail() of every installed PHP at Mailpit (on) or back to PHP's default (off).
func (a *App) SetMailCatch(on bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	host, p := "localhost", "25"
	if on {
		host, p = "127.0.0.1", strconv.Itoa(port("mailpit_smtp"))
	}
	n := 0
	for _, v := range a.PHPVersions() {
		f := filepath.Join(a.phpDir(v), "php.ini")
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		ini := iniSet(iniSet(string(b), "SMTP", host), "smtp_port", p)
		if on && iniGet(ini, "sendmail_from") == "" {
			ini = iniSet(ini, "sendmail_from", "devpanel@localhost")
		}
		if err := os.WriteFile(f, []byte(ini), 0644); err != nil {
			return "", err
		}
		n++
	}
	msg := fmt.Sprintf("mail() of %d PHP version(s) now goes to %s:%s", n, host, p)
	if on && !a.running("mailpit") {
		msg += " (start Mailpit to receive it)"
	}
	return msg, a.restartWeb()
}

// ---- Memcached / MongoDB helpers ----

// findExe returns the first file named name under dir (some builds keep it in bin\).
func findExe(dir, name string) string {
	found := ""
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && found == "" && !d.IsDir() && strings.EqualFold(d.Name(), name) {
			found = p
		}
		return nil
	})
	return found
}

func (a *App) mongoData() string { return a.path(`data\mongodb-` + filepath.Base(a.svcDir("mongodb"))) }

// ---- Quick App templates ----

var composerTemplates = map[string]struct{ Package, Root string }{
	"laravel":     {"laravel/laravel", ""},
	"symfony":     {"symfony/skeleton", ""},
	"codeigniter": {"codeigniter4/appstarter", ""},
	"cakephp":     {"cakephp/app", "webroot"},
	"drupal":      {"drupal/recommended-project", "web"},
}

func (a *App) AppTemplates() []string {
	return []string{"laravel", "wordpress", "symfony", "codeigniter", "cakephp", "drupal", "blank"}
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return strings.NewReplacer("+", "x", "/", "y", "=", "").Replace(base64.StdEncoding.EncodeToString(b))[:n]
}

// wpConfig fills wp-config-sample.php: DB settings and fresh random salts.
func wpConfig(sample, db string, dbPort int, password string) string {
	r := strings.NewReplacer(
		"database_name_here", db,
		"username_here", "root",
		"password_here", strings.ReplaceAll(password, "'", `\'`),
		"'localhost'", "'127.0.0.1:"+strconv.Itoa(dbPort)+"'",
	)
	s := r.Replace(sample)
	for strings.Contains(s, "put your unique phrase here") {
		s = strings.Replace(s, "put your unique phrase here", randomString(64), 1)
	}
	return s
}

// installWordPress downloads the latest WordPress into dir and writes wp-config.php for MySQL.
func (a *App) installWordPress(dir, db string) error {
	a.emitEvent("quickapp", "downloading wordpress.org/latest.zip …")
	f, err := a.download("WordPress", "https://wordpress.org/latest.zip")
	if err != nil {
		return err
	}
	defer os.Remove(f)
	if err := a.extractAny(f, dir); err != nil {
		return err
	}
	sample, err := os.ReadFile(filepath.Join(dir, "wp-config-sample.php"))
	if err != nil {
		return errors.New("wp-config-sample.php missing in the download")
	}
	return os.WriteFile(filepath.Join(dir, "wp-config.php"), []byte(wpConfig(string(sample), db, port("mysql"), a.cfg.MySQLPassword)), 0644)
}

// ---- Rename / delete projects ----

func (a *App) procRunningIn(folder string) bool {
	for _, st := range a.Processes() {
		if st.Running && strings.EqualFold(st.Dir, folder) {
			return true
		}
	}
	return false
}

// moveSiteKeys carries per-site settings over to a new folder name ("" = forget them).
func (a *App) moveSiteKeys(from, to string) {
	for _, m := range []map[string]string{a.cfg.Hostnames, a.cfg.SiteRoots, a.cfg.SitePHP} {
		if v, ok := m[from]; ok && m != nil {
			delete(m, from)
			if to != "" {
				m[to] = v
			}
		}
	}
	if v, ok := a.cfg.SiteAliases[from]; ok {
		delete(a.cfg.SiteAliases, from)
		if to != "" {
			a.cfg.SiteAliases[to] = v
		}
	}
	if to == "" {
		a.cfg.Procs = slices.DeleteFunc(a.cfg.Procs, func(p Proc) bool { return strings.EqualFold(p.Dir, from) })
	} else {
		for i := range a.cfg.Procs {
			if strings.EqualFold(a.cfg.Procs[i].Dir, from) {
				a.cfg.Procs[i].Dir = to
			}
		}
	}
}

func (a *App) RenameProject(folder, newName string) (string, error) {
	if !folderRe.MatchString(newName) {
		return "", errors.New("new name: letters, digits, - and _ (max 63)")
	}
	from, err := a.siteDir(folder)
	if err != nil {
		return "", err
	}
	to := filepath.Join(filepath.Dir(from), newName)
	if d, _ := a.siteDir(newName); d != "" {
		to = d
	}
	if exists(to) {
		return "", fmt.Errorf("%s already exists", newName)
	}
	if a.procRunningIn(folder) {
		return "", errors.New("stop this project's processes first")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.Rename(from, to); err != nil {
		return "", fmt.Errorf("rename failed (is a file open in an editor or terminal?): %w", err)
	}
	a.moveSiteKeys(folder, newName)
	if err := a.saveConfig(); err != nil {
		return "", err
	}
	_, _, err = a.syncVhostFiles()
	if err == nil {
		err = a.restartWeb()
	}
	return "renamed to " + newName + " (database names and .env are unchanged)", err
}

// DeleteProject moves a project folder to the Recycle Bin (recoverable). confirm must repeat the folder name.
// Databases are not touched.
func (a *App) DeleteProject(folder, confirm string) (string, error) {
	dir, err := a.siteDir(folder)
	if err != nil {
		return "", err
	}
	if confirm != folder {
		return "", errors.New("confirmation does not match; nothing deleted")
	}
	if a.procRunningIn(folder) {
		return "", errors.New("stop this project's processes first")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := `Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory($env:DP_DIR, 'OnlyErrorDialogs', 'SendToRecycleBin')`
	cmd := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps))
	cmd.Env = append(os.Environ(), "DP_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil || exists(dir) {
		return "", fmt.Errorf("could not move to Recycle Bin: %v %s", err, strings.TrimSpace(string(out)))
	}
	a.moveSiteKeys(folder, "")
	if err := a.saveConfig(); err != nil {
		return "", err
	}
	if _, _, err := a.syncVhostFiles(); err != nil {
		return "", err
	}
	return folder + " moved to the Recycle Bin (its database, if any, was kept)", a.restartWeb()
}

// ---- Custom vhost templates (Laragon: usr\tpl) ----

func (a *App) apacheTplPath() string { return a.path(`etc\apache2\devpanel-vhost.tpl`) }
func (a *App) nginxTplPath() string  { return a.path(`etc\nginx\devpanel-server.tpl`) }

const apacheTplHelp = `# DevPanel Apache vhost template, used for every site. Placeholders:
# {{ROOT}} {{HOST}} {{ALIASES}} {{HTTP_PORT}} {{SSL_PORT}} {{CERT}} {{KEY}} {{PROXY}}
# Delete this file (Settings > Reset) to go back to the built-in template.
`

const apacheTplDefault = `define ROOT "{{ROOT}}"
define SITE "{{HOST}}"

<VirtualHost *:{{HTTP_PORT}}>
    DocumentRoot "${ROOT}"
    ServerName ${SITE}
    ServerAlias *.${SITE}{{ALIASES}}
    <Directory "${ROOT}">
        AllowOverride All
        Require all granted
    </Directory>{{PROXY}}
</VirtualHost>

<VirtualHost *:{{SSL_PORT}}>
    DocumentRoot "${ROOT}"
    ServerName ${SITE}
    ServerAlias *.${SITE}{{ALIASES}}
    <Directory "${ROOT}">
        AllowOverride All
        Require all granted
    </Directory>{{PROXY}}

    SSLEngine on
    SSLCertificateFile      {{CERT}}
    SSLCertificateKeyFile   {{KEY}}
</VirtualHost>
`

const nginxTplHelp = `    # DevPanel Nginx server template, used for every site. Placeholders:
    # {{NAMES}} {{ROOT}} {{HTTP_PORT}} {{SSL_PORT}} {{CERT}} {{KEY}} {{LOCATIONS}} {{FASTCGI_PARAMS}} {{UPSTREAM}}
    # Delete this file (Settings > Reset) to go back to the built-in template.
`

const nginxTplDefault = `    server {
        listen {{HTTP_PORT}};
        listen {{SSL_PORT}} ssl;
        server_name {{NAMES}};
        root "{{ROOT}}";
        index index.php index.html index.htm;
        ssl_certificate "{{CERT}}";
        ssl_certificate_key "{{KEY}}";
        {{LOCATIONS}}
        location ~ /\.(?!well-known) { deny all; }
    }
`

func fillTpl(tpl string, vals map[string]string) string {
	for k, v := range vals {
		tpl = strings.ReplaceAll(tpl, "{{"+k+"}}", v)
	}
	return tpl
}

// userTpl returns the user's template if they made one, else the default.
func userTpl(path, def string) string {
	if b, err := os.ReadFile(path); err == nil {
		return string(b)
	}
	return def
}

// EditVhostTemplate creates the template from the default if needed and opens it. kind: "apache" or "nginx".
func (a *App) EditVhostTemplate(kind string) error {
	f, def := a.apacheTplPath(), apacheTplHelp+apacheTplDefault
	if kind == "nginx" {
		f, def = a.nginxTplPath(), nginxTplHelp+nginxTplDefault
	} else if kind != "apache" {
		return fmt.Errorf("unknown template %q", kind)
	}
	if !exists(f) {
		os.MkdirAll(filepath.Dir(f), 0755)
		if err := os.WriteFile(f, []byte(def), 0644); err != nil {
			return err
		}
	}
	return a.openInEditor(f)
}

// ResetVhostTemplate removes a custom template so the built-in one is used again.
func (a *App) ResetVhostTemplate(kind string) error {
	f := a.apacheTplPath()
	if kind == "nginx" {
		f = a.nginxTplPath()
	}
	if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *App) CustomTemplates() map[string]bool {
	return map[string]bool{"apache": exists(a.apacheTplPath()), "nginx": exists(a.nginxTplPath())}
}

// ---- Laragon's Apache aliases (/phpmyadmin, /adminer, ...) for Nginx ----

var aliasRe = regexp.MustCompile(`(?m)^\s*Alias\s+(/\S+)\s+"([^"]+)"`)

// webAliases reads etc\apache2\alias\*.conf and keeps the ones whose folder exists.
func (a *App) webAliases() [][2]string {
	files, _ := filepath.Glob(a.path(`etc\apache2\alias\*.conf`))
	var out [][2]string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, m := range aliasRe.FindAllStringSubmatch(string(b), -1) {
			if st, err := os.Stat(m[2]); err == nil && st.IsDir() {
				out = append(out, [2]string{strings.TrimSuffix(m[1], "/"), strings.TrimSuffix(filepath.ToSlash(m[2]), "/")})
			}
		}
	}
	return out
}

// ---- Terminal: cmd or Cmder ----

// openConsole opens a terminal in dir with the selected versions on PATH.
func (a *App) openConsole(dir string) error {
	if err := a.writeEnvCmd(); err != nil {
		return err
	}
	if cm := a.path(`bin\cmder\Cmder.exe`); a.cfg.Terminal == "cmder" && exists(cm) {
		cmd := exec.Command(cm, "/START", dir)
		cmd.Env = a.procEnv() // Cmder's shells inherit it
		return cmd.Start()
	}
	return exec.Command("cmd", "/c", "start", "", "/D", dir, "cmd", "/k", a.envCmdPath()).Run()
}

func (a *App) SetTerminal(t string) error {
	if t != "cmd" && t != "cmder" {
		return errors.New("terminal must be cmd or cmder")
	}
	if t == "cmder" && !exists(a.path(`bin\cmder\Cmder.exe`)) {
		return errors.New(`Cmder not found at bin\cmder\Cmder.exe`)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Terminal = t
	return a.saveConfig()
}

func (a *App) Terminal() string {
	if a.cfg.Terminal == "" {
		return "cmd"
	}
	return a.cfg.Terminal
}
