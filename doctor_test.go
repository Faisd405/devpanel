package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func findCheck(list []Check, part string) *Check {
	for i := range list {
		if strings.Contains(list[i].Title, part) {
			return &list[i]
		}
	}
	return nil
}

func TestChecks(t *testing.T) {
	a, root := standaloneApp(t)
	a.cfg.Hostnames = map[string]string{}
	php := filepath.Join(root, `bin\php\php-8.4.1`)
	touch(t, filepath.Join(php, `ext\php_intl.dll`))
	touch(t, filepath.Join(php, "php-cgi.exe"))
	os.WriteFile(filepath.Join(php, "php.ini"), []byte(";extension=intl\n"), 0644)
	a.cfg.Versions["php"] = "php-8.4.1"
	st, _ := os.Stat(filepath.Join(php, "php.ini"))
	phpModCache.Store(php, phpMods{st.ModTime(), map[string]bool{"json": true, "zend opcache": true}}) // instead of php -m
	site := filepath.Join(root, `www\shop`)
	touch(t, filepath.Join(site, "composer.json"))
	os.WriteFile(filepath.Join(site, "composer.json"), []byte(`{"require":{"php":"^8.2","ext-json":"*","ext-intl":"*","ext-gmp":"*","ext-zend-opcache":"*"}}`), 0644)
	os.WriteFile(filepath.Join(site, ".env"), []byte("APP_URL=http://old_shop.test\n"), 0644)
	// a link whose target is gone
	gone := filepath.Join(t.TempDir(), "php-x")
	os.MkdirAll(gone, 0755)
	makeJunction(filepath.Join(root, `bin\php\herd-php-x`), gone)
	os.Remove(gone)

	list := a.Checks()
	intl, gmp := findCheck(list, "extension intl"), findCheck(list, "extension gmp")
	if intl == nil || intl.Action != "ext" || intl.Args[0] != "php-8.4.1" {
		t.Fatalf("intl: %+v", intl)
	}
	if gmp == nil || gmp.Fix != "" { // no DLL shipped: reported without a fix button
		t.Fatalf("gmp: %+v", gmp)
	}
	if findCheck(list, "extension json") != nil || findCheck(list, "zend-opcache") != nil {
		t.Fatal("loaded extensions reported")
	}
	if c := findCheck(list, "APP_URL"); c == nil || c.Action != "appurl" {
		t.Fatalf("app url: %+v", c)
	}
	broken := findCheck(list, "Broken link")
	if broken == nil || broken.Args[1] != "herd-php-x" {
		t.Fatalf("broken link: %+v", broken)
	}

	// Fixes
	if _, err := a.FixCheck("ext", intl.Args); err != nil && !strings.Contains(err.Error(), "apache") { // restartWeb may meet the real Apache on 80
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(php, "php.ini")); !extEnabled(string(b), "intl") {
		t.Fatalf("intl not enabled:\n%s", b)
	}
	if _, err := a.FixCheck("ext", []string{`..\..\x`, "intl"}); err == nil {
		t.Fatal("unknown PHP folder accepted")
	}
	if _, err := a.FixCheck("unlink", broken.Args); err != nil || exists(filepath.Join(root, `bin\php\herd-php-x`)) {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := a.FixCheck("rm -rf", nil); err == nil {
		t.Fatal("unknown action accepted")
	}
}

func TestUseDevPanelMail(t *testing.T) {
	a, root := standaloneApp(t)
	site := filepath.Join(root, `www\shop`)
	touch(t, filepath.Join(site, ".env"))
	os.WriteFile(filepath.Join(site, ".env"), []byte("MAIL_MAILER=log\nMAIL_HOST=smtp.mailgun.org\n"), 0644)
	if _, err := a.UseDevPanelMail("shop"); err == nil {
		t.Fatal("Mailpit not installed: must refuse")
	}
	os.MkdirAll(filepath.Join(root, `bin\mailpit\1.22`), 0755)
	msg, err := a.UseDevPanelMail("shop")
	if err != nil || !strings.Contains(msg, "MAIL_HOST=smtp.mailgun.org") {
		t.Fatalf("%q %v", msg, err)
	}
	b, _ := os.ReadFile(filepath.Join(site, ".env"))
	for _, want := range []string{"MAIL_MAILER=smtp", "MAIL_HOST=127.0.0.1", "MAIL_PORT=1025"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s:\n%s", want, b)
		}
	}
}

// The web server Start All uses: the one picked last, else the first ticked; not-installed picks are ignored.
func TestPreferredWeb(t *testing.T) {
	a, root := standaloneApp(t)
	os.MkdirAll(filepath.Join(root, `bin\apache\httpd-2.4`), 0755)
	os.MkdirAll(filepath.Join(root, `bin\nginx\nginx-1.28`), 0755)
	a.cfg.Services = []string{"apache", "nginx"}
	if w := a.preferredWeb(); w != "apache" {
		t.Fatalf("default = %s", w)
	}
	a.cfg.WebServer = "nginx"
	if w := a.preferredWeb(); w != "nginx" {
		t.Fatalf("picked = %s", w)
	}
	a.cfg.WebServer = "caddy" // not installed
	if w := a.preferredWeb(); w != "apache" {
		t.Fatalf("missing pick = %s", w)
	}
	if _, err := a.SetWebServer("mysql"); err == nil {
		t.Fatal("mysql accepted as web server")
	}
	if _, err := a.SetWebServer("caddy"); err == nil {
		t.Fatal("not installed accepted")
	}
}
