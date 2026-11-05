package main

// FlyEnv-inspired additions: Caddy/FrankenPHP web servers, reverse proxy per site, more services
// (Meilisearch, Ollama, Elasticsearch, RabbitMQ) and runtimes (Java, .NET, Ruby, Bun, Deno, Erlang).

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// webServers share ports 80/443; only one runs at a time (first ticked wins in Start All).
var webServers = []string{"apache", "nginx", "caddy", "frankenphp"}

func isWebServer(name string) bool { return slices.Contains(webServers, name) }

// startTimeout: JVM/Erlang services need longer than the 20 s default to open their port.
func startTimeout(name string) time.Duration {
	switch name {
	case "elasticsearch":
		return 120 * time.Second
	case "rabbitmq":
		return 60 * time.Second
	}
	return 20 * time.Second
}

// procDirs are the folders a service's processes run from (RabbitMQ runs on the Erlang install).
func (a *App) procDirs(name string) []string {
	dirs := []string{a.svcDir(name)}
	if name == "rabbitmq" {
		dirs = append(dirs, a.svcDir("erlang"))
	}
	return slices.DeleteFunc(dirs, func(d string) bool { return d == "" })
}

func (a *App) ownedIn(paths []string, name string) bool {
	for _, d := range a.procDirs(name) {
		if anyUnder(paths, d) {
			return true
		}
	}
	return false
}

func (a *App) killOwned(name string) {
	for _, d := range a.procDirs(name) {
		killUnder(d)
	}
}

func (a *App) dataDir(name string) string {
	d := a.path(`data\` + name)
	os.MkdirAll(d, 0755)
	return d
}

// spawnEnv starts a long-running process with extra environment variables.
func spawnEnv(env []string, exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func firstExe(dir string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.exe"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// startExtra starts the services added here; ok=false means "not one of mine".
func (a *App) startExtra(name string) (ok bool, err error) {
	d := a.svcDir(name)
	p := strconv.Itoa(port(name))
	switch name {
	case "caddy", "frankenphp":
		return true, a.startCaddy(name)
	case "meilisearch":
		exe := firstExe(d)
		if exe == "" {
			return true, errors.New("meilisearch .exe not found in " + d)
		}
		return true, spawn(exe, "--http-addr", "127.0.0.1:"+p, "--db-path", a.dataDir("meilisearch"), "--env", "development", "--no-analytics")
	case "ollama":
		return true, spawnEnv([]string{"OLLAMA_HOST=127.0.0.1:" + p, "OLLAMA_MODELS=" + filepath.Join(a.dataDir("ollama"), "models")},
			filepath.Join(d, "ollama.exe"), "serve")
	case "elasticsearch":
		data := a.dataDir("elasticsearch")
		bat := filepath.Join(d, `bin\elasticsearch.bat`)
		return true, spawnEnv([]string{"ES_JAVA_OPTS=-Xms512m -Xmx512m"}, os.Getenv("ComSpec"), "/c", bat,
			"-Ehttp.port="+p, "-Ehttp.host=127.0.0.1", "-Epath.data="+filepath.Join(data, "data"), "-Epath.logs="+filepath.Join(data, "logs"),
			"-Ediscovery.type=single-node", "-Expack.security.enabled=false")
	case "rabbitmq":
		erl := a.svcDir("erlang")
		if erl == "" {
			return true, errors.New("RabbitMQ needs Erlang: install it from Environment or Install (erlang)")
		}
		base := a.dataDir("rabbitmq")
		return true, spawnEnv([]string{"ERLANG_HOME=" + erl, "RABBITMQ_BASE=" + base, "RABBITMQ_NODE_PORT=" + p, "RABBITMQ_NODENAME=rabbit@localhost"},
			os.Getenv("ComSpec"), "/c", filepath.Join(d, `sbin\rabbitmq-server.bat`))
	}
	return false, nil
}

// stopExtra stops the services added here; ok=false means "not one of mine".
func (a *App) stopExtra(name string) bool {
	switch name {
	case "caddy", "frankenphp":
		a.killOwned(name)
		a.stopPHPPools()
	case "meilisearch", "ollama", "elasticsearch", "rabbitmq":
		a.killOwned(name) // no clean shutdown command on Windows for these; all keep a journal/log
	default:
		return false
	}
	return true
}

// ---- Reverse proxy per site ----

// SetSiteProxy forwards a site to a local app (e.g. http://127.0.0.1:3000 for Vite/Next/Go). "" = serve files/PHP.
func (a *App) SetSiteProxy(folder, target string) error {
	if _, err := a.siteDir(folder); err != nil {
		return err
	}
	target = strings.TrimSpace(target)
	if target != "" {
		if !strings.Contains(target, "://") {
			target = "http://" + target
		}
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
			return errors.New("proxy target must look like http://127.0.0.1:3000")
		}
		target = u.Scheme + "://" + u.Host
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.SiteProxy == nil {
		a.cfg.SiteProxy = map[string]string{}
	}
	if target == "" {
		delete(a.cfg.SiteProxy, folder)
	} else {
		a.cfg.SiteProxy[folder] = target
	}
	if err := a.saveConfig(); err != nil {
		return err
	}
	if _, _, err := a.syncVhostFiles(); err != nil {
		return err
	}
	return a.restartWeb()
}

func apacheProxy(target string) string {
	if target == "" {
		return ""
	}
	// upgrade=websocket (Apache 2.4.47+) keeps Vite/Next hot reload working through the proxy.
	return fmt.Sprintf(`
    <IfModule proxy_module>
        ProxyPreserveHost On
        ProxyPass / %[1]s/ upgrade=websocket
        ProxyPassReverse / %[1]s/
    </IfModule>`, target)
}

func nginxLocations(target, fcgi, upstream string) string {
	if target != "" {
		return fmt.Sprintf(`location / {
            proxy_pass %s;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
        }`, target)
	}
	return fmt.Sprintf(`location / { try_files $uri $uri/ /index.php$is_args$args; }
        location ~ \.php$ {
            try_files $uri =404;
            include "%s";
            fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
            fastcgi_pass %s;
        }`, fcgi, upstream)
}

// devpanel-modules.conf is included before sites-enabled so ProxyPass is known when vhosts are read.
const apacheModulesConf = `# Generated by DevPanel: modules needed by per-site reverse proxy.
<IfModule !proxy_module>
    LoadModule proxy_module modules/mod_proxy.so
</IfModule>
<IfModule !proxy_http_module>
    LoadModule proxy_http_module modules/mod_proxy_http.so
</IfModule>
<IfModule !proxy_wstunnel_module>
    LoadModule proxy_wstunnel_module modules/mod_proxy_wstunnel.so
</IfModule>
`

// ensureApacheModules writes devpanel-modules.conf and includes it right before the sites-enabled include.
func (a *App) ensureApacheModules() error {
	conf := a.path(`etc\apache2\devpanel-modules.conf`)
	if _, err := writeIfChanged(conf, apacheModulesConf); err != nil {
		return err
	}
	f := filepath.Join(a.svcDir("apache"), `conf\httpd.conf`)
	b, err := os.ReadFile(f)
	if err != nil || strings.Contains(string(b), "devpanel-modules.conf") {
		return nil
	}
	s := string(b)
	i := strings.Index(s, "IncludeOptional \""+filepath.ToSlash(a.path(`etc\apache2\sites-enabled`)))
	if i < 0 {
		return nil // unusual httpd.conf: leave it, ProxyPass stays inside <IfModule>
	}
	line := `IncludeOptional "` + filepath.ToSlash(conf) + "\"\r\n"
	return os.WriteFile(f, []byte(s[:i]+line+s[i:]), 0644)
}

// ---- Caddy / FrankenPHP ----

func (a *App) caddyConfPath(name string) string {
	return a.path(`etc\caddy\devpanel-` + name + `.Caddyfile`)
}

// caddyConf renders a Caddyfile for all sites. Caddy talks to the php-cgi pool (like Nginx);
// FrankenPHP runs PHP itself (php_server). Certificates are DevPanel's local CA leaf.
func (a *App) caddyConf(name string, list []Site) string {
	fwd := filepath.ToSlash
	_, _, crt, key := a.sslFiles()
	frank := name == "frankenphp"
	_, pools := a.phpPools(list)
	upstreams := func(php string) string {
		var u []string
		for _, p := range pools[php] {
			u = append(u, "127.0.0.1:"+strconv.Itoa(p))
		}
		return strings.Join(u, " ")
	}
	var b strings.Builder
	b.WriteString("# Generated by DevPanel. Do not edit.\n{\n\tauto_https off\n\tadmin off\n")
	if frank {
		b.WriteString("\tfrankenphp\n")
	}
	b.WriteString("}\n\n")
	httpP, sslP := strconv.Itoa(port(name)), strconv.Itoa(port(name+"_ssl"))
	// One HTTPS block (with tls) and one HTTP block per site: Caddy refuses TLS settings on an HTTP listener.
	site := func(names []string, root, php, proxy string) {
		var body string
		switch {
		case proxy != "":
			body = fmt.Sprintf("\treverse_proxy %s\n", proxy)
		case frank:
			body = fmt.Sprintf("\troot * \"%s\"\n\tphp_server\n", fwd(root))
		default:
			body = fmt.Sprintf("\troot * \"%s\"\n\tencode gzip\n\tphp_fastcgi %s\n\tfile_server\n", fwd(root), upstreams(php))
		}
		addrs := func(scheme, p string) string {
			var out []string
			for _, n := range names {
				out = append(out, scheme+"://"+n+":"+p)
			}
			return strings.Join(out, ", ")
		}
		fmt.Fprintf(&b, "%s {\n\ttls \"%s\" \"%s\"\n%s}\n\n", addrs("https", sslP), fwd(crt), fwd(key), body)
		fmt.Fprintf(&b, "%s {\n%s}\n\n", addrs("http", httpP), body)
	}
	site([]string{"localhost"}, a.www(), a.CurrentPHP(), "")
	for _, s := range list {
		names := []string{s.Host, "*." + s.Host}
		for _, al := range s.Aliases {
			names = append(names, al, "*."+al)
		}
		php := s.PHP
		if php == "" {
			php = a.CurrentPHP()
		}
		site(names, s.Root, php, a.cfg.SiteProxy[s.Folder])
	}
	return b.String()
}

func (a *App) caddyExe(name string) string {
	d := a.svcDir(name)
	if name == "frankenphp" {
		return filepath.Join(d, "frankenphp.exe")
	}
	return filepath.Join(d, "caddy.exe")
}

func (a *App) writeCaddyConf(name string, list []Site) (bool, error) {
	if a.svcDir(name) == "" {
		return false, nil
	}
	return writeIfChanged(a.caddyConfPath(name), a.caddyConf(name, list))
}

func (a *App) validateCaddy(name string) error {
	out, err := output(a.caddyExe(name), "validate", "--config", a.caddyConfPath(name), "--adapter", "caddyfile")
	if err != nil || !strings.Contains(strings.ToLower(out), "valid configuration") {
		return fmt.Errorf("%s config error: %s", name, out)
	}
	return nil
}

func (a *App) startCaddy(name string) error {
	list, _ := a.sites()
	if _, err := a.writeLeaf(allHosts(list)); err != nil {
		return err
	}
	if _, err := a.writeCaddyConf(name, list); err != nil {
		return err
	}
	if err := a.validateCaddy(name); err != nil {
		return err
	}
	if name == "caddy" {
		if err := a.startPHPPools(list); err != nil {
			return err
		}
	}
	return spawn(a.caddyExe(name), "run", "--config", a.caddyConfPath(name), "--adapter", "caddyfile")
}

// ---- Runtimes: extra environment variables they expect ----

// runtimeEnv returns JAVA_HOME / DOTNET_ROOT / ERLANG_HOME for the selected versions.
func (a *App) runtimeEnv() []string {
	var env []string
	for _, kv := range [][2]string{{"java", "JAVA_HOME"}, {"dotnet", "DOTNET_ROOT"}, {"erlang", "ERLANG_HOME"}} {
		if d := a.svcDir(kv[0]); d != "" {
			env = append(env, kv[1]+"="+d)
		}
	}
	return env
}
