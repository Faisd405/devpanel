package main

import (
	"context"
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
	"time"
)

type FileRef struct {
	Label  string `json:"label"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Log    bool   `json:"log"` // offer "View" (tail) in addition to Edit
}

type Tool struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"` // opened in the browser by the UI instead of calling RunTool
}

type Detail struct {
	Name    string      `json:"name"`
	Label   string      `json:"label"`
	Version string      `json:"version"`
	Dir     string      `json:"dir"`
	Port    int         `json:"port"`
	Service bool        `json:"service"`
	Running bool        `json:"running"`
	Info    [][2]string `json:"info"`
	Files   []FileRef   `json:"files"`
	Folders []FileRef   `json:"folders"`
	Tools   []Tool      `json:"tools"`
}

var detailNames = []string{"apache", "nginx", "caddy", "frankenphp", "php", "postgresql", "mysql", "redis", "mailpit", "memcached", "mongodb",
	"meilisearch", "elasticsearch", "rabbitmq", "ollama"}

func ref(label, path string, log bool) FileRef {
	_, err := os.Stat(path)
	return FileRef{label, path, err == nil, log}
}

var datadirRe = regexp.MustCompile(`(?m)^\s*datadir\s*=\s*"?([^"\r\n]+?)"?\s*$`)

func (a *App) mysqlData(dir string) string {
	if b, err := os.ReadFile(findMyIni(dir)); err == nil {
		if m := datadirRe.FindSubmatch(b); m != nil {
			return filepath.Clean(string(m[1]))
		}
	}
	return a.path(`data\` + filepath.Base(dir))
}

// Detail describes one service (or PHP): where it lives, its config/log files and its tools.
func (a *App) Detail(name string) (Detail, error) {
	if !slices.Contains(detailNames, name) && !isRuntime(name) {
		return Detail{}, fmt.Errorf("unknown service %q", name)
	}
	dir := a.svcDir(name)
	d := Detail{Name: name, Dir: dir, Version: filepath.Base(dir)}
	for _, t := range envTools {
		if t.Name == name {
			d.Label = t.Label
		}
	}
	if s := svc(name); s.Name != "" {
		d.Service, d.Port, d.Running = true, s.Port, a.running(name)
	}
	if dir == "" {
		d.Version = ""
		return d, nil
	}
	d.Info = [][2]string{{"Folder", dir}}
	if isRuntime(name) {
		a.runtimeDetail(&d)
		return d, nil
	}
	switch name {
	case "apache":
		d.Info = append(d.Info, [2]string{"Ports", fmt.Sprintf("%d (http), %d (https)", port("apache"), port("apache_ssl"))}, [2]string{"PHP", a.CurrentPHP() + " via mod_fcgid"})
		conf, logs := filepath.Join(dir, `conf\httpd.conf`), filepath.Join(dir, "logs")
		if a.isAdopted("apache") { // linked: DevPanel's own config and logs, the other environment's are not used
			conf, logs = a.adoptedApacheConf(), a.path(`data\apache`)
			d.Info = append(d.Info, [2]string{"Linked from", resolveDir(dir) + " (its httpd.conf is not used or changed)"})
		}
		d.Files = []FileRef{
			ref("httpd.conf", conf, false),
			ref("httpd-ssl.conf", a.path(`etc\apache2\httpd-ssl.conf`), false),
			ref("fcgid.conf (PHP handler)", a.path(`etc\apache2\fcgid.conf`), false),
			ref("error.log", filepath.Join(logs, "error.log"), true),
			ref("access.log", filepath.Join(logs, "access.log"), true),
		}
		d.Folders = []FileRef{ref("sites-enabled (vhosts)", a.path(`etc\apache2\sites-enabled`), false), ref("alias", a.path(`etc\apache2\alias`), false), ref("logs", logs, false)}
		d.Tools = []Tool{{"test", "Test config (httpd -t)", ""}, {"vhosts", "List vhosts (httpd -S)", ""}, {"", "Open http://localhost", fmt.Sprintf("http://localhost:%d", port("apache"))}}
	case "nginx":
		d.Info = append(d.Info, [2]string{"Ports", fmt.Sprintf("%d (http), %d (https)", port("nginx"), port("nginx_ssl"))},
			[2]string{"PHP", fmt.Sprintf("php-cgi pool, %d per version, from port %d", poolSize, port("php_fastcgi"))},
			[2]string{"Note", "Uses the same port as Apache: run one of them (pick in a group)"})
		d.Files = []FileRef{
			ref("devpanel-nginx.conf (generated)", a.nginxConfPath(), true),
			ref("error.log", filepath.Join(a.nginxPrefix(), `logs\error.log`), true),
			ref("access.log", filepath.Join(a.nginxPrefix(), `logs\access.log`), true),
		}
		d.Folders = []FileRef{ref("Nginx folder", dir, false), ref("logs", filepath.Join(a.nginxPrefix(), "logs"), false)}
		d.Tools = []Tool{{"test", "Test config (nginx -t)", ""}, {"", "Open http://localhost", fmt.Sprintf("http://localhost:%d", port("nginx"))}}
	case "php":
		d.Info = append(d.Info, [2]string{"Used by", "Apache (fcgid), Nginx (php-cgi pool) and CLI terminal"})
		d.Files = []FileRef{ref("php.ini", filepath.Join(dir, "php.ini"), false)}
		d.Folders = []FileRef{ref("ext", filepath.Join(dir, "ext"), false), ref("PHP folder", dir, false)}
		d.Tools = []Tool{{"version", "php -v", ""}, {"modules", "php -m", ""}}
	case "postgresql":
		data := a.pgData()
		d.Info = append(d.Info, [2]string{"Data", data}, [2]string{"User", "postgres (trust, no password)"})
		d.Files = []FileRef{
			ref("postgresql.conf", filepath.Join(data, "postgresql.conf"), false),
			ref("pg_hba.conf (access)", filepath.Join(data, "pg_hba.conf"), false),
			ref("devpanel.log", filepath.Join(data, "devpanel.log"), true),
		}
		d.Folders = []FileRef{ref("data", data, false), ref("bin", filepath.Join(dir, "bin"), false)}
		d.Tools = []Tool{{"psql", "Open psql", ""}, {"heidisql", "Open HeidiSQL", ""}}
	case "mysql":
		data := a.mysqlData(dir)
		d.Info = append(d.Info, [2]string{"Data", data}, [2]string{"User", "root"})
		d.Files = []FileRef{ref("my.ini", findMyIni(dir), false)}
		if errs, _ := filepath.Glob(filepath.Join(data, "*.err")); len(errs) > 0 {
			d.Files = append(d.Files, ref("error log", errs[0], true))
		}
		d.Folders = []FileRef{ref("data", data, false), ref("bin", filepath.Join(dir, "bin"), false)}
		d.Tools = []Tool{{"cli", "Open mysql client", ""}, {"heidisql", "Open HeidiSQL", ""}}
	case "redis":
		d.Files = []FileRef{ref("redis.windows.conf", filepath.Join(dir, "redis.windows.conf"), false)}
		d.Folders = []FileRef{ref("Redis folder", dir, false)}
		d.Tools = []Tool{{"cli", "Open redis-cli", ""}, {"info", "Server info", ""}}
	case "caddy", "frankenphp":
		d.Info = append(d.Info, [2]string{"Ports", fmt.Sprintf("%d (http), %d (https)", port(name), port(name+"_ssl"))},
			[2]string{"Note", "Uses the same ports as Apache/Nginx: run one web server at a time"})
		d.Files = []FileRef{ref("Caddyfile (generated)", a.caddyConfPath(name), true)}
		d.Folders = []FileRef{ref(d.Label+" folder", dir, false)}
		d.Tools = []Tool{{"", "Open http://localhost", fmt.Sprintf("http://localhost:%d", port(name))}}
	case "meilisearch":
		d.Info = append(d.Info, [2]string{"URL", fmt.Sprintf("http://127.0.0.1:%d (no master key, development mode)", port(name))})
		d.Folders = []FileRef{ref("data", a.path(`data\meilisearch`), false), ref("Meilisearch folder", dir, false)}
		d.Tools = []Tool{{"", "Open dashboard", fmt.Sprintf("http://localhost:%d", port(name))}}
	case "elasticsearch":
		d.Info = append(d.Info, [2]string{"URL", fmt.Sprintf("http://127.0.0.1:%d (security off, single node, 512 MB heap)", port(name))})
		d.Files = []FileRef{ref("elasticsearch.yml", filepath.Join(dir, `config\elasticsearch.yml`), false)}
		d.Folders = []FileRef{ref("data & logs", a.path(`data\elasticsearch`), false), ref("Elasticsearch folder", dir, false)}
		d.Tools = []Tool{{"", "Open http://localhost", fmt.Sprintf("http://localhost:%d", port(name))}}
	case "rabbitmq":
		d.Info = append(d.Info, [2]string{"AMQP", fmt.Sprintf("amqp://guest:guest@127.0.0.1:%d", port(name))},
			[2]string{"Erlang", folderName(a.svcDir("erlang"))})
		d.Folders = []FileRef{ref("data & logs", a.dataDir("rabbitmq"), false), ref("RabbitMQ folder", dir, false)}
	case "ollama":
		d.Info = append(d.Info, [2]string{"API", fmt.Sprintf("http://127.0.0.1:%d", port(name))}, [2]string{"Models", a.path(`data\ollama\models`)})
		d.Folders = []FileRef{ref("models", a.path(`data\ollama\models`), false), ref("Ollama folder", dir, false)}
		d.Tools = []Tool{{"models", "List models (ollama list)", ""}}
	case "memcached":
		d.Info = append(d.Info, [2]string{"Memory", "64 MB, listens on 127.0.0.1"})
		d.Folders = []FileRef{ref("Memcached folder", dir, false)}
	case "mongodb":
		data := a.mongoData()
		d.Info = append(d.Info, [2]string{"Data", data}, [2]string{"URI", fmt.Sprintf("mongodb://127.0.0.1:%d", port("mongodb"))})
		d.Files = []FileRef{ref("mongod.log", filepath.Join(data, "mongod.log"), true)}
		d.Folders = []FileRef{ref("data", data, false), ref("bin", filepath.Join(dir, "bin"), false)}
	case "mailpit":
		d.Info = append(d.Info, [2]string{"SMTP", fmt.Sprintf("127.0.0.1:%[1]d (MAIL_PORT=%[1]d)", port("mailpit_smtp"))})
		d.Folders = []FileRef{ref("Mailpit folder", dir, false)}
		d.Tools = []Tool{{"", "Open inbox", fmt.Sprintf("http://localhost:%d", port("mailpit"))}}
	}
	return d, nil
}

// allowed limits file/folder actions to paths listed in some service's Detail.
func (a *App) allowed(p string, folder bool) bool {
	p = filepath.Clean(p)
	for _, n := range append(slices.Clone(detailNames), runtimeNames...) {
		d, _ := a.Detail(n)
		list := d.Files
		if folder {
			list = d.Folders
		}
		for _, f := range list {
			if strings.EqualFold(filepath.Clean(f.Path), p) {
				return true
			}
		}
	}
	return false
}

func (a *App) openInEditor(file string) error {
	editor := a.path(`bin\notepad++\notepad++.exe`)
	if _, err := os.Stat(editor); err != nil {
		editor = "notepad.exe"
	}
	// Not hidden(): that would start the editor window hidden.
	return exec.Command(editor, file).Start()
}

func (a *App) EditFile(p string) error {
	if !a.allowed(p, false) {
		return errors.New("not a known service file")
	}
	return a.openInEditor(p)
}

func (a *App) OpenFolder(p string) error {
	if !a.allowed(p, true) {
		return errors.New("not a known service folder")
	}
	return exec.Command("explorer", p).Start()
}

// TailFile returns the last n lines of a service log (reads at most the last 256 KB).
func (a *App) TailFile(p string, n int) (string, error) {
	if !a.allowed(p, false) {
		return "", errors.New("not a known service file")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, _ := f.Stat(); st != nil && st.Size() > 256<<10 {
		f.Seek(-256<<10, io.SeekEnd)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// output runs a short command and returns its combined output, even when it exits non-zero.
func output(exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := hidden(exec.CommandContext(ctx, exe, args...)).CombinedOutput()
	if len(out) == 0 && err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// terminal opens a console with the selected environment and runs a command in it.
func (a *App) terminal(args ...string) error {
	if err := a.writeEnvCmd(); err != nil {
		return err
	}
	// ^& is unescaped by the outer cmd so the new console runs: env.cmd & <args>
	cmd := append([]string{"/c", "start", "", "/D", a.www(), "cmd", "/k", a.envCmdPath(), "^&"}, args...)
	return exec.Command("cmd", cmd...).Run()
}

func (a *App) RunTool(name, id string) (string, error) {
	dir := a.svcDir(name)
	if dir == "" {
		return "", fmt.Errorf("%s is not installed", name)
	}
	if isRuntime(name) {
		return a.runtimeTool(name, id)
	}
	heidi := func() (string, error) {
		return "HeidiSQL opened", exec.Command(a.path(`bin\heidisql\heidisql.exe`)).Start()
	}
	switch name + "/" + id {
	case "apache/test":
		return output(filepath.Join(dir, `bin\httpd.exe`), append(a.apacheArgs(), "-t")...)
	case "ollama/models":
		cmd := hidden(exec.Command(filepath.Join(dir, "ollama.exe"), "list"))
		cmd.Env = append(os.Environ(), fmt.Sprintf("OLLAMA_HOST=127.0.0.1:%d", port("ollama")))
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), nil
	case "nginx/test":
		return output(filepath.Join(dir, "nginx.exe"), "-t", "-p", a.nginxPrefix(), "-c", a.nginxConfPath())
	case "apache/vhosts":
		return output(filepath.Join(dir, `bin\httpd.exe`), append(a.apacheArgs(), "-S")...)
	case "php/version":
		return output(filepath.Join(dir, "php.exe"), "-v")
	case "php/modules":
		return output(filepath.Join(dir, "php.exe"), "-m")
	case "postgresql/psql":
		return "psql opened", a.terminal("psql", "-U", "postgres", "-p", strconv.Itoa(port("postgresql")))
	case "mysql/cli":
		args := []string{"mysql", "-uroot", "-P" + strconv.Itoa(port("mysql"))}
		if mysqlPwd != "" {
			args = append(args, "-p") // prompts in the console instead of exposing the password
		}
		return "mysql client opened", a.terminal(args...)
	case "postgresql/heidisql", "mysql/heidisql":
		return heidi()
	case "redis/cli":
		return "redis-cli opened", a.terminal("redis-cli", "-p", strconv.Itoa(port("redis")))
	case "redis/info":
		return output(filepath.Join(dir, "redis-cli.exe"), "-p", strconv.Itoa(port("redis")), "info", "server")
	}
	return "", fmt.Errorf("unknown tool %s/%s", name, id)
}

// ---- PHP extensions ----

type PHPExt struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

var zendExts = []string{"opcache", "xdebug"}

// extRe matches real directive lines only (`extension=x` or `;extension=x`), not doc comments like `;   extension=x`.
func extRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?im)^[ \t]*(;?)(zend_extension|extension)[ \t]*=[ \t]*"?(?:php_)?` + regexp.QuoteMeta(name) + `(?:\.dll)?"?[ \t]*(\r?)$`)
}

func extEnabled(ini, name string) bool {
	for _, m := range extRe(name).FindAllStringSubmatch(ini, -1) {
		if m[1] == "" {
			return true
		}
	}
	return false
}

// setExt enables (first matching line uncommented, others commented) or disables (all commented) an extension.
func setExt(ini, name string, on bool) string {
	re := extRe(name)
	first := true
	found := false
	out := re.ReplaceAllStringFunc(ini, func(line string) string {
		m := re.FindStringSubmatch(line)
		found = true
		semi := ";"
		if on && first {
			semi = ""
		}
		first = false
		return semi + m[2] + "=" + name + m[3]
	})
	if on && !found {
		key := "extension"
		if slices.Contains(zendExts, name) {
			key = "zend_extension"
		}
		if !strings.HasSuffix(out, "\n") {
			out += "\r\n"
		}
		out += key + "=" + name + "\r\n"
	}
	return out
}

func (a *App) PHPExtensions() ([]PHPExt, error) {
	dir := a.svcDir("php")
	ini, err := os.ReadFile(filepath.Join(dir, "php.ini"))
	if err != nil {
		return nil, fmt.Errorf("php.ini: %w", err)
	}
	dlls, _ := filepath.Glob(filepath.Join(dir, `ext\php_*.dll`))
	out := []PHPExt{}
	for _, f := range dlls {
		n := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "php_"), ".dll")
		out = append(out, PHPExt{n, extEnabled(string(ini), n)})
	}
	return out, nil
}

// SetPHPExtension toggles an extension in the selected PHP's php.ini and restarts Apache if running.
func (a *App) SetPHPExtension(name string, on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dir := a.svcDir("php")
	if _, err := os.Stat(filepath.Join(dir, `ext\php_`+name+`.dll`)); err != nil {
		return fmt.Errorf("extension %q not found in %s\\ext", name, filepath.Base(dir))
	}
	f := filepath.Join(dir, "php.ini")
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(f, []byte(setExt(string(b), name, on)), 0644); err != nil {
		return err
	}
	return a.restartWeb()
}
