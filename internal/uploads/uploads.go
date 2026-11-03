// Package uploads provides owner-checked, quota-reserved tus uploads in private
// staging storage. Uploaded data is never written directly into an instance.
package uploads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	tus "github.com/tus/tusd/v2/pkg/handler"
	"golang.org/x/sys/unix"
)

type Config struct {
	Root       *os.Root
	BasePath   string
	Authorize  func(*http.Request) (owner string, err error)
	MaxSize    int64
	Quota      int64
	MaxUploads int
	TTL        time.Duration
}

type Service struct {
	config   Config
	handler  http.Handler
	lease    *os.File
	mu       sync.Mutex
	reserved int64
	count    int
	closed   bool
}

type state struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ownerKey struct{}

type Completed struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
	File      *os.File  `json:"-"`
}

// New borrows Root; the caller closes it after stopping HTTP requests and Close.
// A process-held flock enforces one quota authority per private storage root.
func New(config Config) (*Service, error) {
	if config.Root == nil || config.Authorize == nil {
		return nil, errors.New("upload root and authorization callback are required")
	}
	if config.MaxSize <= 0 || config.Quota < config.MaxSize || config.MaxUploads < 1 || config.MaxUploads > 100000 || config.TTL <= 0 || config.TTL > 7*24*time.Hour {
		return nil, errors.New("positive upload limits, quota, count and TTL of at most seven days are required")
	}
	if !strings.HasPrefix(config.BasePath, "/") || strings.ContainsAny(config.BasePath, "?#\\") || strings.Contains(config.BasePath, "//") {
		return nil, errors.New("tus base path must be an absolute URL path")
	}
	config.BasePath = strings.TrimSuffix(config.BasePath, "/") + "/"
	if name := strings.Trim(config.BasePath, "/"); name != "" && files.ValidatePath(name) != nil {
		return nil, errors.New("tus base path contains unsafe components")
	}
	info, err := files.Check(config.Root, ".")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("upload storage must be a private directory (0700)")
	}
	lease, err := config.Root.OpenFile(".nesc-service.lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = files.Check(config.Root, ".nesc-service.lock"); err != nil {
		lease.Close()
		return nil, err
	}
	if err = unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lease.Close()
		return nil, errors.New("upload storage is already in use")
	}
	s := &Service{config: config, lease: lease}
	if err = s.recoverStorage(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	composer := tus.NewStoreComposer()
	composer.UseCore(s)
	composer.UseTerminater(s)
	composer.UseLocker(s)
	h, err := tus.NewHandler(tus.Config{
		StoreComposer: composer, BasePath: config.BasePath, MaxSize: config.MaxSize,
		DisableDownload: true, DisableConcatenation: true,
		Cors:               &tus.CorsConfig{Disable: true},
		AcquireLockTimeout: 5 * time.Second, NetworkTimeout: 30 * time.Second,
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	s.handler = http.StripPrefix(strings.TrimSuffix(config.BasePath, "/"), h)
	return s, nil
}

func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	owner, err := s.config.Authorize(r)
	if err != nil || owner == "" || len(owner) > 256 {
		http.Error(w, "upload access denied", http.StatusForbidden)
		return
	}
	if r.Header.Get("X-HTTP-Method-Override") != "" || len(r.Header.Get("Upload-Metadata")) > 4096 {
		http.Error(w, "invalid upload request", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(r.URL.Path, s.config.BasePath) {
		http.NotFound(w, r)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, s.config.BasePath)
	if id != "" {
		st, readErr := s.readState(id)
		if readErr != nil || st.Owner != owner || !time.Now().Before(st.ExpiresAt) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Upload-Expires", st.ExpiresAt.UTC().Format(http.TimeFormat))
	} else if r.Method == http.MethodPost {
		if err = s.Prune(r.Context()); err != nil {
			http.Error(w, "upload cleanup failed", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Upload-Expires", time.Now().Add(s.config.TTL).UTC().Format(http.TimeFormat))
	}
	r = r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner))
	s.handler.ServeHTTP(w, r)
}

func validFilename(name string) bool {
	return files.ValidatePath(name) == nil && name != "." && !strings.Contains(name, "/") && !strings.HasPrefix(strings.ToLower(name), ".nesc-")
}

func (s *Service) readState(id string) (state, error) {
	if !model.ValidID(id) {
		return state{}, tus.ErrNotFound
	}
	f, err := files.OpenRegular(s.config.Root, id+".json")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return state{}, tus.ErrNotFound
		}
		return state{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		return state{}, errors.New("invalid upload state")
	}
	var st state
	if err = json.Unmarshal(raw, &st); err != nil {
		return state{}, err
	}
	if st.ID != id || st.Owner == "" || len(st.Owner) > 256 || !validFilename(st.Filename) || st.Size < 0 || st.Size > s.config.MaxSize || st.CreatedAt.IsZero() || !st.ExpiresAt.After(st.CreatedAt) || st.ExpiresAt.Sub(st.CreatedAt) > 7*24*time.Hour {
		return state{}, errors.New("invalid upload state")
	}
	return st, nil
}

// NewUpload implements tus.DataStore with reservation before data is accepted.
func (s *Service) NewUpload(ctx context.Context, info tus.FileInfo) (tus.Upload, error) {
	owner, _ := ctx.Value(ownerKey{}).(string)
	if owner == "" || len(owner) > 256 {
		return nil, tus.NewError("ERR_OWNER", "upload owner is required", http.StatusForbidden)
	}
	if !validFilename(info.MetaData["filename"]) || info.SizeIsDeferred || info.IsPartial || info.IsFinal || info.Size < 0 {
		return nil, tus.NewError("ERR_METADATA", "filename and fixed upload length are required", http.StatusBadRequest)
	}
	if info.Size > s.config.MaxSize {
		return nil, tus.ErrMaxSizeExceeded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("upload storage is closed")
	}
	if info.Size > s.config.Quota-s.reserved || s.count >= s.config.MaxUploads {
		return nil, tus.NewError("ERR_QUOTA", "upload storage quota exceeded", http.StatusRequestEntityTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st := state{ID: model.NewID(), Owner: owner, Filename: info.MetaData["filename"], Size: info.Size, CreatedAt: time.Now().UTC()}
	st.ExpiresAt = st.CreatedAt.Add(s.config.TTL)
	f, err := s.config.Root.OpenFile(st.ID+".data", os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err == nil {
		err = s.createLock(st.ID)
	}
	if err == nil {
		raw, marshalErr := json.Marshal(st)
		err = marshalErr
		if err == nil {
			var metadata *os.File
			metadata, err = s.config.Root.OpenFile(".nesc-new-"+st.ID, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
			if err == nil {
				_, err = metadata.Write(raw)
				if err == nil {
					err = metadata.Sync()
				}
				err = errors.Join(err, metadata.Close())
				if err == nil {
					err = files.RenameNoReplace(s.config.Root, ".nesc-new-"+st.ID, st.ID+".json")
				}
			}
		}
	}
	if err == nil {
		err = s.syncRoot()
	}
	if err != nil {
		s.config.Root.Remove(".nesc-new-" + st.ID)
		s.config.Root.Remove(st.ID + ".json")
		s.config.Root.Remove(st.ID + ".data")
		s.config.Root.Remove(st.ID + ".lock")
		return nil, err
	}
	s.reserved += st.Size
	s.count++
	return &upload{service: s, state: st}, nil
}

func (s *Service) GetUpload(ctx context.Context, id string) (tus.Upload, error) {
	st, err := s.readState(id)
	owner, _ := ctx.Value(ownerKey{}).(string)
	if err != nil {
		return nil, err
	}
	if owner == "" || owner != st.Owner || !time.Now().Before(st.ExpiresAt) {
		return nil, tus.ErrNotFound
	}
	return &upload{service: s, state: st}, nil
}

func (s *Service) AsTerminatableUpload(u tus.Upload) tus.TerminatableUpload {
	return u.(*upload)
}

type upload struct {
	service *Service
	state   state
}

func (u *upload) open(flag int) (*os.File, error) {
	root := u.service.config.Root
	name := u.state.ID + ".data"
	before, err := files.Check(root, name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, files.ErrUnsafeType
	}
	f, err := root.OpenFile(name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() > u.state.Size {
		f.Close()
		return nil, errors.New("upload data does not match its state")
	}
	return f, nil
}

func (u *upload) GetInfo(ctx context.Context) (tus.FileInfo, error) {
	f, err := u.open(os.O_RDONLY)
	if err != nil {
		return tus.FileInfo{}, err
	}
	info, err := f.Stat()
	f.Close()
	if err != nil {
		return tus.FileInfo{}, err
	}
	return tus.FileInfo{ID: u.state.ID, Size: u.state.Size, Offset: info.Size(), MetaData: tus.MetaData{"filename": u.state.Filename}}, nil
}

func (u *upload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	ctx, cancel := context.WithDeadline(ctx, u.state.ExpiresAt)
	defer cancel()
	f, err := u.open(os.O_WRONLY)
	if err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, err
	}
	if offset < 0 || info.Size() != offset || offset > u.state.Size {
		f.Close()
		return 0, tus.ErrMismatchOffset
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return 0, err
	}
	n, err := files.Copy(ctx, f, src, u.state.Size-offset, nil)
	err = errors.Join(err, f.Sync(), f.Close())
	return n, err
}

func (u *upload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return u.open(os.O_RDONLY)
}

func (u *upload) FinishUpload(ctx context.Context) error { return ctx.Err() }

func (u *upload) Terminate(ctx context.Context) error { return u.service.remove(u.state.ID) }

// Lookup returns only complete uploads belonging to owner. The caller must close
// File; an open completed descriptor remains usable if expiry subsequently prunes it.
func (s *Service) Lookup(ctx context.Context, owner, id string) (Completed, error) {
	lock, err := s.NewLock(id)
	if err != nil {
		return Completed{}, err
	}
	if err = lock.Lock(ctx, func() {}); err != nil {
		return Completed{}, err
	}
	defer lock.Unlock()
	st, err := s.readState(id)
	if err != nil || owner == "" || st.Owner != owner || !time.Now().Before(st.ExpiresAt) {
		return Completed{}, tus.ErrNotFound
	}
	u := upload{service: s, state: st}
	f, err := u.open(os.O_RDONLY)
	if err != nil {
		return Completed{}, err
	}
	info, err := f.Stat()
	if err != nil || info.Size() != st.Size {
		f.Close()
		return Completed{}, errors.New("upload is not complete")
	}
	return Completed{ID: st.ID, Filename: st.Filename, Size: st.Size, ExpiresAt: st.ExpiresAt, File: f}, nil
}

func (s *Service) Delete(ctx context.Context, owner, id string) error {
	lock, err := s.NewLock(id)
	if err != nil {
		return err
	}
	if err = lock.Lock(ctx, func() {}); err != nil {
		return err
	}
	defer lock.Unlock()
	st, err := s.readState(id)
	if err != nil || owner == "" || st.Owner != owner {
		return tus.ErrNotFound
	}
	return s.remove(id)
}

func (s *Service) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.readState(id)
	if err != nil {
		return err
	}
	if err = s.config.Root.Remove(id + ".data"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = s.config.Root.Remove(id + ".json"); err != nil {
		return err
	}
	s.count--
	s.reserved -= st.Size
	s.config.Root.Remove(id + ".lock")
	return s.syncRoot()
}

func (s *Service) states(ctx context.Context) ([]state, error) {
	f, err := s.config.Root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	result := []state{}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		names, readErr := f.Readdirnames(256)
		for _, name := range names {
			id := strings.TrimSuffix(name, ".json")
			if id == name || !model.ValidID(id) {
				continue
			}
			st, err := s.readState(id)
			if err != nil {
				if errors.Is(err, tus.ErrNotFound) {
					continue
				}
				return nil, err
			}
			result = append(result, st)
			if len(result) > s.config.MaxUploads {
				return nil, errors.New("stored uploads exceed configured count")
			}
		}
		if errors.Is(readErr, io.EOF) {
			return result, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func (s *Service) Prune(ctx context.Context) error {
	states, err := s.states(ctx)
	if err != nil {
		return err
	}
	for _, st := range states {
		if time.Now().Before(st.ExpiresAt) {
			continue
		}
		lock, err := s.NewLock(st.ID)
		if err != nil {
			return err
		}
		if err = lock.Lock(ctx, func() {}); err != nil {
			return err
		}
		err = s.remove(st.ID)
		unlockErr := lock.Unlock()
		if errors.Is(err, tus.ErrNotFound) {
			err = nil
		}
		if err = errors.Join(err, unlockErr); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverStorage(ctx context.Context) error {
	states, err := s.states(ctx)
	if err != nil {
		return err
	}
	for _, st := range states {
		if st.Size > s.config.Quota-s.reserved {
			return errors.New("stored uploads exceed configured quota")
		}
		s.reserved += st.Size
		s.count++
		if err = s.createLock(st.ID); err != nil {
			return err
		}
	}
	if err = s.Prune(ctx); err != nil {
		return err
	}
	// Creation writes data before state. A crash between them leaves an orphan,
	// which is safe to remove only while the exclusive process lease is held.
	f, err := s.config.Root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		names, readErr := f.Readdirnames(256)
		for _, name := range names {
			if strings.HasPrefix(name, ".nesc-new-") && model.ValidID(strings.TrimPrefix(name, ".nesc-new-")) {
				if _, err = files.Check(s.config.Root, name); err != nil {
					return err
				}
				if err = s.config.Root.Remove(name); err != nil {
					return err
				}
				continue
			}
			id := strings.TrimSuffix(strings.TrimSuffix(name, ".data"), ".lock")
			if id == name || !model.ValidID(id) {
				continue
			}
			if _, err := s.readState(id); errors.Is(err, tus.ErrNotFound) {
				if _, err = files.Check(s.config.Root, name); err != nil {
					return err
				}
				if err = s.config.Root.Remove(name); err != nil {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return s.syncRoot()
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (s *Service) createLock(id string) error {
	// Publish lock files before upload metadata, not concurrently in PATCH.
	f, err := s.config.Root.OpenFile(id+".lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	info, checkErr := files.Check(s.config.Root, id+".lock")
	if checkErr != nil || !info.Mode().IsRegular() {
		f.Close()
		return files.ErrUnsafeType
	}
	return errors.Join(f.Sync(), f.Close())
}

func (s *Service) syncRoot() error {
	f, err := s.config.Root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Close must be called only after all requests and background operations finish.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(unix.Flock(int(s.lease.Fd()), unix.LOCK_UN), s.lease.Close())
}

// NewLock implements tus.Locker. Keeping offsets behind OS locks also protects
// Lookup, explicit deletion and expiry from an in-progress PATCH.
func (s *Service) NewLock(id string) (tus.Lock, error) {
	if !model.ValidID(id) || path.Base(id) != id {
		return nil, tus.ErrNotFound
	}
	if _, err := s.readState(id); err != nil {
		return nil, err
	}
	return &uploadLock{root: s.config.Root, id: id}, nil
}

type uploadLock struct {
	root *os.Root
	id   string
	file *os.File
}

func (l *uploadLock) Lock(ctx context.Context, requestUnlock func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := l.root.OpenFile(l.id+".lock", os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tus.ErrNotFound
		}
		return err
	}
	info, err := files.Check(l.root, l.id+".lock")
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return files.ErrUnsafeType
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			l.file = f
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return fmt.Errorf("lock upload: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return tus.ErrLockTimeout
		case <-ticker.C:
		}
	}
}

func (l *uploadLock) Unlock() error {
	if l.file == nil {
		return nil
	}
	err := errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close())
	l.file = nil
	return err
}
