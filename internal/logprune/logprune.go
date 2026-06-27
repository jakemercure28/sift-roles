// Package logprune deletes old dated component logs.
package logprune

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var datedLog = regexp.MustCompile(`^(\d{8})\.log$`)

// Result reports one log-prune pass.
type Result struct {
	Scanned int
	Deleted int
	DryRun  bool
}

// Options configures one prune pass.
type Options struct {
	Root          string
	RetentionDays int
	DryRun        bool
	Now           time.Time
	Log           *slog.Logger
}

// Prune deletes logs/<component>/YYYYMMDD.log files older than RetentionDays,
// ported from scripts/prune-logs.js. Persistent files such as
// logs/<component>/<component>.log are ignored.
func Prune(opts Options) (Result, error) {
	if opts.Root == "" {
		opts.Root = "logs"
	}
	if opts.RetentionDays <= 0 {
		return Result{}, fmt.Errorf("invalid retention days: %d", opts.RetentionDays)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-time.Duration(opts.RetentionDays) * 24 * time.Hour)

	entries, err := os.ReadDir(opts.Root)
	if os.IsNotExist(err) {
		return Result{DryRun: opts.DryRun}, nil
	}
	if err != nil {
		return Result{}, err
	}

	var res Result
	res.DryRun = opts.DryRun
	for _, component := range entries {
		if !component.IsDir() {
			continue
		}
		dir := filepath.Join(opts.Root, component.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			match := datedLog.FindStringSubmatch(file.Name())
			if match == nil {
				continue
			}
			res.Scanned++
			if !isExpired(match[1], cutoff) {
				continue
			}
			full := filepath.Join(dir, file.Name())
			if opts.DryRun {
				if opts.Log != nil {
					opts.Log.Info("would delete old log", "file", full)
				}
				continue
			}
			if err := os.Remove(full); err != nil {
				if opts.Log != nil {
					opts.Log.Warn("delete old log failed", "file", full, "error", err)
				}
				continue
			}
			res.Deleted++
		}
	}
	return res, nil
}

func isExpired(stamp string, cutoff time.Time) bool {
	if len(stamp) != 8 {
		return false
	}
	y, errY := strconv.Atoi(stamp[0:4])
	m, errM := strconv.Atoi(stamp[4:6])
	d, errD := strconv.Atoi(stamp[6:8])
	if errY != nil || errM != nil || errD != nil {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Before(cutoff)
}
