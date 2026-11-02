package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type Schedule struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"`
	Hour       int    `json:"hour"`
	Minute     int    `json:"minute"`
	Weekdays   []int  `json:"weekdays"`
	Timezone   string `json:"timezone"`
	Enabled    bool   `json:"enabled"`
	NextRun    int64  `json:"next_run"`
	LastJobID  string `json:"last_job_id"`
}

func (s Schedule) Validate() error {
	if !model.ValidID(s.InstanceID) {
		return errors.New("invalid instance identifier")
	}
	if !slices.Contains([]string{"backup", "restart", "mod_check"}, s.Kind) {
		return errors.New("unsupported scheduled operation")
	}
	if s.Hour < 0 || s.Hour > 23 || s.Minute < 0 || s.Minute > 59 || len(s.Weekdays) == 0 {
		return errors.New("choose a valid time and at least one weekday")
	}
	for _, day := range s.Weekdays {
		if day < 0 || day > 6 {
			return errors.New("weekday must be between 0 and 6")
		}
	}
	_, err := time.LoadLocation(s.Timezone)
	return err
}

// Next scans real instants so skipped and repeated DST wall times have deterministic behavior.
func Next(s Schedule, after time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	loc, _ := time.LoadLocation(s.Timezone)
	t := after.Truncate(time.Minute).Add(time.Minute)
	for n := 0; n < 9*24*60; n++ {
		local := t.In(loc)
		if local.Hour() == s.Hour && local.Minute() == s.Minute && slices.Contains(s.Weekdays, int(local.Weekday())) {
			return t, nil
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, errors.New("could not find next schedule execution")
}

type Service struct {
	DB   *database.Store
	Jobs *jobs.Engine
}

func (s *Service) Save(ctx context.Context, entry Schedule) (Schedule, error) {
	if err := entry.Validate(); err != nil {
		return entry, err
	}
	if entry.ID == "" {
		entry.ID = model.NewID()
	} else if !model.ValidID(entry.ID) {
		return entry, errors.New("invalid schedule identifier")
	}
	next, err := Next(entry, time.Now())
	if err != nil {
		return entry, err
	}
	entry.NextRun = next.Unix()
	b, _ := json.Marshal(entry)
	_, err = s.DB.ExecContext(ctx, "INSERT INTO schedules(id,instance_id,kind,data,enabled,next_run) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET instance_id=excluded.instance_id,kind=excluded.kind,data=excluded.data,enabled=excluded.enabled,next_run=excluded.next_run", entry.ID, entry.InstanceID, entry.Kind, string(b), entry.Enabled, entry.NextRun)
	return entry, err
}
func (s *Service) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT data,next_run,last_job_id FROM schedules ORDER BY next_run")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []Schedule{}
	for rows.Next() {
		var entry Schedule
		var b string
		var next int64
		var last string
		if err := rows.Scan(&b, &next, &last); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(b), &entry); err != nil {
			return nil, err
		}
		entry.NextRun = next
		entry.LastJobID = last
		all = append(all, entry)
	}
	return all, rows.Err()
}
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	entries, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Enabled || entry.NextRun > now.Unix() {
			continue
		}
		// One catch-up run per schedule; older missed occurrences are coalesced.
		key := fmt.Sprintf("schedule:%s:%d", entry.ID, entry.NextRun)
		job, err := s.Jobs.Enqueue(ctx, entry.InstanceID, entry.Kind, map[string]any{"scheduled": true}, key, -10)
		if err != nil {
			return err
		}
		next, err := Next(entry, now)
		if err != nil {
			return err
		}
		if _, err := s.DB.ExecContext(ctx, "UPDATE schedules SET next_run=?,last_job_id=? WHERE id=? AND next_run=?", next.Unix(), job.ID, entry.ID, entry.NextRun); err != nil {
			return err
		}
	}
	return nil
}

func InWindow(now time.Time, zone, start, end string) (bool, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false, err
	}
	parse := func(value string) (int, error) {
		t, err := time.Parse("15:04", value)
		return t.Hour()*60 + t.Minute(), err
	}
	a, err := parse(start)
	if err != nil {
		return false, err
	}
	b, err := parse(end)
	if err != nil {
		return false, err
	}
	n := now.In(loc).Hour()*60 + now.In(loc).Minute()
	if a == b {
		return true, nil
	}
	if a < b {
		return n >= a && n < b, nil
	}
	return n >= a || n < b, nil
}
