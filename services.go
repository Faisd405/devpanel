package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Laragon-style service selection: only the services you tick are shown and started by "Start All".
// Missing ones can be installed in place (newest matching package from the catalog).

// Official download links for services Laragon's packages.conf doesn't list (checked: all answer 200).
var builtinPackages = []CustomPkg{
	{"redis-5.0.14.1", "https://github.com/tporadowski/redis/releases/download/v5.0.14.1/Redis-x64-5.0.14.1.zip", "redis"},
	{"mailpit-latest", "https://github.com/axllent/mailpit/releases/latest/download/mailpit-windows-amd64.zip", "mailpit"},
	{"memcached-1.6.8", "https://github.com/jefyt/memcached-windows/releases/download/1.6.8_mingw/memcached-1.6.8-win64-mingw.zip", "memcached"},
	{"mongodb-8.0.4", "https://fastdl.mongodb.org/windows/mongodb-windows-x86_64-8.0.4.zip", "mongodb"},
	{"caddy-2.11.9", "https://github.com/caddyserver/caddy/releases/download/v2.11.9/caddy_2.11.9_windows_amd64.zip", "caddy"},
	{"frankenphp-1.13.4", "https://github.com/php/frankenphp/releases/download/v1.13.4/frankenphp-windows-x86_64.zip", "frankenphp"},
	{"meilisearch-latest", "https://github.com/meilisearch/meilisearch/releases/latest/download/meilisearch-windows-amd64.exe", "meilisearch"},
	{"ollama-latest", "https://github.com/ollama/ollama/releases/latest/download/ollama-windows-amd64.zip", "ollama"},
	{"elasticsearch-9.5.6", "https://artifacts.elastic.co/downloads/elasticsearch/elasticsearch-9.5.6-windows-x86_64.zip", "elasticsearch"},
	{"rabbitmq-4.4.0", "https://github.com/rabbitmq/rabbitmq-server/releases/download/v4.4.0/rabbitmq-server-windows-4.4.0.zip", "rabbitmq"},
	{"erlang-28.5.0.8", "https://github.com/erlang/otp/releases/download/OTP-28.5.0.8/otp_win64_28.5.0.8.zip", "erlang"},
	{"java-21", "https://api.adoptium.net/v3/binary/latest/21/ga/windows/x64/jdk/hotspot/normal/eclipse", "java"},
	{"dotnet-8", "https://aka.ms/dotnet/8.0/dotnet-sdk-win-x64.zip", "dotnet"},
	{"ruby-4.0.9", "https://github.com/oneclick/rubyinstaller2/releases/download/RubyInstaller-4.0.9-1/rubyinstaller-4.0.9-1-x64.7z", "ruby"},
	{"bun-latest", "https://github.com/oven-sh/bun/releases/latest/download/bun-windows-x64.zip", "bun"},
	{"deno-latest", "https://github.com/denoland/deno/releases/latest/download/deno-x86_64-pc-windows-msvc.zip", "deno"},
	// Core stack, so DevPanel also works standalone (no Laragon catalog). PHP/Node come from their official feeds.
	{"apache-2.4.69", "https://www.apachelounge.com/download/VS18/binaries/httpd-2.4.69-261002-Win64-VS18.zip", "apache"},
	{"nginx-1.28.0", "https://nginx.org/download/nginx-1.28.0.zip", "nginx"},
	{"mysql-8.4.3", "https://cdn.mysql.com/archives/mysql-8.4/mysql-8.4.3-winx64.zip", "mysql"},
	{"postgresql-17.2", "https://sbp.enterprisedb.com/getfile.jsp?fileid=1259294", "postgresql"},
	{"phpmyadmin-5.2.2", "https://files.phpmyadmin.net/phpMyAdmin/5.2.2/phpMyAdmin-5.2.2-english.zip", "phpmyadmin"},
	{"go-1.26.5", "https://go.dev/dl/go1.26.5.windows-amd64.zip", "go"},
	{"ngrok-v3", "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-windows-amd64.zip", "ngrok"},
}

type ServiceInfo struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Running   bool   `json:"running"`
	Version   string `json:"version"`
	Category  string `json:"category"`
}

// folderName is the version folder of an install dir, "" when not installed (filepath.Base("") is ".").
func folderName(d string) string {
	if d == "" {
		return ""
	}
	return filepath.Base(d)
}

func serviceLabel(name string) string {
	for _, t := range envTools {
		if t.Name == name {
			return t.Label
		}
	}
	return name
}

// enabled reports whether a service is ticked. Until the user chooses, every installed service counts.
func (a *App) enabled(name string) bool {
	if a.cfg.Services == nil {
		return a.svcDir(name) != ""
	}
	return slices.Contains(a.cfg.Services, name)
}

// ServiceList is the "Manage services" view: all services, installed or not.
func (a *App) ServiceList() []ServiceInfo {
	out := []ServiceInfo{}
	paths := processPaths()
	for _, s := range services {
		d := a.svcDir(s.Name)
		out = append(out, ServiceInfo{s.Name, serviceLabel(s.Name), d != "", a.enabled(s.Name),
			d != "" && portOpen(port(s.Name)) && a.ownedIn(paths, s.Name), folderName(d), categoryOf(s.Name)})
	}
	return out
}

func (a *App) SetServiceEnabled(name string, on bool) error {
	if svc(name).Name == "" {
		return fmt.Errorf("unknown service %q", name)
	}
	if !on && a.running(name) {
		return fmt.Errorf("stop %s before turning it off", name)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Services == nil { // first choice: start from what is currently shown
		a.cfg.Services = []string{}
		for _, s := range services {
			if a.svcDir(s.Name) != "" {
				a.cfg.Services = append(a.cfg.Services, s.Name)
			}
		}
	}
	a.cfg.Services = slices.DeleteFunc(a.cfg.Services, func(s string) bool { return s == name })
	if on {
		a.cfg.Services = append(a.cfg.Services, name)
	}
	a.refreshTray()
	return a.saveConfig()
}

var versionNum = regexp.MustCompile(`\d+(?:\.\d+)*`)

// pickPackage chooses the newest catalog package for a service (MySQL over MariaDB for "mysql").
func pickPackage(pkgs []Package, name string) (Package, bool) {
	prefix := name
	for _, t := range pkgTypes {
		if t.Type == name {
			prefix = t.NamePrefix
		}
	}
	var best Package
	found := false
	for _, p := range pkgs {
		n := strings.ToLower(p.Name)
		if p.Type != name && !strings.HasPrefix(n, prefix) {
			continue
		}
		if !found || verLess(versionNum.FindString(best.Name), versionNum.FindString(p.Name)) {
			best, found = p, true
		}
	}
	return best, found
}

// PackagesFor lists catalog packages that install into this service's folder (bin\php, bin\mysql, ...),
// newest first. Matching on the folder keeps phpMyAdmin out of PHP and puts MariaDB under MySQL.
func (a *App) PackagesFor(name string) []Package {
	dir := `bin\` + name
	for _, t := range pkgTypes {
		if t.Type == name {
			dir = t.Dir
		}
	}
	out := []Package{}
	for _, p := range a.Packages().Packages {
		if strings.EqualFold(p.Group, dir) {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(x, y Package) int {
		vx, vy := versionNum.FindString(x.Folder), versionNum.FindString(y.Folder)
		switch {
		case verLess(vy, vx):
			return -1
		case verLess(vx, vy):
			return 1
		}
		return 0
	})
	return out
}

// InstallService installs the newest package for a service and ticks it.
func (a *App) InstallService(name string) (string, error) {
	if svc(name).Name == "" {
		return "", fmt.Errorf("unknown service %q", name)
	}
	p, ok := pickPackage(a.Packages().Packages, name)
	if !ok {
		return "", fmt.Errorf("no package for %s in the catalog; add a link in Install", name)
	}
	msg := p.Name + " already installed"
	if !p.Installed {
		var err error
		if msg, err = a.Install(p.Name); err != nil {
			return "", err
		}
	}
	if err := a.SetServiceEnabled(name, true); err != nil {
		return "", err
	}
	return msg, nil
}

// StartAll starts every ticked service. Apache wins over Nginx when both are ticked (same port).
func (a *App) StartAll() (string, error) {
	var errs []error
	var notes []string
	web := a.preferredWeb() // only one web server: they share ports 80/443
	if web != "" && !a.running(web) && portOpen(port(web)) && !a.ownPortHolder(processPaths(), web, port(web)) {
		notes = append(notes, fmt.Sprintf("%s skipped: port %d is used by %s", serviceLabel(web), port(web), a.portOwner(port(web))))
		web = ""
	}
	for _, s := range services {
		if !a.enabled(s.Name) || a.svcDir(s.Name) == "" {
			continue
		}
		if isWebServer(s.Name) && s.Name != web {
			if web != "" {
				notes = append(notes, fmt.Sprintf("%s skipped (%s uses the same port; Use on %s switches)", serviceLabel(s.Name), serviceLabel(web), serviceLabel(s.Name)))
			}
			continue
		}
		if err := a.Start(s.Name); err != nil {
			errs = append(errs, err)
		}
	}
	return strings.Join(notes, "; "), errors.Join(errs...)
}

func (a *App) StopAll() error {
	var errs []error
	for _, s := range services {
		if a.running(s.Name) {
			errs = append(errs, a.Stop(s.Name))
		}
	}
	return errors.Join(errs...)
}
