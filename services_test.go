package main

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServiceSelection(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\redis\redis-x64-5.0.14.1`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\postgresql\postgresql`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}

	// Before any choice: installed services are on, others off.
	if !a.enabled("redis") || !a.enabled("postgresql") || a.enabled("mysql") {
		t.Fatal("default selection should be the installed services")
	}
	// First choice snapshots the installed ones, then applies the change.
	if err := a.SetServiceEnabled("mysql", true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.cfg.Services, []string{"postgresql", "redis", "mysql"}) {
		t.Fatalf("services = %v", a.cfg.Services)
	}
	a.SetServiceEnabled("redis", false)
	if a.enabled("redis") || !a.enabled("mysql") {
		t.Fatal("toggle")
	}
	if a.SetServiceEnabled("oracle", true) == nil {
		t.Fatal("unknown service accepted")
	}
	list := a.ServiceList()
	if len(list) != len(services) {
		t.Fatal("ServiceList must show every service")
	}
	for _, s := range list {
		if s.Name == "redis" && (!s.Installed || s.Enabled) {
			t.Fatalf("redis row: %+v", s)
		}
	}
}

func TestPickPackage(t *testing.T) {
	pkgs := []Package{
		{Name: "mysql-8.0", Group: `bin\mysql`}, {Name: "mysql-9.1", Group: `bin\mysql`}, {Name: "mariadb-11.4", Group: `bin\mysql`},
		{Name: "postgresql-16.6"}, {Name: "postgresql-17.2"}, {Name: "mailpit-latest", Type: "mailpit"},
		{Name: "PHP-8.4"},
	}
	for svcName, want := range map[string]string{"mysql": "mysql-9.1", "postgresql": "postgresql-17.2", "mailpit": "mailpit-latest"} {
		if p, ok := pickPackage(pkgs, svcName); !ok || p.Name != want {
			t.Errorf("%s: %q want %q", svcName, p.Name, want)
		}
	}
	if _, ok := pickPackage(pkgs, "mongodb"); ok {
		t.Fatal("found a package that does not exist")
	}
}

func TestStartAllSkipsNginxWithApache(t *testing.T) {
	defer setPortOverrides(nil)
	setPortOverrides(map[string]int{"apache": freePort(t), "nginx": freePort(t)})
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\apache\httpd-x\bin`), 0755) // no httpd.exe: starting just fails
	os.MkdirAll(filepath.Join(root, `bin\nginx\nginx-x`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}, Services: []string{"apache", "nginx"}}}
	note, _ := a.StartAll()
	if !strings.Contains(note, "Nginx skipped") {
		t.Fatalf("note = %q", note)
	}
}

func TestInstallService(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("mailpit.exe")
	f.Write([]byte("MZ"))
	w.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.Write(buf.Bytes()) }))
	defer srv.Close()

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr"), 0755)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), nil, 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}, Services: []string{},
		Packages: []CustomPkg{{"mailpit-9.9", srv.URL + "/mailpit-9.9.zip", "mailpit"}}}, cfgPath: filepath.Join(root, "devpanel.json")}
	if _, err := a.InstallService("mailpit"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, `bin\mailpit\mailpit-9.9\mailpit.exe`)) || !a.enabled("mailpit") {
		t.Fatal("not installed or not ticked")
	}
	if a.svcDir("mailpit") == "" {
		t.Fatal("installed service not found by svcDir")
	}
}

func TestNotInstalledHasNoVersion(t *testing.T) {
	a := &App{cfg: Config{Root: t.TempDir(), Versions: map[string]string{}}}
	for _, s := range a.Status() {
		if s.Version != "" {
			t.Fatalf("%s: version %q while not installed", s.Name, s.Version)
		}
	}
	for _, s := range a.ServiceList() {
		if s.Version != "" || s.Installed {
			t.Fatalf("%s: %+v", s.Name, s)
		}
	}
}

func TestPackagesFor(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr"), 0755)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), []byte(
		"PHP-8.3=https://x/php-8.3.18-nts-Win32-vs16-x64.zip\nPHP-8.5=https://x/php-8.5.4-nts-Win32-vs17-x64.zip\nPHP-8.4=https://x/php-8.4.5-nts-Win32-vs17-x64.zip\n"+
			"phpmyadmin=https://x/phpMyAdmin-5.2.2-english.zip\nmysql-8.4=https://x/mysql-8.4.3-winx64.zip\nmariadb-11.4=https://x/mariadb-11.4.4-winx64.zip\n"), 0644)
	os.MkdirAll(filepath.Join(root, `bin\php\php-8.4.5-nts-Win32-vs17-x64`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	var names []string
	for _, p := range a.PackagesFor("php") {
		names = append(names, p.Name)
		if p.Name == "PHP-8.4" && !p.Installed {
			t.Error("installed flag missing")
		}
	}
	if !slices.Equal(names, []string{"PHP-8.5", "PHP-8.4", "PHP-8.3"}) {
		t.Fatalf("php packages = %v (phpMyAdmin must not appear, newest first)", names)
	}
	names = nil
	for _, p := range a.PackagesFor("mysql") {
		names = append(names, p.Name)
	}
	if !slices.Contains(names, "mysql-8.4") || !slices.Contains(names, "mariadb-11.4") {
		t.Fatalf("mysql packages = %v", names)
	}
}

func TestCategories(t *testing.T) {
	// Every service and environment tool belongs to a real category (none fall into "Other").
	for _, s := range services {
		if categoryOf(s.Name) == "Other" {
			t.Errorf("service %s has no category", s.Name)
		}
	}
	for _, e := range envTools {
		if categoryOf(e.Name) == "Other" {
			t.Errorf("tool %s has no category", e.Name)
		}
	}
	if categoryOf("caddy") != "Web & App Servers" || categoryOf("php") != "Languages & Runtimes" || categoryOf("rabbitmq") != "Cache & Messaging" {
		t.Fatal("wrong category")
	}
	a := &App{cfg: Config{Root: t.TempDir(), Versions: map[string]string{}}}
	for _, s := range a.Status() {
		if s.Category == "" {
			t.Fatalf("%s: empty category in Status", s.Name)
		}
	}
}

func TestRuntimeDetails(t *testing.T) {
	root := t.TempDir()
	node := filepath.Join(root, `bin\nodejs\node-v22`)
	os.MkdirAll(node, 0755)
	os.MkdirAll(filepath.Join(root, `bin\java\jdk-21`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	rs := a.Runtimes()
	if len(rs) != len(runtimeNames) {
		t.Fatalf("Runtimes = %d", len(rs))
	}
	for _, r := range rs {
		if r.Category != "Languages & Runtimes" {
			t.Errorf("%s category %q", r.Name, r.Category)
		}
		if r.Name == "nodejs" && r.Version != "node-v22" || r.Name == "go" && r.Version != "" {
			t.Errorf("%s version %q", r.Name, r.Version)
		}
	}
	d, err := a.Detail("java")
	if err != nil || d.Version != "jdk-21" {
		t.Fatalf("java detail: %+v %v", d, err)
	}
	info := map[string]string{}
	for _, kv := range d.Info {
		info[kv[0]] = kv[1]
	}
	if info["JAVA_HOME"] != filepath.Join(root, `bin\java\jdk-21`) || !strings.Contains(info["On PATH"], `jdk-21\bin`) {
		t.Fatalf("java info: %v", info)
	}
	if !a.allowed(filepath.Join(root, `bin\java\jdk-21`), true) {
		t.Fatal("runtime folder must be openable")
	}
	if _, err := a.RunTool("go", "version"); err == nil {
		t.Fatal("tool on a missing runtime must fail")
	}
	if _, err := a.RunTool("nodejs", "version"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("no node.exe in the folder: %v", err)
	}
}
