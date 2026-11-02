// Package panelupdate implements the bounded request/report handoff to the independent updater.
package panelupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/updates"
)

const RequestFile = "panel-update-request.json"
const ReportFile = "panel-update-report.json"

type Request struct {
	ID          string          `json:"id"`
	JobID       string          `json:"job_id"`
	Action      string          `json:"action"`
	Version     string          `json:"version"`
	Channel     updates.Channel `json:"channel"`
	RequestedBy string          `json:"requested_by"`
}
type Report struct {
	RequestID string          `json:"request_id"`
	JobID     string          `json:"job_id"`
	State     string          `json:"state"`
	Error     string          `json:"error"`
	Journal   updates.Journal `json:"journal"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (r Request) Validate() error {
	if !model.ValidID(r.ID) || !model.ValidID(r.JobID) {
		return errors.New("invalid update handoff identifier")
	}
	if r.Action != "stage" && r.Action != "apply" && r.Action != "rollback" && r.Action != "recover" {
		return errors.New("unsupported independent update operation")
	}
	if r.Channel != updates.Stable && r.Channel != updates.Beta {
		return errors.New("invalid release channel")
	}
	if len(r.Version) > 120 || strings.ContainsAny(r.Version, "\x00\r\n/") {
		return errors.New("invalid release version in update request")
	}
	if r.Action == "stage" && r.Version == "" {
		return errors.New("staging requires an explicit release version")
	}
	return nil
}
func WriteRequest(ctx context.Context, root *os.Root, r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return writeJSON(ctx, root, RequestFile, r, 0600)
}
func ReadRequest(ctx context.Context, root *os.Root) (Request, error) {
	var r Request
	err := readJSON(ctx, root, RequestFile, &r, false)
	if err != nil {
		return r, err
	}
	return r, r.Validate()
}
func Publish(ctx context.Context, root *os.Root, r Report) error {
	r.UpdatedAt = time.Now().UTC()
	return writeJSON(ctx, root, ReportFile, r, 0644)
}
func ReadReport(ctx context.Context, root *os.Root) (Report, error) {
	var r Report
	err := readJSON(ctx, root, ReportFile, &r, true)
	if err != nil {
		return r, err
	}
	if !model.ValidID(r.RequestID) || !model.ValidID(r.JobID) || r.State != "running" && r.State != "succeeded" && r.State != "failed" {
		return r, errors.New("invalid independent updater report")
	}
	return r, nil
}
func readJSON(ctx context.Context, root *os.Root, name string, out any, rootOwned bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := files.OpenRegular(root, name)
	if err != nil {
		return err
	}
	defer f.Close()
	if rootOwned {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !trustedOwner(info) {
			return errors.New("independent updater report is not root-owned")
		}
	}
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("ambiguous updater handoff JSON")
	}
	return nil
}
func writeJSON(ctx context.Context, root *os.Root, name string, value any, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("updater handoff exceeds its size limit")
	}
	tmp := ".panel-update-" + model.NewID()
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(b)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
