package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWWWOverride(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(other, "shop"), 0755)
	a := &App{cfg: Config{Root: root, TLD: "test"}, cfgPath: filepath.Join(root, "devpanel.json")}
	if a.www() != filepath.Join(root, "www") {
		t.Fatal("default www")
	}
	for _, bad := range []string{"relative", filepath.Join(other, "missing")} {
		if a.SetWWW(bad) == nil {
			t.Errorf("SetWWW(%q) accepted", bad)
		}
	}
	if err := a.SetWWW(other); err != nil {
		t.Fatal(err)
	}
	if list, _ := a.sites(); len(list) != 1 || list[0].Folder != "shop" {
		t.Fatalf("sites from custom www: %+v", list)
	}
}

func TestMailCatch(t *testing.T) {
	defer setPortOverrides(nil)
	// Free ports for the web servers: the real Apache on :80 is not this sandbox's to restart.
	setPortOverrides(map[string]int{"mailpit_smtp": 1025, "apache": freePort(t), "nginx": freePort(t)})
	root := t.TempDir()
	for _, v := range []string{"php-8.4.5-nts-Win32-vs17-x64", "php-8.5.12-nts-Win32-vs17-x64"} {
		d := filepath.Join(root, `bin\php`, v)
		os.MkdirAll(d, 0755)
		os.WriteFile(filepath.Join(d, "php-cgi.exe"), nil, 0644)
		os.WriteFile(filepath.Join(d, "php.ini"), []byte("[mail function]\r\nSMTP = localhost\r\nsmtp_port = 25\r\n;sendmail_from = me@example.com\r\n"), 0644)
	}
	os.MkdirAll(filepath.Join(root, `etc\apache2`), 0755)
	os.WriteFile(filepath.Join(root, `etc\apache2\fcgid.conf`), []byte(`FcgidWrapper "C:/x/bin/php/php-8.5.12-nts-Win32-vs17-x64/php-cgi.exe" .php`), 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{"php": "php-8.5.12-nts-Win32-vs17-x64"}}}
	if a.MailCatch() {
		t.Fatal("not caught yet")
	}
	if _, err := a.SetMailCatch(true); err != nil {
		t.Fatal(err)
	}
	for _, v := range a.PHPVersions() {
		b, _ := os.ReadFile(filepath.Join(a.phpDir(v), "php.ini"))
		s := string(b)
		if iniGet(s, "SMTP") != "127.0.0.1" || iniGet(s, "smtp_port") != "1025" || iniGet(s, "sendmail_from") != "devpanel@localhost" {
			t.Fatalf("%s php.ini:\n%s", v, s)
		}
	}
	if !a.MailCatch() {
		t.Fatal("MailCatch should report on")
	}
	a.SetMailCatch(false)
	if a.MailCatch() {
		t.Fatal("still caught after off")
	}
}

func TestWPConfig(t *testing.T) {
	sample := "define( 'DB_NAME', 'database_name_here' );\ndefine( 'DB_USER', 'username_here' );\ndefine( 'DB_PASSWORD', 'password_here' );\ndefine( 'DB_HOST', 'localhost' );\n" +
		"define( 'AUTH_KEY', 'put your unique phrase here' );\ndefine( 'NONCE_SALT', 'put your unique phrase here' );\n"
	out := wpConfig(sample, "my_blog", 3307, "p'w")
	for _, want := range []string{"'my_blog'", "'root'", `'p\'w'`, "'127.0.0.1:3307'"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "put your unique phrase here") {
		t.Fatal("salts not replaced")
	}
	lines := strings.Split(out, "\n")
	if lines[4] == lines[5] || len(lines[4]) < 60 {
		t.Fatal("salts must be long and different")
	}
}

func TestRenameProjectAndKeys(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `www\shop`), 0755)
	os.MkdirAll(filepath.Join(root, `etc\apache2\sites-enabled`), 0755)
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{},
		Hostnames: map[string]string{"shop": "store.test"}, SiteRoots: map[string]string{"shop": "."},
		SiteAliases: map[string][]string{"shop": {"api.store.test"}}, SitePHP: map[string]string{},
		Procs: []Proc{{Name: "q", Dir: "shop", Cmd: "x"}}}, cfgPath: filepath.Join(root, "devpanel.json")}
	if _, err := a.RenameProject("shop", "bad name"); err == nil {
		t.Fatal("invalid name accepted")
	}
	if _, err := a.RenameProject("shop", "shop2"); err != nil && !strings.Contains(err.Error(), "apache") {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, `www\shop2`)) || exists(filepath.Join(root, `www\shop`)) {
		t.Fatal("folder not renamed")
	}
	if a.cfg.Hostnames["shop2"] != "store.test" || a.cfg.SiteRoots["shop2"] != "." || a.cfg.SiteAliases["shop2"][0] != "api.store.test" || a.cfg.Procs[0].Dir != "shop2" {
		t.Fatalf("settings not carried over: %+v", a.cfg)
	}
	if !exists(filepath.Join(root, `etc\apache2\sites-enabled\auto.store.test.conf`)) {
		t.Fatal("vhost not regenerated")
	}
	// Delete guards (the real delete goes to the Recycle Bin, not exercised here).
	if _, err := a.DeleteProject("shop2", "shop"); err == nil || !strings.Contains(err.Error(), "nothing deleted") {
		t.Fatalf("delete without matching confirmation: %v", err)
	}
	if _, err := a.DeleteProject("..", ".."); err == nil {
		t.Fatal("path traversal accepted")
	}
	a.moveSiteKeys("shop2", "")
	if len(a.cfg.Procs) != 0 || a.cfg.Hostnames["shop2"] != "" {
		t.Fatal("forgetting a project must drop its settings and processes")
	}
}

func TestVhostTemplates(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `www\shop\public`), 0755)
	os.MkdirAll(filepath.Join(root, `etc\apache2\sites-enabled`), 0755)
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	a.syncVhostFiles()
	conf := filepath.Join(root, `etc\apache2\sites-enabled\auto.shop.test.conf`)
	b, _ := os.ReadFile(conf)
	if strings.Contains(string(b), "{{") || strings.Contains(string(b), "Placeholders") || !strings.Contains(string(b), `define SITE "shop.test"`) {
		t.Fatalf("built-in template output:\n%s", b)
	}
	// A custom template is used for every site.
	os.WriteFile(a.apacheTplPath(), []byte(apacheTplHelp+"# custom {{HOST}} at {{ROOT}} on {{HTTP_PORT}}\n"), 0644)
	a.syncVhostFiles()
	b, _ = os.ReadFile(conf)
	if !strings.Contains(string(b), "# custom shop.test at "+filepath.ToSlash(filepath.Join(root, `www\shop\public`))+" on 80") {
		t.Fatalf("custom template not applied:\n%s", b)
	}
	if !a.CustomTemplates()["apache"] {
		t.Fatal("CustomTemplates")
	}
	a.ResetVhostTemplate("apache")
	a.syncVhostFiles()
	if b, _ = os.ReadFile(conf); strings.Contains(string(b), "# custom") {
		t.Fatal("reset did not restore the built-in template")
	}
	if fillTpl("{{A}}-{{A}}-{{B}}", map[string]string{"A": "x"}) != "x-x-{{B}}" {
		t.Fatal("fillTpl")
	}
}

func TestAliasesAndExe(t *testing.T) {
	root := t.TempDir()
	pma := filepath.Join(root, `etc\apps\phpMyAdmin`)
	os.MkdirAll(pma, 0755)
	os.MkdirAll(filepath.Join(root, `etc\apache2\alias`), 0755)
	os.WriteFile(filepath.Join(root, `etc\apache2\alias\phpmyadmin.conf`), []byte(`Alias /phpmyadmin "`+filepath.ToSlash(pma)+`/"`+"\n<Directory x>\n</Directory>\n"), 0644)
	os.WriteFile(filepath.Join(root, `etc\apache2\alias\adminer.conf`), []byte(`Alias /adminer "C:/does/not/exist/"`), 0644)
	a := &App{cfg: Config{Root: root}}
	got := a.webAliases()
	if len(got) != 1 || got[0][0] != "/phpmyadmin" || got[0][1] != filepath.ToSlash(pma) {
		t.Fatalf("webAliases = %v", got)
	}

	d := filepath.Join(t.TempDir(), "memcached-1.6.8-win64-mingw")
	os.MkdirAll(filepath.Join(d, "bin"), 0755)
	os.WriteFile(filepath.Join(d, `bin\memcached.exe`), nil, 0644)
	if findExe(d, "memcached.exe") != filepath.Join(d, `bin\memcached.exe`) || findExe(d, "nope.exe") != "" {
		t.Fatal("findExe")
	}
	if g, f := pkgTarget("mariadb-11.4", "https://x/mariadb-11.4.4-winx64.zip", ""); g != `bin\mysql` || f != "mariadb-11.4.4-winx64" {
		t.Fatalf("mariadb target: %s %s", g, f)
	}
	if g, _ := pkgTarget("mongodb-8", "https://x/mongodb-windows-x86_64-8.0.4.zip", ""); g != `bin\mongodb` {
		t.Fatalf("mongodb target: %s", g)
	}
	if !slices.Contains(a.AppTemplates(), "wordpress") {
		t.Fatal("templates")
	}
}
