package main

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real Adminer download + PHP built-in server in a sandbox. The login goes to a closed port (127.0.0.1:1):
// no real database is contacted; it only proves the empty-password rule is lifted.
func TestRuntimeAdminer(t *testing.T) {
	if os.Getenv("DEVPANEL_RUNTIME") == "" {
		t.Skip("set DEVPANEL_RUNTIME=1")
	}
	phps, _ := filepath.Glob(`C:\laragon\bin\php\php-8*\php.exe`)
	if len(phps) == 0 {
		t.Skip("no PHP to borrow")
	}
	defer setPortOverrides(nil)
	setPortOverrides(map[string]int{"adminer": freePort(t)})
	a, root := standaloneApp(t)
	if err := makeJunction(filepath.Join(root, `bin\php\borrowed`), filepath.Dir(phps[0])); err != nil {
		t.Fatal(err)
	}
	a.cfg.Versions["php"] = "borrowed"
	u, err := a.OpenAdminer("") // no pre-filled server: nothing points at a real database
	if err != nil {
		t.Fatal(err)
	}
	defer a.StopAdminer()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if (res.StatusCode != 200 && res.StatusCode != 403) || !strings.Contains(string(b), "DevPanel · Adminer") || !strings.Contains(string(b), "auth[password]") {
		t.Fatalf("%d %.300s", res.StatusCode, b)
	}
	res, err = http.PostForm(strings.Split(u, "?")[0], url.Values{"auth[driver]": {"pgsql"}, "auth[server]": {"127.0.0.1:1"},
		"auth[username]": {"postgres"}, "auth[password]": {""}, "auth[db]": {""}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(b), "without a password") {
		t.Fatal("empty password still refused by Adminer")
	}
	t.Logf("adminer at %s: %d, login without password passes Adminer's check", u, res.StatusCode)
	a.StopAdminer()
	if portOpen(port("adminer")) {
		t.Fatal("adminer still running after StopAdminer")
	}
}
