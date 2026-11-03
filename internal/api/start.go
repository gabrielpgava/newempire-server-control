package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
	"github.com/gabrielpgava/newempire-server-control/internal/uploads"
)

func (a *App) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	a.cancel = cancel
	f, err := os.OpenFile(filepath.Join(a.Config.DataDir, "health-token"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, writeErr := f.WriteString(auth.Token() + "\n")
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
			cancel()
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		cancel()
		return err
	}
	if a.Fabric == nil {
		a.Fabric = fabric.NewClient(nil)
	}
	if a.Content == nil {
		a.Content = modrinth.NewService(nil)
	}
	root, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "uploads"))
	if err != nil {
		cancel()
		return err
	}
	a.uploadRoot = root
	a.Uploads, err = uploads.New(uploads.Config{Root: root, BasePath: "/api/v1/uploads/", MaxSize: a.Config.MaxUploadBytes, Quota: a.Config.MaxUploadBytes * 2, MaxUploads: 64, TTL: 48 * time.Hour, Authorize: func(r *http.Request) (string, error) {
		s := session(r)
		if s.User.ID == "" || !auth.Permitted(s.User.Role, "admin") {
			return "", errors.New("administrator authentication is required for uploads")
		}
		return s.User.ID, nil
	}})
	if err != nil {
		root.Close()
		cancel()
		return err
	}
	a.UploadHandler = a.Uploads.Handler()
	a.RegisterJobs()
	if err := a.Jobs.Start(ctx, 2); err != nil {
		a.Uploads.Close()
		root.Close()
		cancel()
		return err
	}
	a.reconcileUpdates(ctx)
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.background(ctx) }()
	return nil
}
func (a *App) Wait() {
	if a.cancel != nil {
		a.cancel()
	}
	a.Jobs.Wait()
	a.wg.Wait()
	if a.Uploads != nil {
		a.Uploads.Close()
	}
	if a.uploadRoot != nil {
		a.uploadRoot.Close()
	}
}

// RegisterJobs also supports a locked, offline recovery CLI without starting HTTP or automation.
func (a *App) RegisterJobs() {
	if a.Fabric == nil {
		a.Fabric = fabric.NewClient(nil)
	}
	if a.Content == nil {
		a.Content = modrinth.NewService(nil)
	}
	a.registerLifecycleJobs()
	a.registerFileJobs()
	a.registerBackupJobs()
	a.registerOffsiteJobs()
	a.registerImportJobs()
	a.registerInstanceJobs()
	a.registerSettingsJobs()
	a.registerGameJobs()
	a.registerContentJobs()
	a.registerRuntimeJobs()
	a.registerUpdateJobs()
}
