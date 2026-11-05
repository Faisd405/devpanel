package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// allHosts returns every hostname (main + aliases) of the sites.
func allHosts(list []Site) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Host)
		out = append(out, s.Aliases...)
	}
	return out
}

// ---- Per-site options (#18 aliases, #19 custom root) ----

// SetSiteOptions sets a custom document root ("" = auto, "." = folder itself, or a subfolder) and aliases.
func (a *App) SetSiteOptions(folder, root string, aliases []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir := filepath.Join(a.www(), folder)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || strings.ContainsAny(folder, `\/`) {
		return fmt.Errorf("no folder %q in www", folder)
	}
	root = strings.Trim(strings.TrimSpace(filepath.ToSlash(root)), "/")
	if root != "" && root != "." {
		if strings.Contains(root, "..") || filepath.IsAbs(root) {
			return errors.New("document root must be a subfolder of the project")
		}
		if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(root))); err != nil || !st.IsDir() {
			return fmt.Errorf("folder %s\\%s does not exist", folder, root)
		}
	}
	var clean []string
	for _, al := range aliases {
		if al = a.normHost(al); al != "" && !slices.Contains(clean, al) {
			clean = append(clean, al)
		}
	}
	prevRoot, prevAl := a.cfg.SiteRoots[folder], a.cfg.SiteAliases[folder]
	if a.cfg.SiteRoots == nil {
		a.cfg.SiteRoots = map[string]string{}
	}
	if a.cfg.SiteAliases == nil {
		a.cfg.SiteAliases = map[string][]string{}
	}
	set := func(r string, al []string) {
		if r == "" {
			delete(a.cfg.SiteRoots, folder)
		} else {
			a.cfg.SiteRoots[folder] = r
		}
		if len(al) == 0 {
			delete(a.cfg.SiteAliases, folder)
		} else {
			a.cfg.SiteAliases[folder] = al
		}
	}
	set(root, clean)
	if _, err := a.sites(); err != nil {
		set(prevRoot, prevAl)
		return err
	}
	return a.saveConfig()
}

// ---- Site info (#16) ----

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// detectFramework guesses the project type from marker files.
func detectFramework(dir string) string {
	has := func(rel string) bool { return exists(filepath.Join(dir, rel)) }
	switch {
	case has("artisan") && has(`modules\system`):
		return "Winter/October CMS"
	case has("artisan"):
		return "Laravel"
	case has("spark"):
		return "CodeIgniter 4"
	case has(`system\core\CodeIgniter.php`):
		return "CodeIgniter 3"
	case has("wp-config.php") || has("wp-login.php"):
		return "WordPress"
	case has("symfony.lock"):
		return "Symfony"
	case has("next.config.js") || has("next.config.mjs") || has("next.config.ts"):
		return "Next.js"
	case has("nuxt.config.ts") || has("nuxt.config.js"):
		return "Nuxt"
	case has("package.json") && !has("composer.json"):
		return "Node.js"
	case has("composer.json"):
		return "PHP"
	case has("index.html"):
		return "Static"
	}
	return ""
}

var appURLRe = regexp.MustCompile(`(?m)^APP_URL\s*=\s*"?([^"\r\n]*)"?`)

func readAppURL(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return ""
	}
	if m := appURLRe.FindSubmatch(b); m != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func urlMatches(appURL, host string) bool { return urlHost(appURL) == host }

func (a *App) enrich(list []Site) {
	for i := range list {
		dir := filepath.Join(a.www(), list[i].Folder)
		list[i].Framework = detectFramework(dir)
		list[i].AppURL = readAppURL(dir)
		// Only flag APP_URLs pointing at a *local* .test name that isn't this site's (e.g. the old app_x.test).
		// localhost and external URLs (staging/production) are left alone on purpose.
		list[i].URLOK = list[i].AppURL == "" || urlMatches(list[i].AppURL, list[i].Host) || !strings.HasSuffix(urlHost(list[i].AppURL), "."+a.cfg.TLD)
	}
}

func urlHost(appURL string) string {
	u := strings.ToLower(appURL)
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	h, _, _ := strings.Cut(u, "/")
	h, _, _ = strings.Cut(h, ":")
	return h
}

// setEnv sets KEY=value in .env content, replacing an active or commented (# KEY=) line, else appending.
func setEnv(env, key, value string) string {
	re := regexp.MustCompile(`(?m)^[ \t]*#?[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=.*?(\r?)$`)
	if loc := re.FindStringSubmatchIndex(env); loc != nil {
		cr := env[loc[2]:loc[3]]
		return env[:loc[0]] + key + "=" + value + cr + env[loc[1]:]
	}
	if env != "" && !strings.HasSuffix(env, "\n") {
		env += "\n"
	}
	return env + key + "=" + value + "\n"
}

// FixAppURL sets APP_URL in the project's .env to this site's URL (https if the CA is trusted).
func (a *App) FixAppURL(folder string) (string, error) {
	list, _ := a.sites()
	i := slices.IndexFunc(list, func(s Site) bool { return s.Folder == folder })
	if i < 0 {
		return "", fmt.Errorf("no site %q", folder)
	}
	f := filepath.Join(a.www(), folder, ".env")
	b, err := os.ReadFile(f)
	if err != nil {
		return "", errors.New("project has no .env")
	}
	scheme := "http"
	if a.SSL().Trusted {
		scheme = "https"
	}
	u := scheme + "://" + list[i].Host
	if err := os.WriteFile(f, []byte(setEnv(string(b), "APP_URL", u)), 0644); err != nil {
		return "", err
	}
	return "APP_URL=" + u + " (run php artisan config:clear if config is cached)", nil
}

func (a *App) siteDir(folder string) (string, error) {
	d := filepath.Join(a.www(), folder)
	if strings.ContainsAny(folder, `\/`) || folder == ".." || !exists(d) {
		return "", fmt.Errorf("no folder %q in www", folder)
	}
	return d, nil
}

// OpenSite opens a project folder in VS Code, Explorer or a terminal with the selected environment.
func (a *App) OpenSite(folder, with string) error {
	d, err := a.siteDir(folder)
	if err != nil {
		return err
	}
	switch with {
	case "code":
		for _, c := range []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), `Programs\Microsoft VS Code\Code.exe`),
			filepath.Join(os.Getenv("ProgramFiles"), `Microsoft VS Code\Code.exe`),
		} {
			if exists(c) {
				return exec.Command(c, d).Start()
			}
		}
		return errors.New("VS Code not found")
	case "explorer":
		return exec.Command("explorer", d).Start()
	case "terminal":
		return a.openConsole(d)
	}
	return fmt.Errorf("unknown target %q", with)
}

// ---- hosts cleanup (#17) ----

func hostsFile() string { return filepath.Join(os.Getenv("SystemRoot"), `System32\drivers\etc\hosts`) }

// staleHostLines returns the indexes of lines added by Laragon/DevPanel for hosts no site uses anymore.
// Lines without those markers (Docker, manual entries) are never touched.
func staleHostLines(lines []string, want []string) []int {
	keep := map[string]bool{}
	for _, h := range want {
		keep[strings.ToLower(h)] = true
	}
	var out []int
	for i, l := range lines {
		body, comment, ok := strings.Cut(l, "#")
		if !ok || !(strings.Contains(comment, "laragon magic") || strings.Contains(comment, "devpanel")) {
			continue
		}
		f := strings.Fields(body)
		if len(f) < 2 {
			continue
		}
		used := false
		for _, h := range f[1:] {
			used = used || keep[strings.ToLower(h)]
		}
		if !used {
			out = append(out, i)
		}
	}
	return out
}

func (a *App) staleHosts() (lines []string, stale []int, err error) {
	b, err := os.ReadFile(hostsFile())
	if err != nil {
		return nil, nil, err
	}
	list, err := a.sites()
	if err != nil {
		return nil, nil, err
	}
	lines = strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	return lines, staleHostLines(lines, allHosts(list)), nil
}

// StaleHosts previews which hosts lines CleanHosts would remove.
func (a *App) StaleHosts() ([]string, error) {
	lines, idx, err := a.staleHosts()
	out := []string{}
	for _, i := range idx {
		out = append(out, strings.TrimSpace(lines[i]))
	}
	return out, err
}

// CleanHosts removes stale Laragon/DevPanel lines from hosts (backup first, UAC prompt).
func (a *App) CleanHosts() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	lines, idx, err := a.staleHosts()
	if err != nil {
		return "", err
	}
	if len(idx) == 0 {
		return "hosts file is already clean", nil
	}
	orig, _ := os.ReadFile(hostsFile())
	backup := filepath.Join(filepath.Dir(a.cfgPath), "hosts-backup-"+time.Now().Format("20060102-150405")+".txt")
	if err := os.WriteFile(backup, orig, 0644); err != nil {
		return "", fmt.Errorf("could not back up hosts, nothing changed: %w", err)
	}
	var kept []string
	for i, l := range lines {
		if !slices.Contains(idx, i) {
			kept = append(kept, l)
		}
	}
	tmp := filepath.Join(os.TempDir(), "devpanel-hosts-new.txt")
	if err := os.WriteFile(tmp, []byte(strings.Join(kept, "\r\n")), 0644); err != nil {
		return "", err
	}
	if err := a.elevated(fmt.Sprintf("@copy /Y \"%s\" \"%s\" >nul\r\n", tmp, hostsFile())); err != nil {
		return "", err
	}
	return fmt.Sprintf("removed %d stale lines (backup: %s)", len(idx), backup), nil
}

// elevated runs a one-off batch script as admin (UAC prompt) and waits for it.
func (a *App) elevated(script string) error {
	bat := filepath.Join(os.TempDir(), "devpanel-elevated.bat")
	if err := os.WriteFile(bat, []byte(script), 0644); err != nil {
		return err
	}
	ps := fmt.Sprintf("Start-Process -FilePath '%s' -Verb RunAs -Wait -WindowStyle Hidden", bat)
	if err := hidden(exec.Command("powershell", "-NoProfile", "-Command", ps)).Run(); err != nil {
		return fmt.Errorf("cancelled or failed (admin needed): %w", err)
	}
	return nil
}

// ---- Share via ngrok (#15) ----

type Share struct {
	Host string `json:"host"`
	URL  string `json:"url"`
}

func ngrokConfigured() bool {
	return exists(filepath.Join(os.Getenv("LOCALAPPDATA"), `ngrok\ngrok.yml`)) || exists(filepath.Join(os.Getenv("USERPROFILE"), `.ngrok2\ngrok.yml`))
}

func (a *App) NgrokReady() bool { return ngrokConfigured() }

// SetNgrokToken saves the ngrok authtoken (from dashboard.ngrok.com) in ngrok's own config.
func (a *App) SetNgrokToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\"'") {
		return errors.New("paste the authtoken from dashboard.ngrok.com")
	}
	if a.ngrokExe() == "" {
		return errors.New("ngrok is not installed: install it from Install (ngrok)")
	}
	out, err := output(a.ngrokExe(), "config", "add-authtoken", token)
	if err != nil {
		return fmt.Errorf("%v %s", err, out)
	}
	return nil
}

// ShareSite exposes one site publicly through ngrok; only one share at a time.
func (a *App) ShareSite(folder string) (Share, error) {
	if !ngrokConfigured() {
		return Share{}, errors.New("set your ngrok authtoken first")
	}
	if !a.running("apache") && !a.running("nginx") {
		return Share{}, errors.New("start Apache or Nginx first")
	}
	list, _ := a.sites()
	i := slices.IndexFunc(list, func(s Site) bool { return s.Folder == folder })
	if i < 0 {
		return Share{}, fmt.Errorf("no site %q", folder)
	}
	a.StopShare()
	host := list[i].Host
	if a.ngrokExe() == "" {
		return Share{}, errors.New("ngrok is not installed: install it from Install (ngrok)")
	}
	cmd := hidden(exec.Command(a.ngrokExe(), "http", "--host-header="+host, strconv.Itoa(port("apache"))))
	if err := cmd.Start(); err != nil {
		return Share{}, err
	}
	a.shareMu.Lock()
	a.share, a.shareCmd = Share{Host: host}, cmd
	a.shareMu.Unlock()
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	// The agent's local API lists the public URL once the tunnel is up.
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if u := ngrokURL(); u != "" {
			a.shareMu.Lock()
			a.share.URL = u
			s := a.share
			a.shareMu.Unlock()
			return s, nil
		}
		select {
		case <-done:
			a.StopShare()
			return Share{}, errors.New("ngrok exited (check the authtoken / free-plan limits)")
		default:
		}
	}
	a.StopShare()
	return Share{}, errors.New("ngrok did not report a public URL (check the authtoken / free-plan limits)")
}

func ngrokURL() string {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://127.0.0.1:4040/api/tunnels")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var r struct {
		Tunnels []struct {
			PublicURL string `json:"public_url"`
		} `json:"tunnels"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return ""
	}
	for _, t := range r.Tunnels {
		if strings.HasPrefix(t.PublicURL, "https://") {
			return t.PublicURL
		}
	}
	return ""
}

func (a *App) CurrentShare() Share {
	a.shareMu.Lock()
	defer a.shareMu.Unlock()
	return a.share
}

func (a *App) StopShare() {
	a.shareMu.Lock()
	cmd := a.shareCmd
	a.share, a.shareCmd = Share{}, nil
	a.shareMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		hidden(exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))).Run()
	}
}
