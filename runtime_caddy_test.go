package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cachedDownload keeps big test downloads in %TEMP%\dp-dl between runs.
func cachedDownload(t *testing.T, a *App, name, url string) string {
	dir := filepath.Join(os.TempDir(), "dp-dl")
	os.MkdirAll(dir, 0755)
	f := filepath.Join(dir, name)
	if exists(f) {
		return f
	}
	tmp, err := a.download(name, url)
	if err != nil {
		t.Skip("download failed: ", err)
	}
	if err := os.Rename(tmp, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func installInto(t *testing.T, a *App, file, dest string) {
	os.MkdirAll(filepath.Dir(dest), 0755)
	if err := a.extractAny(file, dest); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCaddy(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	a, tmp := sandbox(t)
	zip := cachedDownload(t, a, "caddy.zip", "https://github.com/caddyserver/caddy/releases/download/v2.11.9/caddy_2.11.9_windows_amd64.zip")
	installInto(t, a, zip, filepath.Join(tmp, `bin\caddy\caddy-2.11.9`))

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "proxied app, host="+r.Host) }))
	defer app.Close()
	os.MkdirAll(filepath.Join(tmp, `www\vite`), 0755)
	a.cfg.SiteProxy = map[string]string{"vite": app.URL}
	setPortOverrides(map[string]int{"caddy": freePort(t), "caddy_ssl": freePort(t), "php_fastcgi": 20100 + os.Getpid()%400*2})

	if err := a.startCaddy("caddy"); err != nil {
		b, _ := os.ReadFile(a.caddyConfPath("caddy"))
		t.Fatalf("%v\n%s", err, b)
	}
	defer a.stop("caddy")
	base := fmt.Sprintf("http://127.0.0.1:%d", port("caddy"))
	v84, def, px := get(t, base+"/v.php", "v84.test"), get(t, base+"/v.php", "def.test"), get(t, base+"/", "vite.test")
	t.Logf("caddy v84.test: %s | def.test: %s | vite.test: %s", v84, def, px)
	if !strings.HasPrefix(v84, "200 8.4.") || !strings.HasPrefix(def, "200 8.5.") || !strings.Contains(px, "proxied app, host=vite.test") {
		t.Fatal("caddy routing wrong")
	}
	a.stopExtra("caddy")
	if a.running("caddy") {
		t.Fatal("caddy still running after stop")
	}
}

func TestRuntimeFrankenPHPConfig(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	a, tmp := sandbox(t)
	zip := cachedDownload(t, a, "frankenphp.zip", "https://github.com/php/frankenphp/releases/download/v1.13.4/frankenphp-windows-x86_64.zip")
	installInto(t, a, zip, filepath.Join(tmp, `bin\frankenphp\frankenphp-1.13.4`))
	if !exists(a.caddyExe("frankenphp")) {
		m, _ := filepath.Glob(filepath.Join(tmp, `bin\frankenphp\frankenphp-1.13.4\*`))
		t.Fatalf("frankenphp.exe not where expected; got %v", m)
	}
	setPortOverrides(map[string]int{"frankenphp": freePort(t), "frankenphp_ssl": freePort(t)})
	list, _ := a.sites()
	a.writeLeaf(allHosts(list))
	if _, err := a.writeCaddyConf("frankenphp", list); err != nil {
		t.Fatal(err)
	}
	if err := a.validateCaddy("frankenphp"); err != nil {
		t.Fatal(err)
	}
	if err := a.startCaddy("frankenphp"); err != nil {
		t.Fatal(err)
	}
	defer a.stop("frankenphp") // waits until the port is closed and the exe is released
	r := get(t, fmt.Sprintf("http://127.0.0.1:%d/v.php", port("frankenphp")), "def.test")
	t.Logf("frankenphp def.test: %s", r)
	if !strings.HasPrefix(r, "200 8.") {
		t.Fatal("frankenphp did not run PHP")
	}
}
