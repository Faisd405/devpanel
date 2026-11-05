package main

import (
	"archive/zip"
	"crypto/x509"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Uses a temp root only: never touches services, the real hosts file, or C:\laragon.
func TestVhostsAndPHP(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{`www\laravel\public`, `www\plain`, `www\node_modules`, `etc\apache2\sites-enabled`, `bin\php\php-8.4\x`} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	os.WriteFile(filepath.Join(root, `bin\php\php-8.4\php-cgi.exe`), nil, 0644)
	sites := filepath.Join(root, `etc\apache2\sites-enabled`)
	os.WriteFile(filepath.Join(sites, "auto.gone.test.conf"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(sites, "00-default.conf"), []byte("x"), 0644)

	a := &App{cfg: Config{Root: root, TLD: "test"}}
	hosts, changed, err := a.syncVhostFiles()
	if err != nil || !slices.Equal(hosts, []string{"laravel.test", "plain.test"}) || changed != 5 {
		t.Fatalf("hosts=%v changed=%d err=%v", hosts, changed, err)
	}
	conf, _ := os.ReadFile(filepath.Join(sites, "auto.laravel.test.conf"))
	if !strings.Contains(string(conf), filepath.ToSlash(filepath.Join(root, `www\laravel\public`))) {
		t.Fatalf("laravel vhost should point at public/: %s", conf)
	}
	if _, err := os.Stat(filepath.Join(sites, "auto.gone.test.conf")); !os.IsNotExist(err) {
		t.Fatal("stale vhost not removed")
	}
	if _, err := os.Stat(filepath.Join(sites, "00-default.conf")); err != nil {
		t.Fatal("non-auto conf must be kept")
	}
	if _, changed, _ = a.syncVhostFiles(); changed != 0 {
		t.Fatalf("second sync should be a no-op, changed=%d", changed)
	}

	hostsFile := "127.0.0.1 laravel.test #laragon magic!\n# 127.0.0.1 plain.test\n"
	if m := missingHosts(hostsFile, hosts); !slices.Equal(m, []string{"plain.test"}) {
		t.Fatalf("missing=%v", m)
	}

	fcgid := `FcgidInitialEnv PATH "C:/laragon/bin/php/php-8.5.12-nts;C:/Windows"
FcgidWrapper "C:/laragon/bin/php/php-8.5.12-nts/php-cgi.exe" .php`
	os.WriteFile(filepath.Join(root, `etc\apache2\fcgid.conf`), []byte(fcgid), 0644)
	if v := a.CurrentPHP(); v != "php-8.5.12-nts" {
		t.Fatalf("CurrentPHP=%q", v)
	}
	if got := phpRe.ReplaceAllString(fcgid, "bin/php/php-8.4"); strings.Count(got, "php-8.4") != 2 || strings.Contains(got, "8.5.12") {
		t.Fatalf("replace: %s", got)
	}
	if !slices.Equal(a.PHPVersions(), []string{"php-8.4"}) {
		t.Fatalf("PHPVersions=%v", a.PHPVersions())
	}
}

func makeZip(t *testing.T, files map[string]string) string {
	p := filepath.Join(t.TempDir(), "x.zip")
	f, _ := os.Create(p)
	w := zip.NewWriter(f)
	for name, body := range files {
		fw, _ := w.Create(name)
		fw.Write([]byte(body))
	}
	w.Close()
	f.Close()
	return p
}

func TestInstallLogic(t *testing.T) {
	cases := [][4]string{
		{"*PHP-8.4", "https://x/php-8.4.5-nts-Win32-vs17-x64.zip", `bin\php`, "php-8.4.5-nts-Win32-vs17-x64"},
		{"postgresql-17.2", "https://x/getfile.jsp?fileid=1", `bin\postgresql`, "postgresql-17.2"},
		{"phpmyadmin", "https://x/phpMyAdmin-5.2.2-english.zip", `etc\apps`, "phpMyAdmin"},
		{"go-1.24", "https://go.dev/dl/go1.24.1.windows-amd64.zip", `bin\go`, "go1.24.1"},
		{"node-22.14", "https://x/node-v22.14.0-win-x64.zip", `bin\nodejs`, "node-v22.14.0-win-x64"},
	}
	for _, c := range cases {
		if g, f := pkgTarget(c[0], c[1], ""); g != c[2] || f != c[3] {
			t.Errorf("pkgTarget(%s) = %s, %s", c[0], g, f)
		}
	}

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `usr`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\php\php-8.4.5-nts-Win32-vs17-x64`), 0755)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), []byte("# PHP\n# https://windows.php.net/\n*PHP-8.4=https://x/php-8.4.5-nts-Win32-vs17-x64.zip\n#mongodb-8=https://x/m.zip\n---\nredis=https://x/redis-7.zip\n"), 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	pl := a.Packages()
	pkgs := nonBuiltin(pl.Packages)
	if len(pl.Errors) > 0 || len(pkgs) != 2 || !pkgs[0].Installed || pkgs[1].Installed || pkgs[1].Group != `bin\redis` {
		t.Fatalf("Packages = %+v", pl)
	}

	// Single top-level folder is stripped, loose top-level files dropped (Apache Lounge layout).
	dest := filepath.Join(t.TempDir(), "apache")
	if err := extract(makeZip(t, map[string]string{"Apache24/bin/httpd.exe": "x", "ReadMe.txt": "r"}), dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, `bin\httpd.exe`)); err != nil {
		t.Fatal("Apache24/ not stripped")
	}
	// Flat archive (PHP layout) kept as-is.
	dest = filepath.Join(t.TempDir(), "php")
	extract(makeZip(t, map[string]string{"php-cgi.exe": "x", "ext/php_pgsql.dll": "x", "dev/a.lib": "x"}), dest)
	if _, err := os.Stat(filepath.Join(dest, `ext\php_pgsql.dll`)); err != nil {
		t.Fatal("flat archive changed")
	}
	// Zip-slip is rejected.
	if err := extract(makeZip(t, map[string]string{"a/../../evil.txt": "x", "b/ok": "x"}), filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatal("zip-slip not rejected")
	}

	os.WriteFile(filepath.Join(dest, "php.ini-development"), []byte("; x\r\n;extension_dir = \"ext\"\r\n;extension=pgsql\r\n;extension=ldap\r\n"), 0644)
	if err := setupPHPIni(dest); err != nil {
		t.Fatal(err)
	}
	ini, _ := os.ReadFile(filepath.Join(dest, "php.ini"))
	if s := string(ini); !strings.Contains(s, "\nextension_dir = \""+filepath.ToSlash(filepath.Join(dest, "ext"))+"\"") || !strings.Contains(s, "\nextension=pgsql") || !strings.Contains(s, ";extension=ldap") {
		t.Fatalf("php.ini:\n%s", s)
	}

	// Pinned version wins over newer installs; unknown pin falls back to highest.
	os.MkdirAll(filepath.Join(root, `bin\postgresql\postgresql`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\postgresql\postgresql-17.2`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\postgresql\postgresql-18.0.partial`), 0755)
	a.cfg.Versions["postgresql"] = "postgresql"
	if a.pgData() != filepath.Join(root, `data\postgresql`) {
		t.Fatalf("pgData = %s", a.pgData())
	}
	a.cfg.Versions["postgresql"] = "missing"
	if filepath.Base(a.svcDir("postgresql")) != "postgresql-17.2" || a.pgData() != filepath.Join(root, `data\postgresql-17.2`) {
		t.Fatalf("svcDir = %s", a.svcDir("postgresql"))
	}
}

func TestSiteNaming(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{`www\app_webshop\public`, `www\app_webshop-prod`, `www\laravel13`, `www\APP_x`, `www\app_`} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	a := &App{cfg: Config{Root: root, TLD: "test", HostRules: []HostRule{{"prefix", "app_", ""}}, Hostnames: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	hosts := func() map[string]string {
		list, err := a.sites()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, s := range list {
			m[s.Folder] = s.Host
		}
		return m
	}
	want := map[string]string{"app_webshop": "webshop.test", "app_webshop-prod": "webshop-prod.test", "laravel13": "laravel13.test", "APP_x": "x.test", "app_": "app_.test"}
	if got := hosts(); !maps.Equal(got, want) {
		t.Fatalf("default hosts = %v", got)
	}

	// Custom name: TLD appended, lowercased; resetting to default/empty clears the override.
	if err := a.SetHostname("app_webshop", "Customs"); err != nil || hosts()["app_webshop"] != "customs.test" {
		t.Fatalf("custom: %v %v", err, hosts())
	}
	if err := a.SetHostname("laravel13", "l13.local"); err != nil || hosts()["laravel13"] != "l13.local" {
		t.Fatalf("custom with own TLD: %v", err)
	}
	a.SetHostname("laravel13", "")
	if _, ok := a.cfg.Hostnames["laravel13"]; ok {
		t.Fatal("empty name should remove override")
	}
	// Collision and invalid names are rejected and leave config unchanged.
	if err := a.SetHostname("laravel13", "customs"); err == nil || hosts()["laravel13"] != "laravel13.test" {
		t.Fatalf("collision accepted: %v", err)
	}
	for _, bad := range []string{"has space", "-x", "a/b", "x.test."} {
		if err := a.SetHostname("laravel13", bad); err == nil {
			t.Errorf("invalid %q accepted", bad)
		}
	}
	if err := a.SetHostname("../etc", "x"); err == nil {
		t.Fatal("path traversal folder accepted")
	}
	// A prefix change that creates a collision is rejected.
	os.MkdirAll(filepath.Join(root, `www\webshop-prod`), 0755)
	if _, err := a.sites(); err == nil || !strings.Contains(err.Error(), "webshop-prod.test") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	if r := a.Sites(); len(r.Sites) == 0 || r.Error == "" {
		t.Fatal("UI view must still list sites on collision")
	}
	os.Remove(filepath.Join(root, `www\webshop-prod`))
	if err := a.SetHostRules([]HostRule{{"prefix", " APP_ ", ""}, {"prefix", "", ""}}); err != nil || len(a.cfg.HostRules) != 1 || a.cfg.HostRules[0].From != "app_" {
		t.Fatalf("rules: %v %v", err, a.cfg.HostRules)
	}

	// Vhost files follow the hostname; old app_* files are removed (+1 change for the new certificate).
	sites := filepath.Join(root, `etc\apache2\sites-enabled`)
	os.MkdirAll(sites, 0755)
	os.WriteFile(filepath.Join(sites, "auto.app_webshop.test.conf"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(sites, "auto.old.local.conf"), []byte("old"), 0644)
	if _, _, err := a.syncVhostFiles(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"auto.app_webshop.test.conf", "auto.old.local.conf"} {
		if _, err := os.Stat(filepath.Join(sites, f)); !os.IsNotExist(err) {
			t.Errorf("%s not removed", f)
		}
	}
	conf, err := os.ReadFile(filepath.Join(sites, "auto.customs.test.conf"))
	if err != nil || !strings.Contains(string(conf), `define SITE "customs.test"`) || !strings.Contains(string(conf), "app_webshop/public") {
		t.Fatalf("customs vhost: %v\n%s", err, conf)
	}
}

func TestHostRules(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{`www\app_webshop`, `www\app_webshop-prod`, `www\shop-prod`} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	a := &App{cfg: Config{Root: root, TLD: "test", Hostnames: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	host := func(f string) string { return a.defaultHost(f) }

	// Prefix replaced instead of dropped; suffix rule applied together with it.
	if err := a.SetHostRules([]HostRule{{"prefix", "app_", "acme_"}, {"suffix", "-prod", "-live"}}); err != nil {
		t.Fatal(err)
	}
	if h := host("app_webshop"); h != "acme_webshop.test" {
		t.Fatalf("prefix replace: %s", h)
	}
	if h := host("app_webshop-prod"); h != "acme_webshop-live.test" {
		t.Fatalf("prefix+suffix: %s", h)
	}
	if h := host("shop-prod"); h != "shop-live.test" {
		t.Fatalf("suffix only: %s", h)
	}
	// Empty rules: folder name as-is.
	a.SetHostRules(nil)
	if h := host("app_webshop"); h != "app_webshop.test" {
		t.Fatalf("no rules: %s", h)
	}
	// Rules causing collisions or invalid hostnames are rejected and the old rules kept.
	if err := a.SetHostRules([]HostRule{{"suffix", "-prod", ""}, {"prefix", "app_", ""}}); err == nil || !strings.Contains(err.Error(), "webshop.test") {
		t.Fatalf("collision accepted: %v", err)
	}
	if err := a.SetHostRules([]HostRule{{"prefix", "app_", "-"}}); err == nil {
		t.Fatal("invalid result hostname accepted")
	}
	if err := a.SetHostRules([]HostRule{{"middle", "x", ""}}); err == nil {
		t.Fatal("bad kind accepted")
	}
	if len(a.cfg.HostRules) != 0 {
		t.Fatalf("rejected rules changed config: %v", a.cfg.HostRules)
	}
}

func TestRewritePath(t *testing.T) {
	// php pinned and on PATH (swapped in place), go pinned but not on PATH (appended),
	// nodejs on PATH but not pinned (kept), python neither (not added).
	tmp := t.TempDir()
	a := &App{cfg: Config{Root: tmp, Versions: map[string]string{"php": "php-8.4", "go": "go1.26.5"}}}
	for _, d := range []string{`bin\php\php-8.4`, `bin\php\php-8.5`, `bin\go\go1.26.5`, `bin\nodejs\node-v22`} {
		os.MkdirAll(filepath.Join(tmp, d), 0755)
	}
	b := func(rel string) string { return filepath.Join(tmp, rel) }
	old := strings.Join([]string{`C:\Windows`, b(`bin\php\php-8.5`), `C:\Tools`, b(`bin\nodejs\node-v22`), ""}, ";")
	got := a.rewritePath(old)
	want := strings.Join([]string{`C:\Windows`, b(`bin\php\php-8.4`), `C:\Tools`, b(`bin\nodejs\node-v22`), b(`bin\go\go1.26.5\bin`)}, ";")
	if got != want {
		t.Fatalf("rewritePath:\n got  %s\n want %s", got, want)
	}
	if again := a.rewritePath(got); again != got {
		t.Fatalf("not idempotent:\n%s", again)
	}
}

func TestSources(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "usr"), 0755)
	os.WriteFile(filepath.Join(root, `usr\packages.conf`), []byte("PHP-8.4=https://x/php-8.4.5.zip\n"), 0644)
	src := filepath.Join(root, "extra.conf")
	os.WriteFile(src, []byte("# extra\nphp-8.4=https://dup/x.zip\nredis-7.2=https://x/redis-7.2.zip\n"), 0644)
	a := &App{cfg: Config{Root: root}, cfgPath: filepath.Join(root, "devpanel.json")}

	if err := a.AddPackage("my-tool", "https://x/tool.zip", "python"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]string{{"", "https://x/a.zip", ""}, {"a/b", "https://x/a.zip", ""}, {"ok", "ftp://x", ""}, {"ok2", "https://x/a.zip", "nope"}, {"PHP-8.4", "https://x/a.zip", ""}} {
		if err := a.AddPackage(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("AddPackage%v accepted", bad)
		}
	}
	if err := a.AddSource(src); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSource(filepath.Join(root, "missing.conf")); err == nil {
		t.Fatal("missing source accepted")
	}
	pl := a.Packages()
	got := map[string]string{}
	for _, p := range nonBuiltin(pl.Packages) {
		got[p.Name] = p.Source + "|" + p.Group + "|" + p.Folder
	}
	want := map[string]string{
		"PHP-8.4":   `Laragon|bin\php|php-8.4.5`,
		"my-tool":   `Custom|bin\python|tool`,
		"redis-7.2": src + `|bin\redis|redis-7.2`,
	}
	if !maps.Equal(got, want) || len(pl.Errors) != 0 {
		t.Fatalf("Packages = %v errors=%v", got, pl.Errors)
	}
	a.RemovePackage("my-tool")
	a.RemoveSource(src)
	if n := len(nonBuiltin(a.Packages().Packages)); n != 1 {
		t.Fatalf("after remove: %d packages", n)
	}
}

func TestEditCatalogGuard(t *testing.T) {
	a := &App{cfg: Config{Root: t.TempDir(), Sources: []string{"https://x/cat.conf"}}}
	for _, f := range []string{`C:\Windows\System32\drivers\etc\hosts`, "https://x/cat.conf", `..\usr\packages.conf`} {
		if err := a.EditCatalog(f); err == nil {
			t.Errorf("EditCatalog(%q) allowed", f)
		}
	}
}

func TestPHPExtToggle(t *testing.T) {
	ini := "; docs\r\n;   extension=mysqli\r\n;extension=bz2\r\nextension=curl\r\n;extension=pgsql\r\nextension=\"php_gd.dll\"\r\n"
	if extEnabled(ini, "mysqli") || !extEnabled(ini, "curl") || !extEnabled(ini, "gd") || extEnabled(ini, "pgsql") {
		t.Fatal("extEnabled wrong")
	}
	on := setExt(ini, "pgsql", true)
	if !strings.Contains(on, "\r\nextension=pgsql\r\n") || !extEnabled(on, "pgsql") {
		t.Fatalf("enable pgsql:\n%q", on)
	}
	// Doc comment untouched, and enabling mysqli appends a real line instead of uncommenting docs.
	on = setExt(ini, "mysqli", true)
	if !strings.Contains(on, ";   extension=mysqli\r\n") || !strings.HasSuffix(on, "extension=mysqli\r\n") || !extEnabled(on, "mysqli") {
		t.Fatalf("enable mysqli:\n%q", on)
	}
	off := setExt(ini, "gd", false)
	if extEnabled(off, "gd") || !strings.Contains(off, ";extension=gd\r\n") {
		t.Fatalf("disable gd:\n%q", off)
	}
	if z := setExt("x\n", "opcache", true); z != "x\nzend_extension=opcache\r\n" {
		t.Fatalf("opcache: %q", z)
	}
	if setExt(ini, "curl", true) != ini {
		t.Fatal("enabling an enabled ext should not change the file")
	}
}

func TestDetailGuard(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\mysql\mysql-8.4\bin`), 0755)
	os.WriteFile(filepath.Join(root, `bin\mysql\mysql-8.4\my.ini`), []byte("[mysqld]\r\ndatadir=\"C:/x/data/mysql-8.4\"\r\n"), 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	d, err := a.Detail("mysql")
	if err != nil || d.Info[1][1] != filepath.Clean(`C:/x/data/mysql-8.4`) {
		t.Fatalf("mysql detail: %v %v", d.Info, err)
	}
	if !a.allowed(filepath.Join(root, `bin\mysql\mysql-8.4\my.ini`), false) {
		t.Fatal("my.ini should be allowed")
	}
	for _, p := range []string{`C:\Windows\win.ini`, filepath.Join(root, `bin\mysql\mysql-8.4\my.ini\..\..\x`)} {
		if a.allowed(p, false) {
			t.Errorf("%s allowed", p)
		}
	}
	if _, err := a.TailFile(`C:\Windows\win.ini`, 10); err == nil {
		t.Fatal("TailFile outside services allowed")
	}
	if _, err := a.Detail("../x"); err == nil {
		t.Fatal("unknown service accepted")
	}
}

func TestSSL(t *testing.T) {
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, TLD: "test"}}
	changed, err := a.writeLeaf([]string{"webshop.test", "l13.local"})
	if err != nil || !changed {
		t.Fatalf("first issue: %v %v", changed, err)
	}
	caCrt, caKey, crt, _ := a.sslFiles()
	ca, _ := readCert(caCrt)
	leaf, _ := readCert(crt)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, h := range []string{"webshop.test", "api.webshop.test", "localhost"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: h}); err != nil {
			t.Errorf("verify %s: %v", h, err)
		}
	}
	if slices.Contains(leaf.DNSNames, "l13.local") {
		t.Error("host outside .test must be skipped (CA name constraints)")
	}
	// The CA itself must refuse to vouch for real domains.
	if !ca.PermittedDNSDomainsCritical || !slices.Equal(ca.PermittedDNSDomains, []string{"test", "localhost"}) {
		t.Fatalf("CA constraints: %v", ca.PermittedDNSDomains)
	}
	if st, _ := os.Stat(caKey); st == nil {
		t.Fatal("CA key missing")
	}
	// Same hosts in another order -> no reissue; new host -> reissue with the same CA.
	if changed, _ := a.writeLeaf([]string{"l13.local", "webshop.test"}); changed {
		t.Fatal("reissued without changes")
	}
	if changed, _ := a.writeLeaf([]string{"webshop.test", "new.test"}); !changed {
		t.Fatal("not reissued for new host")
	}
	if ca2, _ := readCert(caCrt); !ca2.Equal(ca) {
		t.Fatal("CA regenerated")
	}
}

func TestDBGuards(t *testing.T) {
	for _, bad := range []string{"", "a;drop", "a`b", "x y", "postgres", "Template1", strings.Repeat("a", 64)} {
		if checkName("postgresql", bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, bad := range []string{"mysql", "information_schema"} {
		if checkName("mysql", bad) == nil {
			t.Errorf("mysql accepted %q", bad)
		}
	}
	if checkName("postgresql", "acme_dev-2") != nil {
		t.Error("rejected valid name")
	}
	a := &App{cfg: Config{Root: t.TempDir()}}
	if err := a.DropDatabase("postgresql", "shop", "shp"); err == nil || !strings.Contains(err.Error(), "nothing dropped") {
		t.Fatalf("drop without matching confirmation: %v", err)
	}
	if _, err := a.RestoreDatabase("mysql", "shop", "x.sql", ""); err == nil || !strings.Contains(err.Error(), "nothing restored") {
		t.Fatalf("restore without confirmation: %v", err)
	}
	if _, err := a.dbBin("oracle", "x"); err == nil {
		t.Fatal("unknown engine accepted")
	}
}

func TestProcesses(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	for _, bad := range []Proc{{"", "shop", "x", false}, {"q", "../x", "x", false}, {"q", "missing", "x", false}, {"q", "shop", " ", false}} {
		if a.SaveProcess(bad) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := a.SaveProcess(Proc{"hello", "shop", "echo hi from %CD% & ping -n 30 127.0.0.1 >nul", false}); err != nil {
		t.Fatal(err)
	}
	if err := a.StartProcess("hello"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if st := a.Processes(); !st[0].Running || st[0].PID == 0 {
		t.Fatalf("not running: %+v", st)
	}
	a.StopProcess("hello")
	if st := a.Processes(); st[0].Running {
		t.Fatal("still running after stop")
	}
	if l := a.ProcessLog("hello"); !strings.Contains(l, "hi from") || !strings.Contains(l, `www\shop`) || !strings.Contains(l, "[exited") {
		t.Fatalf("log: %q", l)
	}
	r := &ringLog{}
	r.Write(make([]byte, 70<<10))
	if len(r.String()) != 64<<10 {
		t.Fatal("ring log not capped")
	}
}

func TestTrayIcon(t *testing.T) {
	ico := trayIcon()
	if len(ico) < 30 || ico[2] != 1 || ico[4] != 1 || string(ico[23:26]) != "PNG" {
		t.Fatalf("bad ICO header % x", ico[:26])
	}
}

// nonBuiltin drops DevPanel's own download links so tests can count catalog entries.
func nonBuiltin(ps []Package) []Package {
	return slices.DeleteFunc(slices.Clone(ps), func(p Package) bool { return p.Source == "Built-in" })
}
