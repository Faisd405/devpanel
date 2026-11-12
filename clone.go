package main

// Clone from Git (the local cousin of Coolify's "deploy from Git"): clone into www, then bring the project
// up with what is already installed: PHP picked by composer.json, composer/npm install, .env, app key,
// database. Nothing is installed without asking: a missing PHP version is reported, not downloaded.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// gitURLRe: https/ssh/git URLs and scp-like git@host:org/repo; never something git would read as an option.
var gitURLRe = regexp.MustCompile(`^(https?://|ssh://|git://|git@[\w.-]+:)[\w./~@:%+-]+$`)

// repoFolder derives the folder name from the repository URL (".../shop.git" -> "shop").
func repoFolder(u string) string {
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return regexp.MustCompile(`[^A-Za-z0-9_-]+`).ReplaceAllString(u, "-")
}

func (a *App) gitExe() (string, error) {
	if g := a.path(`bin\git\cmd\git.exe`); exists(g) {
		return g, nil
	}
	if g, err := exec.LookPath("git"); err == nil {
		return g, nil
	}
	return "", errors.New("git is not installed (install Git for Windows, or Laragon's git)")
}

// CloneProject clones repo into www\<folder> ("" = from the URL) and sets it up. engine: "" = what the
// project's .env.example says, "mysql", "pgsql", "sqlite" or "none".
func (a *App) CloneProject(repo, folder, engine string) (string, error) {
	repo = strings.TrimSpace(repo)
	if !gitURLRe.MatchString(repo) {
		return "", errors.New("use a Git URL like https://github.com/org/repo.git or git@github.com:org/repo.git")
	}
	if folder = strings.TrimSpace(folder); folder == "" {
		folder = repoFolder(repo)
	}
	if !folderRe.MatchString(folder) {
		return "", errors.New("folder name: letters, digits, - and _ (max 63)")
	}
	if d, err := a.siteDir(folder); err == nil {
		return "", fmt.Errorf("%s already exists", d)
	}
	git, err := a.gitExe()
	if err != nil {
		return "", err
	}
	a.emitEvent("quickapp", "git clone "+repo+" "+folder+" …")
	if err := a.streamCmd(a.www(), git, "clone", "--", repo, folder); err != nil {
		os.RemoveAll(filepath.Join(a.www(), folder)) // a half clone would block a retry
		return "", err
	}
	notes, err := a.setupProject(folder, engine)
	return "cloned " + folder + "; " + strings.Join(notes, "; "), err
}

// setupProject brings a project folder up; every step adds a note, a failed step stops the rest.
func (a *App) setupProject(folder, engine string) ([]string, error) {
	dir, err := a.siteDir(folder)
	if err != nil {
		return nil, err
	}
	var notes []string
	note := func(s string) { notes = append(notes, s); a.emitEvent("quickapp", "→ "+s) }
	has := func(f string) bool { return exists(filepath.Join(dir, f)) }

	// PHP: the composer.json constraint decides; an installed version that fits is pinned to the site.
	php := filepath.Join(a.svcDir("php"), "php.exe")
	if c := composerPHP(dir); c != "" {
		r, err := a.CheckRequirement("php", c)
		switch {
		case err != nil:
			note("PHP check failed: " + err.Error())
		case r.ActiveFits:
		case r.UseInstalled != "":
			if err := a.SetSitePHP(folder, r.UseInstalled); err == nil {
				php = filepath.Join(a.phpDir(r.UseInstalled), "php.exe")
				note("PHP " + r.UseInstalled + " for this site (needs " + c + ")")
			}
		default:
			s := "no installed PHP fits " + c
			if r.SuggestVersion != "" {
				s += "; suggested: PHP " + r.SuggestVersion + " (install it in Environment, then pick it for the site)"
			}
			note(s)
			return notes, errors.New(s) // composer install would fail on the wrong PHP
		}
	}
	if has("composer.json") {
		if !exists(php) {
			return notes, errors.New("PHP is not installed")
		}
		if err := a.ensureComposer(); err != nil {
			return notes, err
		}
		a.emitEvent("quickapp", "composer install …")
		if err := a.streamCmd(dir, php, a.path(`bin\composer\composer.phar`), "install", "--no-interaction"); err != nil {
			return notes, fmt.Errorf("composer install: %w", err)
		}
		note("composer install done")
	}
	if has("package.json") {
		cmd := "install"
		if has("package-lock.json") {
			cmd = "ci"
		}
		a.emitEvent("quickapp", "npm "+cmd+" …")
		if err := a.streamCmd(dir, "cmd", "/c", "npm", cmd); err != nil {
			note("npm " + cmd + " failed (" + err.Error() + "); run it yourself after picking a Node version")
		} else {
			note("npm " + cmd + " done")
		}
	}

	// .env from the example, app key, database.
	if !has(".env") && has(".env.example") {
		b, err := os.ReadFile(filepath.Join(dir, ".env.example"))
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, ".env"), b, 0644)
		}
		if err != nil {
			return notes, err
		}
		note(".env created from .env.example")
	}
	if has("artisan") && has(".env") {
		if b, _ := os.ReadFile(filepath.Join(dir, ".env")); envValue(string(b), "APP_KEY") == "" {
			if err := a.streamCmd(dir, php, "artisan", "key:generate", "--no-interaction"); err == nil {
				note("APP_KEY generated")
			}
		}
	}
	if has(".env") {
		b, _ := os.ReadFile(filepath.Join(dir, ".env"))
		if engine == "" {
			engine = envValue(string(b), "DB_CONNECTION")
		}
		switch engine {
		case "mysql", "mariadb", "pgsql":
			svcName := map[string]string{"pgsql": "postgresql"}[engine]
			if svcName == "" {
				svcName = "mysql"
			}
			db := dbNameFor(folder)
			if err := createDatabase(a, svcName, db); err != nil {
				note("database " + db + " not created (" + err.Error() + "); start " + svcName + " and create it in Databases")
			} else {
				note("database " + db + " created")
			}
			env := setEnv(string(b), "DB_DATABASE", db)
			os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0644)
			e := engine
			if e == "mariadb" {
				e = "mysql"
			}
			if msg, err := a.UseDevPanelDB(folder, e); err == nil {
				note(strings.SplitN(msg, " (", 2)[0])
			}
		case "sqlite":
			if has("artisan") && !has(`database\database.sqlite`) {
				os.MkdirAll(filepath.Join(dir, "database"), 0755)
				os.WriteFile(filepath.Join(dir, `database\database.sqlite`), nil, 0644)
				note("database/database.sqlite created")
			}
		}
		if has("artisan") {
			if _, err := a.FixAppURL(folder); err == nil {
				note("APP_URL set")
			}
			note("next: php artisan migrate (DevPanel doesn't run migrations for you)")
		}
	}
	a.mu.Lock()
	_, _, err = a.syncVhostFiles()
	if err == nil {
		err = a.restartWeb()
	}
	a.mu.Unlock()
	if err != nil {
		note("vhosts: " + err.Error())
	}
	return notes, nil
}

// composerPHP: the "php" constraint in composer.json's require ("" = none).
func composerPHP(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		return ""
	}
	var cj struct {
		Require map[string]string `json:"require"`
	}
	if json.Unmarshal(b, &cj) != nil {
		return ""
	}
	return cj.Require["php"]
}

// createDatabase is replaced in tests, so they never touch a real database server.
var createDatabase = (*App).CreateDatabase
