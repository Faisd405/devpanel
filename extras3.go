package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func httpGet(url string, limit int64) ([]byte, error) {
	c := http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) DevPanel")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// ---- Xdebug (#10) ----

var phpFolderRe = regexp.MustCompile(`(?i)^php-(\d+)\.(\d+)\.\d+(?:-\d+)?-(nts-)?Win32-(vs\d+)-x64$`)

// xdebugDLLName is the xdebug.org file pattern for a PHP folder: php_xdebug-<v>-8.4-nts-vs17-x86_64.dll.
func xdebugPattern(phpFolder string) (*regexp.Regexp, error) {
	m := phpFolderRe.FindStringSubmatch(phpFolder)
	if m == nil {
		return nil, fmt.Errorf("can't tell PHP build from folder %q", phpFolder)
	}
	ts := "ts"
	if m[3] != "" {
		ts = "nts"
	}
	return regexp.MustCompile(`php_xdebug-(\d+\.\d+\.\d+)-` + m[1] + `\.` + m[2] + `-` + ts + `-` + strings.ToLower(m[4]) + `-x86_64\.dll`), nil
}

// newestStable picks the highest stable version among matches (alpha/beta/RC never match the pattern).
func newestStable(page string, re *regexp.Regexp) (name string) {
	best := ""
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		if best == "" || verLess(best, m[1]) {
			best, name = m[1], m[0]
		}
	}
	return name
}

// verLess compares dotted numeric versions.
func verLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

type XdebugInfo struct {
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
}

func (a *App) Xdebug() XdebugInfo {
	d := a.svcDir("php")
	ini, _ := os.ReadFile(filepath.Join(d, "php.ini"))
	return XdebugInfo{exists(filepath.Join(d, `ext\php_xdebug.dll`)), extEnabled(string(ini), "xdebug"), iniGet(string(ini), "xdebug.mode")}
}

// SetXdebug downloads the matching DLL if needed and enables/disables it (step debugging on trigger, port 9003).
func (a *App) SetXdebug(on bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.svcDir("php")
	dll := filepath.Join(d, `ext\php_xdebug.dll`)
	msg := ""
	if on && !exists(dll) {
		re, err := xdebugPattern(filepath.Base(d))
		if err != nil {
			return "", err
		}
		page, err := httpGet("https://xdebug.org/download", 4<<20)
		if err != nil {
			return "", err
		}
		name := newestStable(string(page), re)
		if name == "" {
			return "", errors.New("no stable Xdebug build for this PHP on xdebug.org yet")
		}
		b, err := httpGet("https://xdebug.org/files/"+name, 64<<20)
		if err != nil {
			return "", err
		}
		if len(b) < 2 || b[0] != 'M' || b[1] != 'Z' {
			return "", errors.New("downloaded file is not a DLL")
		}
		if err := os.WriteFile(dll, b, 0644); err != nil {
			return "", err
		}
		msg = "installed " + name + "; "
	}
	f := filepath.Join(d, "php.ini")
	b, err := os.ReadFile(f)
	if err != nil {
		return "", err
	}
	ini := setExt(string(b), "xdebug", on)
	if on {
		for k, v := range map[string]string{"xdebug.mode": "debug,develop", "xdebug.start_with_request": "trigger", "xdebug.client_port": "9003", "xdebug.client_host": "127.0.0.1"} {
			if iniGet(ini, k) == "" {
				ini = iniSet(ini, k, v)
			}
		}
	}
	if err := os.WriteFile(f, []byte(ini), 0644); err != nil {
		return "", err
	}
	state := "disabled"
	if on {
		state = "enabled (start a debug session with XDEBUG_TRIGGER / browser extension, IDE on port 9003)"
	}
	return msg + "Xdebug " + state, a.restartWeb()
}

// ---- Update check (#21) ----

type Update struct {
	Tool      string `json:"tool"`
	Installed string `json:"installed"`
	Latest    string `json:"latest"`
	Package   string `json:"package"` // name to pass to Install
}

// installedVersions maps "8.4" -> "8.4.5" (highest patch installed per minor).
func installedPHP(folders []string) map[string]string {
	out := map[string]string{}
	re := regexp.MustCompile(`^php-(\d+\.\d+)\.(\d+)`)
	for _, f := range folders {
		if m := re.FindStringSubmatch(f); m != nil {
			v := m[1] + "." + m[2]
			if cur, ok := out[m[1]]; !ok || verLess(cur, v) {
				out[m[1]] = v
			}
		}
	}
	return out
}

func nodeVersion(exe string) string {
	out, err := output(exe, "--version")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "v")
}

// CheckUpdates compares installed PHP (per minor) and Node (per major) with the official feeds.
// Results become installable packages (source "Updates") with SHA-256 verification.
func (a *App) CheckUpdates() ([]Update, error) {
	var ups []Update
	var pkgs []Package
	var errs []error

	if b, err := httpGet("https://windows.php.net/downloads/releases/releases.json", 4<<20); err != nil {
		errs = append(errs, err)
	} else {
		var rel map[string]map[string]json.RawMessage
		if err := json.Unmarshal(b, &rel); err != nil {
			errs = append(errs, err)
		}
		for minor, cur := range installedPHP(a.versions("php")) {
			r, ok := rel[minor]
			if !ok {
				continue
			}
			var ver string
			json.Unmarshal(r["version"], &ver)
			latest := strings.SplitN(ver, "-", 2)[0]
			if !verLess(cur, latest) {
				continue
			}
			for _, build := range []string{"nts-vs17-x64", "nts-vs16-x64"} {
				var b struct {
					Zip struct{ Path, Sha256 string } `json:"zip"`
				}
				if json.Unmarshal(r[build], &b) == nil && b.Zip.Path != "" {
					name := "PHP-" + latest
					ups = append(ups, Update{"PHP " + minor, cur, latest, name})
					p := a.pkg(name, "https://windows.php.net/downloads/releases/"+b.Zip.Path, "php", "Updates")
					p.SHA256 = b.Zip.Sha256
					pkgs = append(pkgs, p)
					break
				}
			}
		}
	}

	if d := a.svcDir("nodejs"); d != "" {
		if cur := nodeVersion(filepath.Join(d, "node.exe")); cur != "" {
			major := strings.Split(cur, ".")[0]
			if b, err := httpGet("https://nodejs.org/dist/index.json", 8<<20); err != nil {
				errs = append(errs, err)
			} else {
				var idx []struct {
					Version string   `json:"version"`
					Files   []string `json:"files"`
				}
				json.Unmarshal(b, &idx)
				for _, r := range idx { // newest first
					v := strings.TrimPrefix(r.Version, "v")
					if strings.Split(v, ".")[0] != major || strings.Contains(v, "-") {
						continue
					}
					if verLess(cur, v) {
						name := "node-" + v
						url := "https://nodejs.org/dist/v" + v + "/node-v" + v + "-win-x64.zip"
						p := a.pkg(name, url, "nodejs", "Updates")
						if sums, err := httpGet("https://nodejs.org/dist/v"+v+"/SHASUMS256.txt", 1<<20); err == nil {
							p.SHA256 = shaFor(string(sums), "node-v"+v+"-win-x64.zip")
						}
						ups = append(ups, Update{"Node.js " + major, cur, v, name})
						pkgs = append(pkgs, p)
					}
					break
				}
			}
		}
	}
	a.srcMu.Lock()
	a.updates = pkgs
	a.srcMu.Unlock()
	sort.Slice(ups, func(i, j int) bool { return ups[i].Tool < ups[j].Tool })
	return ups, errors.Join(errs...)
}

func shaFor(sums, file string) string {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == file {
			return f[0]
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- Watchdog (#14) + notifications (#25) ----

// notify shows a Windows toast (via PowerShell's WinRT bridge). Text goes through env vars, never the script.
func (a *App) notify(title, body string) {
	if a.cfg.NoNotify {
		return
	}
	script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$x = $t.GetElementsByTagName('text')
$x.Item(0).AppendChild($t.CreateTextNode($env:DP_TITLE)) > $null
$x.Item(1).AppendChild($t.CreateTextNode($env:DP_BODY)) > $null
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show([Windows.UI.Notifications.ToastNotification]::new($t))`
	cmd := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script))
	cmd.Env = append(os.Environ(), "DP_TITLE=DevPanel · "+title, "DP_BODY="+body)
	go cmd.Run()
}

func (a *App) Notify(title, body string) { a.notify(title, body) }

// watchServices restarts services DevPanel started that died on their own (max 3 tries per 10 minutes).
func (a *App) watchServices() {
	tries := map[string][]time.Time{}
	for range time.Tick(15 * time.Second) {
		if !a.cfg.Watchdog || !a.mu.TryLock() {
			continue
		}
		for name := range a.wanted {
			if a.running(name) {
				continue
			}
			recent := tries[name][:0]
			for _, t := range tries[name] {
				if time.Since(t) < 10*time.Minute {
					recent = append(recent, t)
				}
			}
			tries[name] = recent
			if len(recent) >= 3 {
				delete(a.wanted, name)
				a.notify(name+" keeps crashing", "Gave up after 3 restarts. Check its log in Services.")
				a.emitEvent("log", name+": crashed repeatedly, watchdog gave up", true)
				continue
			}
			tries[name] = append(tries[name], time.Now())
			if err := a.start(name); err != nil {
				a.emitEvent("log", "watchdog: restart "+name+": "+err.Error(), true)
			} else {
				a.notify(name+" restarted", name+" had stopped unexpectedly and was restarted.")
				a.emitEvent("log", "watchdog: "+name+" had stopped, restarted", false)
			}
		}
		a.mu.Unlock()
	}
}

// ---- Composer / Git (#12) ----

type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (a *App) ToolVersions() []ToolInfo {
	php := filepath.Join(a.svcDir("php"), "php.exe")
	// Old composer.phar on new PHP prints "Deprecated: ..." first; take the line that has the version.
	versionLine := func(want string) func(string, error) string {
		return func(s string, err error) string {
			if err != nil {
				return "not available"
			}
			for _, l := range strings.Split(s, "\n") {
				if l = strings.TrimSpace(regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(l, "")); strings.HasPrefix(strings.ToLower(l), want) {
					return l
				}
			}
			return "unknown"
		}
	}
	return []ToolInfo{
		{"Composer", versionLine("composer version")(output(php, a.path(`bin\composer\composer.phar`), "--version", "--no-ansi"))},
		{"Git", versionLine("git version")(output(a.path(`bin\git\cmd\git.exe`), "--version"))},
	}
}

func (a *App) UpdateComposer() (string, error) {
	if !exists(a.composerPhar()) { // standalone: first time = install it
		if err := a.ensureComposer(); err != nil {
			return "", err
		}
		return "Composer installed", nil
	}
	php := filepath.Join(a.svcDir("php"), "php.exe")
	out, err := output(php, a.path(`bin\composer\composer.phar`), "self-update", "--stable", "--no-ansi", "--no-interaction")
	if err != nil {
		return "", fmt.Errorf("%v %s", err, out)
	}
	return firstNonEmpty(out), nil
}

func firstNonEmpty(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return "done"
}

// ---- MySQL root password (#11) ----

// SetMySQLPassword changes the password of the account DevPanel logs in with (root) and remembers it.
func (a *App) SetMySQLPassword(pw, confirm string) (string, error) {
	if pw != confirm {
		return "", errors.New("passwords don't match")
	}
	if strings.ContainsAny(pw, "'\\\r\n") {
		return "", errors.New("password can't contain ' \\ or line breaks")
	}
	exe, err := a.dbBin("mysql", "mysql")
	if err != nil {
		return "", err
	}
	sql := fmt.Sprintf("ALTER USER CURRENT_USER() IDENTIFIED BY '%s'; FLUSH PRIVILEGES;", pw)
	if _, err := dbRun(nil, nil, exe, myArgs("-e", sql)...); err != nil {
		return "", err
	}
	a.mu.Lock()
	a.cfg.MySQLPassword = pw
	setMySQLPassword(pw)
	err = a.saveConfig()
	a.mu.Unlock()
	return "root password changed. Update DB_PASSWORD in your projects' .env files.", err
}

// ---- PostgreSQL major upgrade helpers (#23) ----

// DumpAll writes pg_dumpall of the running server (all databases + roles) to backup\postgresql.
func (a *App) DumpAll() (string, error) {
	exe, err := a.dbBin("postgresql", "pg_dumpall")
	if err != nil {
		return "", err
	}
	os.MkdirAll(a.backupDir("postgresql"), 0755)
	f := filepath.Join(a.backupDir("postgresql"), "ALL_"+filepath.Base(a.svcDir("postgresql"))+"_"+time.Now().Format("20060102-150405")+".sql")
	if _, err := dbRun(nil, nil, exe, pgArgs("-f", f)...); err != nil {
		os.Remove(f)
		return "", err
	}
	return "saved " + f, nil
}

// RestoreAll replays a pg_dumpall file into the running server. confirm must be "RESTORE ALL".
// "already exists" errors (e.g. role postgres) are expected and ignored by psql (no ON_ERROR_STOP).
func (a *App) RestoreAll(file, confirm string) (string, error) {
	if confirm != "RESTORE ALL" {
		return "", errors.New(`type RESTORE ALL to confirm; nothing restored`)
	}
	if !exists(file) {
		return "", errors.New("dump file not found")
	}
	exe, err := a.dbBin("postgresql", "psql")
	if err != nil {
		return "", err
	}
	if _, err := dbRun(nil, nil, exe, pgArgs("-d", "postgres", "-q", "-f", file)...); err != nil && !strings.Contains(err.Error(), "already exists") {
		return "", err
	}
	return "restored " + filepath.Base(file) + " into " + filepath.Base(a.svcDir("postgresql")), nil
}

// ---- Shortcuts (#28) ----

// CreateShortcuts adds DevPanel to the Start Menu and the Desktop.
func (a *App) CreateShortcuts() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	script := `$s = New-Object -ComObject WScript.Shell
foreach ($p in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
  $l = $s.CreateShortcut((Join-Path $p 'DevPanel.lnk'))
  $l.TargetPath = $env:DP_EXE; $l.WorkingDirectory = (Split-Path $env:DP_EXE); $l.IconLocation = $env:DP_EXE + ',0'; $l.Save()
}`
	cmd := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script))
	cmd.Env = append(os.Environ(), "DP_EXE="+exe)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v %s", err, out)
	}
	return "shortcuts created in Start Menu and on the Desktop", nil
}
