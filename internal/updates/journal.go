package updates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

type State string

const (
	Downloading    State = "downloading"
	Staged         State = "staged"
	Stopping       State = "stopping"
	Snapshotting   State = "snapshotting"
	Installing     State = "installing"
	CheckingHealth State = "checking_health"
	Complete       State = "complete"
	RollingBack    State = "rolling_back"
	RolledBack     State = "rolled_back"
	Failed         State = "failed"
	ManualRecovery State = "manual_recovery"
)

type Transaction struct {
	ID              string    `json:"id"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version"`
	Schema          int       `json:"schema"`
	Channel         Channel   `json:"channel"`
	State           State     `json:"state"`
	BinarySHA256    string    `json:"binary_sha256,omitempty"`
	PreviousSHA256  string    `json:"previous_sha256,omitempty"`
	SnapshotSHA256  string    `json:"snapshot_sha256,omitempty"`
	Error           string    `json:"error,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Event struct {
	TransactionID string    `json:"transaction_id"`
	Version       string    `json:"version"`
	State         State     `json:"state"`
	Message       string    `json:"message"`
	Time          time.Time `json:"time"`
}

type Journal struct {
	Transaction *Transaction `json:"transaction"`
	LastApplied *Transaction `json:"last_applied"`
	History     []Event      `json:"history"`
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func validTransaction(t *Transaction) bool {
	if t == nil {
		return true
	}
	_, a := parseVersion(t.Version)
	_, b := parseVersion(t.PreviousVersion)
	if !idPattern.MatchString(t.ID) || a != nil || b != nil || len(t.Version) > 120 || len(t.PreviousVersion) > 120 || t.Schema < 1 || (t.Channel != Stable && t.Channel != Beta) {
		return false
	}
	for _, h := range []string{t.BinarySHA256, t.PreviousSHA256, t.SnapshotSHA256} {
		if h != "" && !validHash(h) {
			return false
		}
	}
	switch t.State {
	case Installing, CheckingHealth, Complete, RollingBack, RolledBack:
		if !validHash(t.SnapshotSHA256) || !validHash(t.PreviousSHA256) || !validHash(t.BinarySHA256) {
			return false
		}
	case Stopping, Snapshotting:
		if !validHash(t.PreviousSHA256) || !validHash(t.BinarySHA256) {
			return false
		}
	case Staged:
		if !validHash(t.BinarySHA256) {
			return false
		}
	}
	switch t.State {
	case Downloading, Staged, Stopping, Snapshotting, Installing, CheckingHealth, Complete, RollingBack, RolledBack, Failed, ManualRecovery:
		return true
	}
	return false
}

func readJournal(r *os.Root) (Journal, error) {
	j := Journal{History: []Event{}}
	b, err := limitedRead(r, "updates/journal.json", 64<<20)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, fmt.Errorf("read update journal: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil || !validTransaction(j.Transaction) || !validTransaction(j.LastApplied) || j.LastApplied != nil && j.LastApplied.State != Complete {
		return Journal{}, problem(ErrRecovery, "update journal is invalid; preserve it for administrator recovery")
	}
	canonical, _ := json.Marshal(j)
	if !bytes.Equal(canonical, b) {
		return Journal{}, problem(ErrRecovery, "update journal has noncanonical or ambiguous content")
	}
	return j, nil
}

func transition(r *os.Root, j *Journal, state State, message string) error {
	t := j.Transaction
	t.State = state
	t.UpdatedAt = time.Now().UTC()
	j.History = append(j.History, Event{t.ID, t.Version, state, message, t.UpdatedAt})
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := atomicWrite(r, "updates/journal.json", b, 0600); err != nil {
		return fmt.Errorf("persist update journal: %w", err)
	}
	return nil
}

func cloneTransaction(t *Transaction) *Transaction {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func (m *Manager) Status(ctx context.Context) (Journal, error) {
	if err := ctx.Err(); err != nil {
		return Journal{}, err
	}
	r, err := m.openInstall()
	if err != nil {
		return Journal{}, err
	}
	defer r.Close()
	return readJournal(r)
}

func inProgress(s State) bool {
	switch s {
	case Stopping, Snapshotting, Installing, CheckingHealth, RollingBack, ManualRecovery:
		return true
	}
	return false
}
