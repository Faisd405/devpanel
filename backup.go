package main

// Scheduled database backups with retention (after Coolify's scheduled backups). Scheduled dumps are named
// <db>_auto_<time>.sql, so retention only ever deletes scheduled ones, never a backup made by hand.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type BackupJob struct {
	Engine string    `json:"engine"` // postgresql, mysql
	DB     string    `json:"db"`
	Every  string    `json:"every"` // hourly, daily, weekly
	Keep   int       `json:"keep"`  // newest scheduled dumps to keep
	Last   time.Time `json:"last"`  // last successful run
	Error  string    `json:"error"` // last failure, "" = fine
}

var backupEvery = map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}

func (a *App) BackupJobs() []BackupJob { return slices.Clone(a.cfg.Backups) }

// SetBackupJob schedules a database ("" every = no schedule). Keep 0 = 7.
func (a *App) SetBackupJob(engine, db, every string, keep int) error {
	if engine != "postgresql" && engine != "mysql" {
		return fmt.Errorf("unknown engine %q", engine)
	}
	if !dbNameRe.MatchString(db) {
		return errors.New("invalid database name")
	}
	if _, ok := backupEvery[every]; !ok && every != "" {
		return fmt.Errorf("every must be hourly, daily or weekly")
	}
	if keep <= 0 {
		keep = 7
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.cfg.Backups, func(j BackupJob) bool { return j.Engine == engine && j.DB == db })
	switch {
	case every == "" && i >= 0:
		a.cfg.Backups = slices.Delete(a.cfg.Backups, i, i+1)
	case every != "" && i >= 0:
		a.cfg.Backups[i].Every, a.cfg.Backups[i].Keep = every, keep
	case every != "":
		a.cfg.Backups = append(a.cfg.Backups, BackupJob{Engine: engine, DB: db, Every: every, Keep: keep})
	}
	return a.saveConfig()
}

// runBackupJobs runs the jobs that are due; a stopped database is skipped (tried again next round).
func (a *App) runBackupJobs(now time.Time) []string {
	var done []string
	for _, j := range a.BackupJobs() { // a copy: the list may change while a dump runs
		if now.Sub(j.Last) < backupEvery[j.Every] || !backupRunning(a, j.Engine) {
			continue
		}
		file := filepath.Join(a.backupDir(j.Engine), j.DB+"_auto_"+now.Format("20060102-150405")+".sql")
		os.MkdirAll(filepath.Dir(file), 0755)
		_, err := backupDump(a, j.Engine, j.DB, file)
		a.mu.Lock()
		if i := slices.IndexFunc(a.cfg.Backups, func(x BackupJob) bool { return x.Engine == j.Engine && x.DB == j.DB }); i >= 0 {
			if err != nil {
				a.cfg.Backups[i].Error = err.Error()
			} else {
				a.cfg.Backups[i].Last, a.cfg.Backups[i].Error = now, ""
			}
			a.saveConfig()
		}
		a.mu.Unlock()
		if err != nil {
			done = append(done, fmt.Sprintf("backup of %s failed: %v", j.DB, err))
			continue
		}
		pruneBackups(a.backupDir(j.Engine), j.DB, j.Keep)
		done = append(done, "backed up "+j.DB)
	}
	return done
}

// replaced in tests: never dump a real database there
var (
	backupDump    = (*App).backupTo
	backupRunning = (*App).running
)

// pruneBackups keeps the newest keep scheduled dumps of db (timestamps sort as text).
func pruneBackups(dir, db string, keep int) {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(db) + `_auto_\d{8}-\d{6}\.sql$`)
	var files []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if re.MatchString(e.Name()) {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)
	for len(files) > keep {
		os.Remove(filepath.Join(dir, files[0]))
		files = files[1:]
	}
}

// watchBackups checks every 10 minutes while the agent runs.
func (a *App) watchBackups() {
	for {
		if len(a.cfg.Backups) > 0 {
			if done := a.runBackupJobs(time.Now()); len(done) > 0 {
				msg := strings.Join(done, "; ")
				a.emitEvent("log", "Scheduled backups: "+msg, strings.Contains(msg, "failed"))
			}
		}
		time.Sleep(10 * time.Minute)
	}
}

// backupChecks: jobs that failed, or haven't run for twice their interval (database never running?).
func (a *App) backupChecks(now time.Time) []Check {
	var out []Check
	for _, j := range a.cfg.Backups {
		switch {
		case j.Error != "":
			out = append(out, Check{Level: "error", Title: "Scheduled backup of " + j.DB + " failed", Detail: j.Error})
		case !j.Last.IsZero() && now.Sub(j.Last) > 2*backupEvery[j.Every]:
			out = append(out, Check{Level: "warn", Title: "No backup of " + j.DB + " since " + j.Last.Format("2006-01-02 15:04"),
				Detail: "Scheduled " + j.Every + "; it runs only while " + j.Engine + " is running and DevPanel is open."})
		}
	}
	return out
}
