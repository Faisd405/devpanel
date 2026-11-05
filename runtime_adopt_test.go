package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real Apache "from XAMPP" (extracted into a fake C:\xampp in a temp dir) is linked, started by DevPanel
// with its own -f config, serves a PHP site, and XAMPP's httpd.conf stays byte-for-byte the same.
func TestRuntimeAdoptedApache(t *testing.T) {
	runtimeOnly(t)
	defer setPortOverrides(nil)
	base := t.TempDir()
	xampp := filepath.Join(base, "xampp")
	a := &App{cfg: Config{Root: filepath.Join(base, "devpanel"), TLD: "test", Versions: map[string]string{}}, cfgPath: filepath.Join(base, `devpanel\devpanel.json`)}
	a.ensureLayout()

	zip := cachedDownload(t, a, "httpd-2.4.69.zip", "https://www.apachelounge.com/download/VS18/binaries/httpd-2.4.69-261002-Win64-VS18.zip")
	installInto(t, a, zip, filepath.Join(xampp, "apache"))
	xamppConf := filepath.Join(xampp, `apache\conf\httpd.conf`)
	original, _ := os.ReadFile(xamppConf)
	junction(t, filepath.Join(xampp, "php"), filepath.Join(realRoot, `bin\php`, php85)) // XAMPP's single PHP folder
	os.MkdirAll(filepath.Join(xampp, "htdocs"), 0755)

	orig := externalSources
	defer func() { externalSources = orig }()
	externalSources = func() []extSource {
		return []extSource{{"XAMPP", "xampp", filepath.Join(xampp, "htdocs"), []string{xampp}, 3}}
	}
	for _, s := range []string{"apache", "php"} {
		if _, err := a.LinkExternal(s, filepath.Join(xampp, s)); err != nil {
			t.Fatal(err)
		}
	}
	a.cfg.Versions["apache"], a.cfg.Versions["php"] = "xampp-apache", "xampp-php"
	if err := a.UseExternalWWW("XAMPP"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(xampp, `htdocs\shop\public`), 0755)
	os.WriteFile(filepath.Join(xampp, `htdocs\shop\public\index.php`), []byte("<?php echo 'adopted ', PHP_VERSION, ' ', $_SERVER['SERVER_NAME'];"), 0644)
	setPortOverrides(map[string]int{"apache": freePort(t), "apache_ssl": freePort(t)})
	if _, _, err := a.syncVhostFiles(); err != nil {
		t.Fatal(err)
	}
	if err := a.start("apache"); err != nil {
		b, _ := os.ReadFile(a.path(`data\apache\error.log`))
		c, _ := os.ReadFile(a.adoptedApacheConf())
		t.Fatalf("%v\n%s\n%s", err, b, c)
	}
	defer a.stop("apache")
	r := get(t, fmt.Sprintf("http://127.0.0.1:%d/", port("apache")), "shop.test")
	t.Logf("adopted XAMPP apache: %s", r)
	if !strings.HasPrefix(r, "200 adopted 8.5.") {
		t.Fatal("linked Apache did not serve the site")
	}
	if !a.running("apache") {
		t.Fatal("linked Apache not recognized as running")
	}
	if err := a.stop("apache"); err != nil || a.running("apache") {
		t.Fatalf("stop: %v", err)
	}
	if after, _ := os.ReadFile(xamppConf); string(after) != string(original) {
		t.Fatal("XAMPP's httpd.conf was modified")
	}
	if exists(xamppConf + ".dist") {
		t.Fatal("DevPanel treated the linked Apache as its own install")
	}
}
