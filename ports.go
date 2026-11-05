package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Extra ports that aren't a service's main port.
var defaultPorts = map[string]int{"apache_ssl": 443, "nginx_ssl": 443, "caddy_ssl": 443, "frankenphp_ssl": 443, "mailpit_smtp": 1025, "php_fastcgi": 9100}

// portTwins: web servers are alternatives on the same ports (only one runs at a time), so sharing is fine.
func portTwins(a, b string) bool {
	ssl := strings.HasSuffix(a, "_ssl")
	return ssl == strings.HasSuffix(b, "_ssl") && isWebServer(strings.TrimSuffix(a, "_ssl")) && isWebServer(strings.TrimSuffix(b, "_ssl"))
}

// portOverrides mirrors Config.Ports so the package-level svc() lookup sees custom ports.
var (
	portMu        sync.RWMutex
	portOverrides = map[string]int{}
)

func setPortOverrides(m map[string]int) {
	portMu.Lock()
	defer portMu.Unlock()
	portOverrides = map[string]int{}
	for k, v := range m {
		portOverrides[k] = v
	}
}

// port returns the configured port for a service or extra port key.
func port(key string) int {
	portMu.RLock()
	p, ok := portOverrides[key]
	portMu.RUnlock()
	if ok {
		return p
	}
	if p, ok := defaultPorts[key]; ok {
		return p
	}
	for _, s := range services {
		if s.Name == key {
			return s.Port
		}
	}
	return 0
}

var portKeys = []struct{ Key, Label, Service string }{
	{"apache", "Apache HTTP", "apache"},
	{"apache_ssl", "Apache HTTPS", "apache"},
	{"nginx", "Nginx HTTP", "nginx"},
	{"nginx_ssl", "Nginx HTTPS", "nginx"},
	{"php_fastcgi", "PHP FastCGI base (Nginx/Caddy)", "nginx"},
	{"caddy", "Caddy HTTP", "caddy"},
	{"caddy_ssl", "Caddy HTTPS", "caddy"},
	{"frankenphp", "FrankenPHP HTTP", "frankenphp"},
	{"frankenphp_ssl", "FrankenPHP HTTPS", "frankenphp"},
	{"meilisearch", "Meilisearch", "meilisearch"},
	{"elasticsearch", "Elasticsearch", "elasticsearch"},
	{"rabbitmq", "RabbitMQ (AMQP)", "rabbitmq"},
	{"ollama", "Ollama", "ollama"},
	{"postgresql", "PostgreSQL", "postgresql"},
	{"mysql", "MySQL", "mysql"},
	{"redis", "Redis", "redis"},
	{"mailpit", "Mailpit web", "mailpit"},
	{"mailpit_smtp", "Mailpit SMTP", "mailpit"},
	{"memcached", "Memcached", "memcached"},
	{"mongodb", "MongoDB", "mongodb"},
}

type PortInfo struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Service string `json:"service"`
	Port    int    `json:"port"`
}

func (a *App) Ports() []PortInfo {
	out := []PortInfo{}
	for _, k := range portKeys {
		out = append(out, PortInfo{k.Key, k.Label, k.Service, port(k.Key)})
	}
	return out
}

// replaceAll applies a regexp replacement to a file; ok=false if nothing matched.
func replaceInFile(path string, re *regexp.Regexp, repl string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if !re.Match(b) {
		return false, nil
	}
	return true, os.WriteFile(path, re.ReplaceAll(b, []byte(repl)), 0644)
}

var (
	listenRe = regexp.MustCompile(`(?m)^Listen\s+\d+`)
	pgPortRe = regexp.MustCompile(`(?m)^#?\s*port\s*=\s*\d+`)
	myPortRe = regexp.MustCompile(`(?m)^port\s*=\s*\d+`)
	rdPortRe = regexp.MustCompile(`(?m)^port\s+\d+`)
)

// SetPort changes a port in the service's own config and remembers it. The service must be stopped.
func (a *App) SetPort(key string, p int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var svcName string
	for _, k := range portKeys {
		if k.Key == key {
			svcName = k.Service
		}
	}
	if svcName == "" {
		return fmt.Errorf("unknown port %q", key)
	}
	if p < 1 || p > 65535 {
		return fmt.Errorf("port must be 1-65535")
	}
	if p == port(key) {
		return nil
	}
	for _, k := range portKeys {
		if k.Key != key && !portTwins(key, k.Key) && port(k.Key) == p {
			return fmt.Errorf("port %d is already used by %s", p, k.Label)
		}
	}
	if a.running(svcName) {
		return fmt.Errorf("stop %s before changing its port", svcName)
	}
	if portOpen(p) {
		return fmt.Errorf("port %d is in use by another program", p)
	}
	d := a.svcDir(svcName)
	var err error
	switch key {
	case "apache":
		_, err = replaceInFile(filepath.Join(d, `conf\httpd.conf`), listenRe, "Listen "+strconv.Itoa(p))
		if err == nil {
			err = a.setDefaultVhostPort(false, p)
		}
	case "apache_ssl":
		_, err = replaceInFile(a.path(`etc\apache2\httpd-ssl.conf`), listenRe, "Listen "+strconv.Itoa(p))
		if err == nil {
			err = a.setDefaultVhostPort(true, p)
		}
	case "postgresql":
		var ok bool
		f := filepath.Join(a.pgData(), "postgresql.conf")
		if ok, err = replaceInFile(f, pgPortRe, "port = "+strconv.Itoa(p)); err == nil && !ok {
			err = appendLine(f, "port = "+strconv.Itoa(p))
		}
	case "mysql":
		_, err = replaceInFile(filepath.Join(d, "my.ini"), myPortRe, "port="+strconv.Itoa(p))
	case "redis":
		_, err = replaceInFile(filepath.Join(d, "redis.windows.conf"), rdPortRe, "port "+strconv.Itoa(p))
	}
	// mailpit takes its ports as start flags, nothing to edit.
	if err != nil {
		return err
	}
	if a.cfg.Ports == nil {
		a.cfg.Ports = map[string]int{}
	}
	a.cfg.Ports[key] = p
	setPortOverrides(a.cfg.Ports)
	if svcName == "apache" {
		if _, _, err := a.syncVhostFiles(); err != nil {
			return err
		}
	}
	return a.saveConfig()
}

var defaultVhostRe = regexp.MustCompile(`(<VirtualHost _default_:)(\d+)>([\s\S]*?</VirtualHost>)`)

// setDefaultVhostPort updates Laragon's 00-default.conf; the block containing SSLEngine is the HTTPS one.
// Matching by content (not by the old port) keeps it right even if the file and config drifted apart.
func (a *App) setDefaultVhostPort(ssl bool, p int) error {
	f := a.path(`etc\apache2\sites-enabled\00-default.conf`)
	b, err := os.ReadFile(f)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	out := defaultVhostRe.ReplaceAllStringFunc(string(b), func(m string) string {
		sub := defaultVhostRe.FindStringSubmatch(m)
		if strings.Contains(sub[3], "SSLEngine") != ssl {
			return m
		}
		return sub[1] + strconv.Itoa(p) + ">" + sub[3]
	})
	return os.WriteFile(f, []byte(out), 0644)
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n" + line + "\n")
	return err
}
