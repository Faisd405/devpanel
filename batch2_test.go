package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPorts(t *testing.T) {
	defer setPortOverrides(nil)
	root := t.TempDir()
	ap := filepath.Join(root, `bin\apache\httpd-2.4\conf`)
	os.MkdirAll(ap, 0755)
	os.MkdirAll(filepath.Join(root, `etc\apache2\sites-enabled`), 0755)
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\mysql\mysql-8.4`), 0755)
	os.WriteFile(filepath.Join(ap, "httpd.conf"), []byte("#Listen 12.34.56.78:80\r\nListen 80\r\n"), 0644)
	os.WriteFile(filepath.Join(root, `etc\apache2\sites-enabled\00-default.conf`), []byte("<VirtualHost _default_:80>\n</VirtualHost>\n<VirtualHost _default_:443>\n  SSLEngine on\n</VirtualHost>\n"), 0644)
	os.WriteFile(filepath.Join(root, `bin\mysql\mysql-8.4\my.ini`), []byte("[client]\nport=3306\n[mysqld]\nport=3306\n"), 0644)
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}

	p := 0
	for q := 18080; q < 18200 && p == 0; q++ {
		if !portOpen(q) {
			p = q
		}
	}
	if p == 0 {
		t.Skip("no free port")
	}
	// The real Apache on this machine may own :80; start from a free port so "service stopped" holds.
	setPortOverrides(map[string]int{"apache": p + 1, "mysql": p + 2})
	if err := a.SetPort("apache", p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(ap, "httpd.conf"))
	d, _ := os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\00-default.conf`))
	v, _ := os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\auto.shop.test.conf`))
	ps := strconv.Itoa(p)
	if !strings.Contains(string(b), "\nListen "+ps+"\r\n") || !strings.Contains(string(b), "#Listen 12.34.56.78:80") {
		t.Fatalf("httpd.conf: %q", b)
	}
	if !strings.Contains(string(d), "_default_:"+ps+">") || !strings.Contains(string(d), "_default_:443>") {
		t.Fatalf("00-default: %q", d)
	}
	if !strings.Contains(string(v), "<VirtualHost *:"+ps+">") || !strings.Contains(string(v), "<VirtualHost *:443>") {
		t.Fatalf("vhost: %s", v)
	}
	if svc("apache").Port != p || port("apache") != p {
		t.Fatal("override not applied")
	}
	if err := a.SetPort("mysql", 3307); err != nil {
		t.Fatal(err)
	}
	if m, _ := os.ReadFile(filepath.Join(root, `bin\mysql\mysql-8.4\my.ini`)); strings.Count(string(m), "port=3307") != 2 {
		t.Fatalf("my.ini: %s", m)
	}
	if !slices.Contains(myArgs(), "-P3307") {
		t.Fatalf("mysql client args: %v", myArgs())
	}
	for _, bad := range []struct {
		k string
		p int
	}{{"apache", 0}, {"apache", 70000}, {"redis", 3307}, {"nope", 1234}} {
		if a.SetPort(bad.k, bad.p) == nil {
			t.Errorf("SetPort(%s,%d) accepted", bad.k, bad.p)
		}
	}
}

func TestEnvAndIni(t *testing.T) {
	env := "APP_URL=http://localhost\r\n# DB_HOST=127.0.0.1\r\nDB_CONNECTION=sqlite\r\n"
	env = setEnv(env, "APP_URL", "http://shop.test")
	env = setEnv(env, "DB_HOST", "127.0.0.1")
	env = setEnv(env, "DB_CONNECTION", "pgsql")
	env = setEnv(env, "DB_DATABASE", "shop")
	want := "APP_URL=http://shop.test\r\nDB_HOST=127.0.0.1\r\nDB_CONNECTION=pgsql\r\nDB_DATABASE=shop\n"
	if env != want {
		t.Fatalf("setEnv:\n%q\nwant\n%q", env, want)
	}
	if dbNameFor("My-Shop") != "my_shop" {
		t.Fatal("dbNameFor")
	}

	ini := "; memory_limit = 1G is an example\r\nmemory_limit = 128M\r\n;date.timezone =\r\nmemory_limit=256M\r\n"
	if iniGet(ini, "memory_limit") != "128M" {
		t.Fatalf("iniGet: %q", iniGet(ini, "memory_limit"))
	}
	ini = iniSet(ini, "memory_limit", "512M")
	ini = iniSet(ini, "date.timezone", "Asia/Jakarta")
	ini = iniSet(ini, "max_input_vars", "5000")
	if !strings.Contains(ini, "memory_limit = 128M\r\n") || !strings.Contains(ini, "memory_limit = 512M\r\n") || !strings.Contains(ini, "\r\ndate.timezone = Asia/Jakarta\r\n") || !strings.HasSuffix(ini, "max_input_vars = 5000\r\n") {
		t.Fatalf("iniSet:\n%q", ini)
	}
}

func TestSiteExtras(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0755)
		os.WriteFile(filepath.Join(root, rel), []byte(body), 0644)
	}
	mk(`www\shop\artisan`, "")
	mk(`www\shop\.env`, "APP_URL=http://app_shop.test\n")
	mk(`www\winter\artisan`, "")
	mk(`www\winter\modules\system\x`, "")
	mk(`www\ci\spark`, "")
	mk(`www\wp\wp-login.php`, "")
	mk(`www\site\web\index.php`, "")
	a := &App{cfg: Config{Root: root, TLD: "test", Hostnames: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	for dir, fw := range map[string]string{"shop": "Laravel", "winter": "Winter/October CMS", "ci": "CodeIgniter 4", "wp": "WordPress", "site": ""} {
		if got := detectFramework(filepath.Join(root, "www", dir)); got != fw {
			t.Errorf("%s: %q want %q", dir, got, fw)
		}
	}
	if !urlMatches("https://Shop.test:8443/x", "shop.test") || urlMatches("http://app_shop.test", "shop.test") {
		t.Fatal("urlMatches")
	}
	// Custom root + aliases, and aliases take part in duplicate checks.
	if err := a.SetSiteOptions("site", "web", []string{"api.site.test", "API.site.test", "api2"}); err != nil {
		t.Fatal(err)
	}
	list, _ := a.sites()
	i := slices.IndexFunc(list, func(s Site) bool { return s.Folder == "site" })
	if list[i].Root != filepath.Join(root, `www\site\web`) || !slices.Equal(list[i].Aliases, []string{"api.site.test", "api2.test"}) {
		t.Fatalf("site opts: %+v", list[i])
	}
	if a.SetSiteOptions("shop", "", []string{"api2"}) == nil {
		t.Fatal("alias colliding with another site accepted")
	}
	for _, bad := range []string{"../x", `C:\x`, "missing"} {
		if a.SetSiteOptions("site", bad, nil) == nil {
			t.Errorf("root %q accepted", bad)
		}
	}
	os.MkdirAll(filepath.Join(root, `etc\apache2\sites-enabled`), 0755)
	if _, _, err := a.syncVhostFiles(); err != nil {
		t.Fatal(err)
	}
	v, _ := os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\auto.site.test.conf`))
	if !strings.Contains(string(v), "ServerAlias *.${SITE} api.site.test *.api.site.test") {
		t.Fatalf("alias not in vhost:\n%s", v)
	}
	leaf, _ := readCert(filepath.Join(root, `etc\ssl\devpanel.crt`))
	if leaf == nil || !slices.Contains(leaf.DNSNames, "api.site.test") {
		t.Fatal("alias missing from certificate")
	}
	// FixAppURL rewrites only APP_URL.
	if _, err := a.FixAppURL("shop"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, `www\shop\.env`)); !strings.Contains(string(b), "://shop.test") {
		t.Fatalf(".env: %s", b)
	}

	// hosts cleanup: only marked lines for unused hosts go; Docker/manual lines stay.
	lines := []string{"127.0.0.1 kubernetes.docker.internal", "127.0.0.1      app_shop.test #laragon magic!", "127.0.0.1 shop.test #devpanel", "127.0.0.1 manual.test", "127.0.0.1 old.test #devpanel"}
	if got := staleHostLines(lines, []string{"shop.test"}); !slices.Equal(got, []int{1, 4}) {
		t.Fatalf("staleHostLines = %v", got)
	}
}

func TestArchivesAndUninstall(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0644)
		return p
	}
	for kind, b := range map[string][]byte{"zip": []byte("PK\x03\x04xx"), "xz": {0xFD, '7', 'z', 'X', 'Z', 0}, "gz": {0x1F, 0x8B, 8}, "7z": {'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}, "": []byte("<html>")} {
		if got := archiveKind(write("f-"+kind, b)); got != kind {
			t.Errorf("archiveKind(%s) = %q", kind, got)
		}
	}
	// flattenInto: a single top folder is unwrapped.
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, `phpMyAdmin-5\libraries`), 0755)
	os.WriteFile(filepath.Join(src, "README"), nil, 0644)
	if err := flattenInto(src, filepath.Join(dir, "dest")); err != nil || !exists(filepath.Join(dir, `dest\libraries`)) {
		t.Fatalf("flatten: %v", err)
	}

	root := t.TempDir()
	for _, d := range []string{`bin\nodejs\node-v20`, `bin\nodejs\node-v22`} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	a := &App{cfg: Config{Root: root, Versions: map[string]string{"nodejs": "node-v22"}}}
	if a.UninstallVersion("nodejs", "node-v22", "node-v22") == nil {
		t.Fatal("removed the selected version")
	}
	if a.UninstallVersion("nodejs", "node-v20", "nope") == nil || !exists(filepath.Join(root, `bin\nodejs\node-v20`)) {
		t.Fatal("removed without confirmation")
	}
	if a.UninstallVersion("nodejs", "..", "..") == nil || a.UninstallVersion("bogus", "x", "x") == nil {
		t.Fatal("bad input accepted")
	}
	if err := a.UninstallVersion("nodejs", "node-v20", "node-v20"); err != nil || exists(filepath.Join(root, `bin\nodejs\node-v20`)) {
		t.Fatalf("uninstall: %v", err)
	}
}

func TestImportAndCLI(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	a := &App{cfg: Config{Root: root, TLD: "test", Groups: map[string][]string{"Old": {"apache"}}, Versions: map[string]string{"php": "x"}, Ports: map[string]int{"mysql": 3307}}, cfgPath: filepath.Join(root, "devpanel.json")}
	in := filepath.Join(root, "in.json")
	os.WriteFile(in, []byte(`{"root":"D:\\elsewhere","groups":{"New":["redis"]},"hostRules":[{"kind":"prefix","from":"x_","to":""}],"ports":{"mysql":9999},"versions":{"php":"y"}}`), 0644)
	if _, err := a.importFrom(in); err != nil {
		t.Fatal(err)
	}
	if a.cfg.Root != root || a.cfg.Versions["php"] != "x" || a.cfg.Ports["mysql"] != 3307 || a.cfg.Groups["New"] == nil || a.cfg.Groups["Old"] != nil {
		t.Fatalf("import: %+v", a.cfg)
	}
	if b, _ := filepath.Glob(filepath.Join(root, "devpanel-before-import-*.json")); len(b) != 1 {
		t.Fatal("no backup before import")
	}
	os.WriteFile(in, []byte(`{"hostnames":{"shop":"BAD NAME"}}`), 0644)
	if _, err := a.importFrom(in); err == nil || a.cfg.Groups["New"] == nil {
		t.Fatal("invalid import accepted or config not restored")
	}

	var out strings.Builder
	if code := a.runCLI(&out, []string{"groups"}); code != 0 || !strings.Contains(out.String(), "New") {
		t.Fatalf("groups: %d %q", code, out.String())
	}
	out.Reset()
	if code := a.runCLI(&out, []string{"start", "nothing"}); code != 1 || !strings.Contains(out.String(), "no service or group") {
		t.Fatalf("start unknown: %d %q", code, out.String())
	}
	out.Reset()
	if code := a.runCLI(&out, []string{"frobnicate"}); code != 2 || !strings.Contains(out.String(), "devpanel status") {
		t.Fatalf("help: %d %q", code, out.String())
	}
}
