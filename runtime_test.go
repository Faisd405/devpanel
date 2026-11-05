package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Runtime checks against the real Apache/Nginx/PHP binaries, but with throwaway config in a temp dir,
// random ports, and processes stopped by their own PID only. Run with: DEVPANEL_RUNTIME=1 go test -run Runtime
func runtimeOnly(t *testing.T) {
	if os.Getenv("DEVPANEL_RUNTIME") != "1" {
		t.Skip("set DEVPANEL_RUNTIME=1")
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func killTree(pid int) { exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run() }

func get(t *testing.T, url, host string) string {
	req, _ := http.NewRequest("GET", url, nil)
	req.Host = host
	c := http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 30; i++ {
		resp, err := c.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return fmt.Sprintf("%d %s", resp.StatusCode, b)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no response from %s", url)
	return ""
}

func junction(t *testing.T, link, target string) {
	os.MkdirAll(filepath.Dir(link), 0755)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
}

const realRoot = `C:\laragon`
const php84, php85 = "php-8.4.5-nts-Win32-vs17-x64", "php-8.5.12-nts-Win32-vs17-x64"

// sandbox: temp root with junctions to the real php/nginx folders and two sites.
func sandbox(t *testing.T) (*App, string) {
	tmp := t.TempDir()
	for _, v := range []string{php84, php85} {
		junction(t, filepath.Join(tmp, `bin\php`, v), filepath.Join(realRoot, `bin\php`, v))
	}
	junction(t, filepath.Join(tmp, `bin\nginx\nginx-1.27.3`), filepath.Join(realRoot, `bin\nginx\nginx-1.27.3`))
	for _, d := range []string{"v84", "def"} {
		os.MkdirAll(filepath.Join(tmp, "www", d), 0755)
		os.WriteFile(filepath.Join(tmp, "www", d, "v.php"), []byte("<?php echo PHP_VERSION, '|', extension_loaded('mbstring') ? 'mb' : 'no-mb';"), 0644)
	}
	os.MkdirAll(filepath.Join(tmp, `etc\apache2`), 0755)
	os.MkdirAll(filepath.Join(tmp, `etc\nginx`), 0755)
	os.WriteFile(filepath.Join(tmp, `etc\apache2\fcgid.conf`), []byte(`FcgidWrapper "C:/laragon/bin/php/`+php85+`/php-cgi.exe" .php`), 0644)
	a := &App{cfg: Config{Root: tmp, TLD: "test", Versions: map[string]string{"php": php85}, SitePHP: map[string]string{"v84": php84}}}
	return a, tmp
}

func TestRuntimeApachePerSitePHP(t *testing.T) {
	runtimeOnly(t)
	a, tmp := sandbox(t)
	list, err := a.sites()
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(tmp, "devpanel-php.conf")
	os.WriteFile(conf, []byte(a.apachePHPConf(list)), 0644)
	// apachePHPConf points at tmp\bin\php (junction); fine for Apache too.
	ap := filepath.Join(realRoot, `bin\apache\httpd-2.4.62-240904-win64-VS17`)
	p := freePort(t)
	fwd := filepath.ToSlash
	httpd := fmt.Sprintf(`ServerRoot "%s"
Listen 127.0.0.1:%d
ServerName localhost
LoadModule authz_core_module modules/mod_authz_core.so
LoadModule dir_module modules/mod_dir.so
LoadModule mime_module modules/mod_mime.so
PidFile "%s/httpd.pid"
ErrorLog "%s/error.log"
DocumentRoot "%s/www"
<Directory "%s/www">
    Require all granted
</Directory>
Include "%s/etc/apache2/fcgid.conf"
Include "%s"
`, fwd(ap), p, fwd(tmp), fwd(tmp), fwd(tmp), fwd(tmp), fwd(realRoot), fwd(conf))
	cf := filepath.Join(tmp, "httpd.conf")
	os.WriteFile(cf, []byte(httpd), 0644)
	if out, err := exec.Command(filepath.Join(ap, `bin\httpd.exe`), "-t", "-f", cf).CombinedOutput(); err != nil {
		t.Fatalf("httpd -t: %v\n%s\n%s", err, out, a.apachePHPConf(list))
	}
	cmd := exec.Command(filepath.Join(ap, `bin\httpd.exe`), "-f", cf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killTree(cmd.Process.Pid)

	base := fmt.Sprintf("http://127.0.0.1:%d", p)
	v84, def := get(t, base+"/v84/v.php", "localhost"), get(t, base+"/def/v.php", "localhost")
	t.Logf("v84 site: %s | default site: %s", v84, def)
	if !strings.HasPrefix(v84, "200 8.4.") || !strings.HasPrefix(def, "200 8.5.") || !strings.Contains(v84, "|mb") {
		b, _ := os.ReadFile(filepath.Join(tmp, "error.log"))
		t.Fatalf("per-site PHP not applied\n%s", b)
	}
}

func TestRuntimeNginx(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	a, tmp := sandbox(t)
	setPortOverrides(map[string]int{"nginx": freePort(t), "nginx_ssl": freePort(t), "php_fastcgi": 19100 + int(time.Now().Unix()%500)*2})
	// A Laragon-style alias (/pma -> folder) must work on Nginx too.
	pma := filepath.Join(tmp, `apps\pma`)
	os.MkdirAll(pma, 0755)
	os.WriteFile(filepath.Join(pma, "index.php"), []byte("<?php echo 'pma ok ', PHP_VERSION;"), 0644)
	os.MkdirAll(filepath.Join(tmp, `etc\apache2\alias`), 0755)
	os.WriteFile(filepath.Join(tmp, `etc\apache2\alias\pma.conf`), []byte(`Alias /pma "`+filepath.ToSlash(pma)+`/"`), 0644)
	list, _ := a.sites()
	if _, err := a.writeLeaf(allHosts(list)); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(tmp, "ngx")
	for _, d := range []string{"logs", `temp\client_body_temp`, `temp\fastcgi_temp`, `temp\proxy_temp`, `temp\uwsgi_temp`, `temp\scgi_temp`} {
		os.MkdirAll(filepath.Join(prefix, d), 0755)
	}
	conf := filepath.Join(tmp, "nginx.conf")
	os.WriteFile(conf, []byte(a.nginxConf(list)), 0644)
	nginx := filepath.Join(realRoot, `bin\nginx\nginx-1.27.3\nginx.exe`)
	if out, err := exec.Command(nginx, "-t", "-p", prefix, "-c", conf).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t: %v\n%s", err, out)
	}
	if err := a.startPHPPools(list); err != nil {
		t.Fatal(err)
	}
	defer a.stopPHPPools()
	cmd := exec.Command(nginx, "-p", prefix, "-c", conf)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer killTree(cmd.Process.Pid)

	base := fmt.Sprintf("http://127.0.0.1:%d", port("nginx"))
	v84, def := get(t, base+"/v.php", "v84.test"), get(t, base+"/v.php", "def.test")
	t.Logf("nginx v84.test: %s | def.test: %s", v84, def)
	if !strings.HasPrefix(v84, "200 8.4.") || !strings.HasPrefix(def, "200 8.5.") || !strings.Contains(def, "|mb") {
		b, _ := os.ReadFile(filepath.Join(prefix, `logs\error.log`))
		t.Fatalf("nginx PHP routing wrong\n%s", b)
	}
	if n := get(t, base+"/missing.php", "def.test"); !strings.HasPrefix(n, "404") {
		t.Fatalf("missing php should 404, got %s", n)
	}
	if p := get(t, base+"/pma/", "localhost"); !strings.HasPrefix(p, "200 pma ok 8.5.") {
		t.Fatalf("alias /pma/: %s", p)
	}
	noRedirect := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := noRedirect.Get(base + "/pma"); err != nil || resp.StatusCode != 301 {
		t.Fatalf("/pma should redirect: %v %v", resp, err)
	}
}

func TestRuntimeMemcached(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	tmp := t.TempDir()
	src := filepath.Join(realRoot, `bin\memcached\memcached-1.6.8-win64-mingw`)
	dst := filepath.Join(tmp, `bin\memcached\memcached-1.6.8-win64-mingw`)
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	p := freePort(t)
	setPortOverrides(map[string]int{"memcached": p})
	a := &App{cfg: Config{Root: tmp, Versions: map[string]string{"memcached": "memcached-1.6.8-win64-mingw"}}}
	if err := a.start("memcached"); err != nil {
		t.Fatal(err)
	}
	defer killUnder(dst)
	if !a.running("memcached") {
		t.Fatal("not running after start")
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(p))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("version\r\n"))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	c.Close()
	t.Logf("memcached says: %q", strings.TrimSpace(string(buf[:n])))
	if !strings.HasPrefix(string(buf[:n]), "VERSION") {
		t.Fatal("no memcached protocol answer")
	}
	if err := a.stop("memcached"); err != nil || a.running("memcached") {
		t.Fatalf("stop: %v", err)
	}
}
