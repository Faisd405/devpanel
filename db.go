package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Names are restricted so they can be passed to SQL without quoting tricks.
var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

var systemDBs = map[string][]string{
	"postgresql": {"postgres", "template0", "template1"},
	"mysql":      {"mysql", "sys", "information_schema", "performance_schema"},
}

func (a *App) dbBin(engine, exe string) (string, error) {
	if engine != "postgresql" && engine != "mysql" {
		return "", fmt.Errorf("unknown engine %q", engine)
	}
	d := a.svcDir(engine)
	if d == "" {
		return "", fmt.Errorf("%s is not installed", engine)
	}
	if !a.running(engine) {
		return "", fmt.Errorf("%s is not running; start it first", engine)
	}
	return filepath.Join(d, "bin", exe+".exe"), nil
}

// dbRun runs a client tool. stdin/stdout may be files (restore / mysqldump). Long timeout for big dumps.
func dbRun(stdin, stdout *os.File, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cmd := withMySQLEnv(hidden(exec.CommandContext(ctx, exe, args...)))
	var errb bytes.Buffer
	cmd.Stderr = &errb
	var outb bytes.Buffer
	cmd.Stdout = &outb
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(outb.String()), nil
}

var mysqlPwd string

func setMySQLPassword(pw string) { mysqlPwd = pw }

// withMySQLEnv gives mysql/mysqladmin/mysqldump the stored root password via MYSQL_PWD,
// so it never appears on a command line or in the process list.
func withMySQLEnv(cmd *exec.Cmd) *exec.Cmd {
	if mysqlPwd != "" && strings.HasPrefix(strings.ToLower(filepath.Base(cmd.Path)), "mysql") {
		cmd.Env = append(os.Environ(), "MYSQL_PWD="+mysqlPwd)
	}
	return cmd
}

func pgArgs(extra ...string) []string {
	extra = append([]string{"-p", strconv.Itoa(port("postgresql"))}, extra...)
	return append([]string{"-U", "postgres", "-h", "127.0.0.1"}, extra...)
}
func myArgs(extra ...string) []string {
	return append([]string{"-uroot", "-h127.0.0.1", "-P" + strconv.Itoa(port("mysql"))}, extra...)
}

func checkName(engine, name string) error {
	if !dbNameRe.MatchString(name) {
		return errors.New("database name: letters, digits, _ and - only (max 63)")
	}
	if slices.Contains(systemDBs[engine], strings.ToLower(name)) {
		return fmt.Errorf("%s is a system database", name)
	}
	return nil
}

func (a *App) Databases(engine string) ([]string, error) {
	var out string
	var err error
	switch engine {
	case "postgresql":
		var psql string
		if psql, err = a.dbBin(engine, "psql"); err == nil {
			out, err = dbRun(nil, nil, psql, pgArgs("-At", "-c", "SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1")...)
		}
	case "mysql":
		var mysql string
		if mysql, err = a.dbBin(engine, "mysql"); err == nil {
			out, err = dbRun(nil, nil, mysql, myArgs("-N", "-e", "SHOW DATABASES")...)
		}
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}
	if err != nil {
		return nil, err
	}
	list := []string{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !slices.Contains(systemDBs[engine], l) {
			list = append(list, l)
		}
	}
	return list, nil
}

func (a *App) createDB(engine, name string) error {
	switch engine {
	case "postgresql":
		exe, err := a.dbBin(engine, "createdb")
		if err != nil {
			return err
		}
		_, err = dbRun(nil, nil, exe, pgArgs("-E", "UTF8", name)...)
		return err
	default:
		exe, err := a.dbBin(engine, "mysql")
		if err != nil {
			return err
		}
		_, err = dbRun(nil, nil, exe, myArgs("-e", "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")...)
		return err
	}
}

func (a *App) CreateDatabase(engine, name string) error {
	if err := checkName(engine, name); err != nil {
		return err
	}
	return a.createDB(engine, name)
}

// DropDatabase permanently deletes a database. confirm must repeat the exact name.
func (a *App) DropDatabase(engine, name, confirm string) error {
	if err := checkName(engine, name); err != nil {
		return err
	}
	if confirm != name {
		return errors.New("confirmation does not match the database name; nothing dropped")
	}
	switch engine {
	case "postgresql":
		exe, err := a.dbBin(engine, "dropdb")
		if err != nil {
			return err
		}
		_, err = dbRun(nil, nil, exe, pgArgs(name)...)
		return err
	default:
		exe, err := a.dbBin(engine, "mysql")
		if err != nil {
			return err
		}
		_, err = dbRun(nil, nil, exe, myArgs("-e", "DROP DATABASE `"+name+"`")...)
		return err
	}
}

func (a *App) backupDir(engine string) string { return a.path(`backup\` + engine) }

// BackupDatabase writes a plain SQL dump to C:\laragon\backup\<engine>\<name>_<time>.sql.
func (a *App) BackupDatabase(engine, name string) (string, error) {
	if !dbNameRe.MatchString(name) {
		return "", errors.New("invalid database name")
	}
	dir := a.backupDir(engine)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return a.backupTo(engine, name, filepath.Join(dir, name+"_"+time.Now().Format("20060102-150405")+".sql"))
}

// backupTo dumps one database into file (removed again if the dump fails).
func (a *App) backupTo(engine, name, file string) (string, error) {
	var err error
	switch engine {
	case "postgresql":
		var exe string
		if exe, err = a.dbBin(engine, "pg_dump"); err == nil {
			_, err = dbRun(nil, nil, exe, pgArgs("--no-owner", "-f", file, name)...)
		}
	default:
		var exe string
		if exe, err = a.dbBin(engine, "mysqldump"); err == nil {
			var f *os.File
			if f, err = os.Create(file); err == nil {
				_, err = dbRun(nil, f, exe, myArgs("--single-transaction", "--routines", "--triggers", name)...)
				f.Close()
			}
		}
	}
	if err != nil {
		os.Remove(file)
		return "", err
	}
	return "saved " + file, nil
}

// PickSQLFile shows a file dialog for restore, starting in the engine's backup folder.
func (a *App) PickSQLFile(engine string) (string, error) {
	if a.ctx == nil {
		return "", errors.New("no window")
	}
	os.MkdirAll(a.backupDir(engine), 0755)
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Choose a .sql dump to restore",
		DefaultDirectory: a.backupDir(engine),
		Filters:          []runtime.FileFilter{{DisplayName: "SQL dump (*.sql)", Pattern: "*.sql"}},
	})
}

// RestoreDatabase runs a SQL dump into a database (created if missing). confirm must repeat the name.
func (a *App) RestoreDatabase(engine, name, file, confirm string) (string, error) {
	if err := checkName(engine, name); err != nil {
		return "", err
	}
	if confirm != name {
		return "", errors.New("confirmation does not match the database name; nothing restored")
	}
	if st, err := os.Stat(file); err != nil || st.IsDir() {
		return "", errors.New("dump file not found")
	}
	existing, err := a.Databases(engine)
	if err != nil {
		return "", err
	}
	if !slices.Contains(existing, name) {
		if err := a.createDB(engine, name); err != nil {
			return "", err
		}
	}
	switch engine {
	case "postgresql":
		exe, err := a.dbBin(engine, "psql")
		if err != nil {
			return "", err
		}
		_, err = dbRun(nil, nil, exe, pgArgs("-d", name, "-v", "ON_ERROR_STOP=1", "-q", "-f", file)...)
		if err != nil {
			return "", err
		}
	default:
		exe, err := a.dbBin(engine, "mysql")
		if err != nil {
			return "", err
		}
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err = dbRun(f, nil, exe, myArgs(name)...); err != nil {
			return "", err
		}
	}
	return "restored " + filepath.Base(file) + " into " + name, nil
}

func (a *App) OpenBackupFolder(engine string) error {
	if engine != "postgresql" && engine != "mysql" {
		return errors.New("unknown engine")
	}
	os.MkdirAll(a.backupDir(engine), 0755)
	return exec.Command("explorer", a.backupDir(engine)).Start()
}
