package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSiteProxyConfigs(t *testing.T) {
	defer setPortOverrides(nil)
	setPortOverrides(map[string]int{"apache": freePort(t), "nginx": freePort(t)}) // real :80 belongs to the real Apache
	root := t.TempDir()
	for _, d := range []string{`www\vite\public`, `www\shop\public`, `etc\apache2\sites-enabled`, `bin\nginx\nginx-1\conf`, `bin\caddy\caddy-2`} {
		os.MkdirAll(filepath.Join(root, d), 0755)
	}
	a := &App{cfg: Config{Root: root, TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(root, "devpanel.json")}
	for _, bad := range []string{"ftp://x", "http://", "localhost:3000/path"} {
		if a.SetSiteProxy("vite", bad) == nil {
			t.Errorf("proxy %q accepted", bad)
		}
	}
	if err := a.SetSiteProxy("vite", "127.0.0.1:5173"); err != nil {
		t.Fatal(err)
	}
	if a.cfg.SiteProxy["vite"] != "http://127.0.0.1:5173" {
		t.Fatalf("normalized target: %q", a.cfg.SiteProxy["vite"])
	}
	ap, _ := os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\auto.vite.test.conf`))
	if strings.Count(string(ap), "ProxyPass / http://127.0.0.1:5173/ upgrade=websocket") != 2 || !strings.Contains(string(ap), "<IfModule proxy_module>") {
		t.Fatalf("apache proxy:\n%s", ap)
	}
	shop, _ := os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\auto.shop.test.conf`))
	if strings.Contains(string(shop), "ProxyPass") || strings.Contains(string(shop), "{{") {
		t.Fatal("proxy leaked into another site or placeholder left")
	}
	if !exists(filepath.Join(root, `etc\apache2\devpanel-modules.conf`)) {
		t.Fatal("proxy modules config not written")
	}
	ng, _ := os.ReadFile(a.nginxConfPath())
	if !strings.Contains(string(ng), "proxy_pass http://127.0.0.1:5173;") || !strings.Contains(string(ng), `proxy_set_header Upgrade $http_upgrade;`) || strings.Contains(string(ng), "{{") {
		t.Fatalf("nginx proxy:\n%s", ng)
	}
	cd, _ := os.ReadFile(a.caddyConfPath("caddy"))
	c := string(cd)
	if !strings.Contains(c, "reverse_proxy http://127.0.0.1:5173") || !strings.Contains(c, "php_fastcgi 127.0.0.1:9100") || !strings.Contains(c, "auto_https off") {
		t.Fatalf("caddy:\n%s", c)
	}
	// Turning the proxy off restores file/PHP serving.
	a.SetSiteProxy("vite", "")
	if ap, _ = os.ReadFile(filepath.Join(root, `etc\apache2\sites-enabled\auto.vite.test.conf`)); strings.Contains(string(ap), "ProxyPass") {
		t.Fatal("proxy not removed")
	}
}

func TestApacheModulesInclude(t *testing.T) {
	root := t.TempDir()
	conf := filepath.Join(root, `bin\apache\httpd-x\conf\httpd.conf`)
	os.MkdirAll(filepath.Dir(conf), 0755)
	sites := filepath.ToSlash(filepath.Join(root, `etc\apache2\sites-enabled`))
	os.WriteFile(conf, []byte("Listen 80\r\nIncludeOptional \""+sites+"/*.conf\"\r\n"), 0644)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	if err := a.ensureApacheModules(); err != nil {
		t.Fatal(err)
	}
	a.ensureApacheModules() // idempotent
	b, _ := os.ReadFile(conf)
	s := string(b)
	if strings.Count(s, "devpanel-modules.conf") != 1 || strings.Index(s, "devpanel-modules.conf") > strings.Index(s, "sites-enabled") {
		t.Fatalf("modules include must come once, before sites-enabled:\n%s", s)
	}
}

func TestWebServerFamily(t *testing.T) {
	if !portTwins("apache", "caddy") || !portTwins("nginx_ssl", "frankenphp_ssl") || portTwins("apache", "caddy_ssl") || portTwins("apache", "mysql") {
		t.Fatal("portTwins")
	}
	defer setPortOverrides(nil)
	setPortOverrides(map[string]int{"apache": freePort(t), "caddy": freePort(t), "nginx": freePort(t)})
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\nginx\n1`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\caddy\c1`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}, Services: []string{"nginx", "caddy"}}}
	note, _ := a.StartAll()
	if !strings.Contains(note, "Caddy skipped (Nginx uses the same port") {
		t.Fatalf("note = %q", note)
	}
	if startTimeout("elasticsearch") <= startTimeout("redis") {
		t.Fatal("JVM services need a longer start timeout")
	}
}

func TestRabbitNeedsErlangAndDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, `bin\rabbitmq\rabbitmq_server-4.4.0\sbin`), 0755)
	a := &App{cfg: Config{Root: root, Versions: map[string]string{}}}
	if ok, err := a.startExtra("rabbitmq"); !ok || err == nil || !strings.Contains(err.Error(), "Erlang") {
		t.Fatalf("rabbitmq without erlang: %v %v", ok, err)
	}
	os.MkdirAll(filepath.Join(root, `bin\erlang\otp-28`), 0755)
	if dirs := a.procDirs("rabbitmq"); len(dirs) != 2 {
		t.Fatalf("rabbitmq processes live in rabbitmq and erlang dirs: %v", dirs)
	}
	env := strings.Join(a.runtimeEnv(), ";")
	if !strings.Contains(env, "ERLANG_HOME="+filepath.Join(root, `bin\erlang\otp-28`)) {
		t.Fatalf("runtimeEnv: %s", env)
	}
	if ok, _ := a.startExtra("postgresql"); ok {
		t.Fatal("startExtra must not claim core services")
	}
}
