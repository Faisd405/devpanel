package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectRoot(t *testing.T) {
	lar := t.TempDir()
	os.WriteFile(filepath.Join(lar, "laragon.exe"), nil, 0644)
	os.MkdirAll(filepath.Join(lar, "devpanel"), 0755)
	if got := detectRoot(filepath.Join(lar, "devpanel")); got != lar {
		t.Fatalf("inside Laragon: %s", got)
	}
	own := filepath.Join(t.TempDir(), "devpanel")
	os.MkdirAll(own, 0755)
	if got := detectRoot(own); got != own {
		t.Fatalf("standalone: %s", got)
	}
}

func standaloneApp(t *testing.T) (*App, string) {
	root := t.TempDir()
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	a.ensureLayout()
	return a, root
}

func TestEnsureApacheConfigStandalone(t *testing.T) {
	a, root := standaloneApp(t)
	ap := filepath.Join(root, `bin\apache\httpd-2.4.69`)
	os.MkdirAll(filepath.Join(ap, "conf"), 0755)
	stock := "Define SRVROOT \"c:/Apache24\"\nListen 80\n"
	os.WriteFile(filepath.Join(ap, `conf\httpd.conf`), []byte(stock), 0644)
	php := filepath.Join(root, `bin\php\php-8.5.12-nts-Win32-vs17-x64`)
	os.MkdirAll(php, 0755)
	os.WriteFile(filepath.Join(php, "php-cgi.exe"), nil, 0644)
	os.WriteFile(a.fcgidModule(), []byte("x"), 0644) // present: no download in unit tests

	if a.CurrentPHP() != "php-8.5.12-nts-Win32-vs17-x64" {
		t.Fatalf("CurrentPHP without fcgid.conf should fall back to the newest PHP, got %q", a.CurrentPHP())
	}
	if err := a.ensureApacheConfig(ap); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(filepath.Join(ap, `conf\httpd.conf`))
	c := string(conf)
	for _, want := range []string{`Define SRVROOT "` + filepath.ToSlash(ap) + `"`, "sites-enabled/*.conf", "fcgid.conf", "devpanel-php.conf", "LoadModule ssl_module", "Listen 80"} {
		if !strings.Contains(c, want) {
			t.Errorf("generated httpd.conf lacks %q", want)
		}
	}
	if dist, _ := os.ReadFile(filepath.Join(ap, `conf\httpd.conf.dist`)); string(dist) != stock {
		t.Fatal("original httpd.conf not kept as .dist")
	}
	for _, f := range []string{`etc\apache2\httpd-ssl.conf`, `etc\apache2\sites-enabled\00-default.conf`, `etc\apache2\fcgid.conf`, `etc\ssl\devpanel.crt`} {
		if !exists(filepath.Join(root, f)) {
			t.Errorf("%s not created", f)
		}
	}
	fc, _ := os.ReadFile(a.path(`etc\apache2\fcgid.conf`))
	if !strings.Contains(string(fc), filepath.ToSlash(php)+"/php-cgi.exe") || a.CurrentPHP() != "php-8.5.12-nts-Win32-vs17-x64" {
		t.Fatalf("fcgid.conf: %s", fc)
	}
	// Idempotent, and an httpd.conf that already includes sites-enabled (Laragon's) is never replaced.
	laragonConf := "# Laragon\nIncludeOptional \"C:/laragon/etc/apache2/sites-enabled/*.conf\"\n"
	os.WriteFile(filepath.Join(ap, `conf\httpd.conf`), []byte(laragonConf), 0644)
	if err := a.ensureApacheConfig(ap); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ap, `conf\httpd.conf`)); string(b) != laragonConf {
		t.Fatal("Laragon's httpd.conf was modified")
	}
}

func TestTarExtractAndComposer(t *testing.T) {
	// .tar.gz and .7z made by Windows' own tar, extracted by DevPanel (no 7z.exe involved).
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, `ruby-4.0\bin`), 0755)
	os.WriteFile(filepath.Join(src, `ruby-4.0\bin\ruby.exe`), []byte("MZ"), 0644)
	a, _ := standaloneApp(t)
	for _, name := range []string{"x.tar.gz", "x.7z"} {
		arch := filepath.Join(t.TempDir(), name)
		args := []string{"-a", "-cf", arch, "-C", src, "ruby-4.0"}
		if name == "x.7z" {
			args = []string{"--format", "7zip", "-cf", arch, "-C", src, "ruby-4.0"}
		}
		if out, err := exec.Command(windowsTar(), args...).CombinedOutput(); err != nil {
			t.Fatalf("make %s: %v %s", name, err, out)
		}
		dest := filepath.Join(t.TempDir(), "out")
		if err := a.extractAny(arch, dest); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !exists(filepath.Join(dest, `bin\ruby.exe`)) {
			t.Fatalf("%s: top folder not flattened", name)
		}
	}

	phar := []byte("<?php // composer")
	sum := sha256.Sum256(phar)
	good := hex.EncodeToString(sum[:])
	sha := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256sum") {
			fmt.Fprint(w, sha+"  composer.phar")
			return
		}
		w.Write(phar)
	}))
	defer srv.Close()
	defer func(u string) { composerURL = u }(composerURL)
	composerURL = srv.URL + "/composer.phar"
	sha = strings.Repeat("0", 64)
	if err := a.ensureComposer(); err == nil || exists(a.composerPhar()) {
		t.Fatal("composer installed despite a wrong checksum")
	}
	sha = good
	if err := a.ensureComposer(); err != nil || !exists(a.composerPhar()) || !exists(filepath.Join(filepath.Dir(a.composerPhar()), "composer.bat")) {
		t.Fatalf("composer: %v", err)
	}
}

func TestStandaloneCatalogAndTools(t *testing.T) {
	a, root := standaloneApp(t)
	pl := a.Packages()
	if len(pl.Errors) != 0 {
		t.Fatalf("missing packages.conf must not be an error standalone: %v", pl.Errors)
	}
	have := map[string]bool{}
	for _, p := range pl.Packages {
		have[p.Name] = true
	}
	for _, want := range []string{"apache-2.4.69", "nginx-1.28.0", "mysql-8.4.3", "postgresql-17.2", "phpmyadmin-5.2.2", "redis-5.0.14.1", "ngrok-v3"} {
		if !have[want] {
			t.Errorf("built-in catalog lacks %s", want)
		}
	}
	if a.ngrokExe() != "" {
		t.Fatal("no ngrok installed yet")
	}
	os.MkdirAll(filepath.Join(root, `bin\ngrok\ngrok-v3`), 0755)
	os.WriteFile(filepath.Join(root, `bin\ngrok\ngrok-v3\ngrok.exe`), nil, 0644)
	if a.ngrokExe() != filepath.Join(root, `bin\ngrok\ngrok-v3\ngrok.exe`) {
		t.Fatalf("ngrokExe = %s", a.ngrokExe())
	}
	os.MkdirAll(filepath.Join(root, `etc\apps\phpMyAdmin`), 0755)
	if err := a.ensurePhpMyAdminAlias(); err != nil || !exists(filepath.Join(root, `etc\apache2\alias\phpmyadmin.conf`)) {
		t.Fatal("phpMyAdmin alias not created")
	}
	if len(a.webAliases()) != 1 {
		t.Fatal("Nginx must pick up the new alias too")
	}
}

func TestExtractKeepsLooseBinaries(t *testing.T) {
	// mod_fcgid layout: one docs folder + the module loose at the top -> nothing may be dropped.
	z := makeZip(t, map[string]string{"mod_fcgid-2.3.10/LICENSE-FCGID": "x", "ReadMe.txt": "r", "-- Win64 VS18 --": "", "mod_fcgid.so": "MZ"})
	dest := filepath.Join(t.TempDir(), "m")
	if err := extract(z, dest); err != nil || !exists(filepath.Join(dest, "mod_fcgid.so")) {
		t.Fatalf("loose binary dropped: %v", err)
	}
	// Apache layout: one folder + only docs loose -> unwrap.
	z = makeZip(t, map[string]string{"Apache24/bin/httpd.exe": "MZ", "ReadMe.txt": "r", "-- Win64 VS18 --": ""})
	dest = filepath.Join(t.TempDir(), "a")
	if err := extract(z, dest); err != nil || !exists(filepath.Join(dest, `bin\httpd.exe`)) {
		t.Fatalf("apache layout not unwrapped: %v", err)
	}
}
