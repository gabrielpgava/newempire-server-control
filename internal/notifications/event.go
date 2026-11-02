package notifications

import (
	"errors"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const (
	Test             = "test"
	BackupSucceeded  = "backup_succeeded"
	BackupFailed     = "backup_failed"
	OffsiteSucceeded = "offsite_succeeded"
	OffsiteFailed    = "offsite_failed"
	RestoreSucceeded = "restore_succeeded"
	RestoreFailed    = "restore_failed"
	JobFailed        = "job_failed"
	ServerStarted    = "server_started"
	ServerStopped    = "server_stopped"
	ServerCrashed    = "server_crashed"
	UpdateAvailable  = "update_available"
	UpdateSucceeded  = "update_succeeded"
	UpdateFailed     = "update_failed"
)

type Event struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	InstanceID string    `json:"instance_id,omitempty"`
	JobID      string    `json:"job_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

func (e Event) Validate() error {
	if !model.ValidID(e.ID) || (e.InstanceID != "" && !model.ValidID(e.InstanceID)) || (e.JobID != "" && !model.ValidID(e.JobID)) {
		return errors.New("notification IDs must be 32 lowercase hexadecimal characters")
	}
	if eventMessage(e.Kind) == "" {
		return errors.New("notification event kind is unsupported")
	}
	if !e.OccurredAt.IsZero() && (e.OccurredAt.UTC().Year() < 1970 || e.OccurredAt.UTC().Year() > 9999) {
		return errors.New("notification occurrence time is invalid")
	}
	return nil
}

func eventMessage(kind string) string {
	switch kind {
	case Test:
		return "Notification delivery test."
	case BackupSucceeded:
		return "Local backup completed."
	case BackupFailed:
		return "Local backup failed. Review the job in the panel."
	case OffsiteSucceeded:
		return "Offsite backup transfer completed; remote metadata matched."
	case OffsiteFailed:
		return "Offsite backup transfer failed. The local backup is unchanged."
	case RestoreSucceeded:
		return "Backup restore completed."
	case RestoreFailed:
		return "Backup restore failed. Review recovery instructions in the panel."
	case JobFailed:
		return "A job failed. Review its diagnostics in the panel."
	case ServerStarted:
		return "The Minecraft server started."
	case ServerStopped:
		return "The Minecraft server stopped."
	case ServerCrashed:
		return "The Minecraft server exited unexpectedly. Review its status in the panel."
	case UpdateAvailable:
		return "An application update is available. Review it in the panel."
	case UpdateSucceeded:
		return "The application update completed."
	case UpdateFailed:
		return "The application update failed. Review recovery instructions in the panel."
	}
	return ""
}

func (e Event) text() string {
	var body strings.Builder
	body.WriteString("New Empire Server Control\n" + eventMessage(e.Kind) + "\nEvent: " + e.ID + "\nTime: " + e.OccurredAt.UTC().Format(time.RFC3339) + "\n")
	if e.InstanceID != "" {
		body.WriteString("Instance: " + e.InstanceID + "\n")
	}
	if e.JobID != "" {
		body.WriteString("Job: " + e.JobID + "\n")
	}
	return body.String()
}
