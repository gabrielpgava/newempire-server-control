package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

const operationTimeout = 15 * time.Minute

type Object struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Verification string `json:"verification"`
}

type Client struct {
	config   Config
	endpoint *url.URL
	http     *http.Client
}

func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("offsite storage is disabled")
	}
	u, _ := endpointURL(cfg.Endpoint)
	return NewWithHTTPClient(cfg, netguard.New(u.Hostname()))
}

// NewWithHTTPClient trusts the supplied transport; production callers must use New.
func NewWithHTTPClient(cfg Config, client *http.Client) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("offsite storage is disabled")
	}
	if client == nil {
		return nil, errors.New("offsite HTTP client is required")
	}
	u, _ := endpointURL(cfg.Endpoint)
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copy.Timeout <= 0 || copy.Timeout > operationTimeout {
		copy.Timeout = operationTimeout
	}
	return &Client{config: cfg, endpoint: u, http: &copy}, nil
}

func (c *Client) objectURL(key string) (*url.URL, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	fullKey := key
	if c.config.Prefix != "" {
		fullKey = c.config.Prefix + "/" + key
	}
	if err := validKey(fullKey); err != nil {
		return nil, err
	}
	u := *c.endpoint
	u.Path = "/" + c.config.Bucket + "/" + fullKey
	return &u, nil
}

func (c *Client) request(ctx context.Context, method, key, checksum string, body io.ReadCloser, size int64) (*http.Response, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("offsite request could not be constructed")
	}
	if method == http.MethodPut {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-Amz-Meta-Sha256", checksum)
	}
	req.Header.Set("Accept-Encoding", "identity")
	sign(req, c.config, checksum, time.Now())
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transferError{message: "offsite request failed", retryable: true}
	}
	return resp, nil
}

func (c *Client) Head(ctx context.Context, key string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if _, err := c.objectURL(key); err != nil {
		return Object{}, err
	}
	var object Object
	err := retry(ctx, func() error {
		resp, err := c.request(ctx, http.MethodHead, key, emptySHA256, nil, 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return statusError(resp.StatusCode)
		}
		object, err = metadata(key, resp)
		return err
	})
	return object, err
}

func metadata(key string, resp *http.Response) (Object, error) {
	checksum := resp.Header.Get("X-Amz-Meta-Sha256")
	if !validSHA256(checksum) || resp.ContentLength < 0 || resp.ContentLength > MaxObjectSize {
		return Object{}, errors.New("offsite object has invalid size or SHA-256 metadata")
	}
	return Object{Key: key, Size: resp.ContentLength, SHA256: strings.ToLower(checksum), Verification: "metadata"}, nil
}

func (c *Client) Put(ctx context.Context, key, source string) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if _, err := c.objectURL(key); err != nil {
		return Object{}, err
	}
	f, info, err := openSource(source)
	if err != nil {
		return Object{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, reader: io.NewSectionReader(f, 0, info.Size())})
	if err != nil || n != info.Size() {
		return Object{}, localError(ctx, "offsite source could not be hashed")
	}
	checksum := hex.EncodeToString(h.Sum(nil))
	err = retry(ctx, func() error {
		body := &uploadBody{ctx: ctx, reader: io.NewSectionReader(f, 0, info.Size()), hash: sha256.New()}
		resp, err := c.request(ctx, http.MethodPut, key, checksum, body, info.Size())
		body.Close()
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			return statusError(resp.StatusCode)
		}
		if !body.matches(checksum, info.Size()) {
			return errors.New("offsite source changed or was not fully transmitted")
		}
		current, err := f.Stat()
		if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return errors.New("offsite source changed during upload")
		}
		return nil
	})
	if err != nil {
		return Object{}, err
	}
	object, err := c.Head(ctx, key)
	if err != nil {
		return Object{}, err
	}
	if object.Size != info.Size() || object.SHA256 != checksum {
		return Object{}, errors.New("offsite HEAD size or SHA-256 metadata does not match the local backup")
	}
	return object, nil
}

func openSource(source string) (*os.File, os.FileInfo, error) {
	if !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return nil, nil, errors.New("offsite source must be an absolute regular backup path")
	}
	root, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return nil, nil, errors.New("offsite source directory is unavailable")
	}
	defer root.Close()
	name := filepath.Base(source)
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Size() > MaxObjectSize {
		return nil, nil, errors.New("offsite source must be a regular backup file no larger than 5 GiB")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, errors.New("offsite source could not be opened")
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		f.Close()
		return nil, nil, errors.New("offsite source changed while opening")
	}
	return f, after, nil
}

// Download verifies actual bytes against a trusted local record, not remote metadata.
func (c *Client) Download(ctx context.Context, key, destination, expectedSHA256 string, expectedSize int64) (Object, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if _, err := c.objectURL(key); err != nil {
		return Object{}, err
	}
	if !validSHA256(expectedSHA256) || expectedSize < 0 || expectedSize > MaxObjectSize {
		return Object{}, errors.New("offsite download requires a trusted SHA-256 and size no larger than 5 GiB")
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return Object{}, errors.New("offsite staging destination must be a new absolute file path")
	}
	root, err := os.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return Object{}, errors.New("offsite staging directory is unavailable")
	}
	defer root.Close()
	name := filepath.Base(destination)
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return Object{}, errors.New("offsite staging destination already exists or is unavailable")
	}
	temp := ".offsite-" + model.NewID() + ".partial"
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Object{}, errors.New("offsite staging file could not be created")
	}
	defer root.Remove(temp)
	defer f.Close()
	expectedSHA256 = strings.ToLower(expectedSHA256)
	err = retry(ctx, func() error {
		if err := f.Truncate(0); err != nil {
			return errors.New("offsite staging file could not be reset")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errors.New("offsite staging file could not be reset")
		}
		resp, err := c.request(ctx, http.MethodGet, key, emptySHA256, nil, 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return statusError(resp.StatusCode)
		}
		if resp.ContentLength != expectedSize || resp.Uncompressed || (resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
			return errors.New("offsite download size or encoding does not match the trusted backup")
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(&contextReader{ctx: ctx, reader: resp.Body}, expectedSize+1))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return errors.New("offsite staging file could not be written")
			}
			return &transferError{message: "offsite download was interrupted", retryable: true}
		}
		if n != expectedSize || hex.EncodeToString(h.Sum(nil)) != expectedSHA256 {
			return errors.New("offsite downloaded bytes do not match the trusted size or SHA-256")
		}
		return nil
	})
	if err != nil {
		return Object{}, err
	}
	if err := f.Sync(); err != nil {
		return Object{}, errors.New("offsite staging file could not be synchronized")
	}
	if err := f.Close(); err != nil {
		return Object{}, errors.New("offsite staging file could not be closed")
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	// Link is an atomic no-replace commit; Rename would overwrite a raced destination.
	if err := root.Link(temp, name); err != nil {
		return Object{}, errors.New("offsite staging destination could not be committed without replacement")
	}
	return Object{Key: key, Size: expectedSize, SHA256: expectedSHA256, Verification: "download_sha256"}, nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type transferError struct {
	message   string
	retryable bool
}

func (e *transferError) Error() string { return e.message }

func statusError(code int) error {
	return &transferError{message: fmt.Sprintf("offsite request returned HTTP %d", code), retryable: code == 408 || code == 429 || code == 500 || code == 502 || code == 503 || code == 504}
}

func retry(ctx context.Context, attempt func() error) error {
	for n := 0; ; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := attempt()
		var failure *transferError
		if err == nil || n == 2 || !errors.As(err, &failure) || !failure.retryable {
			return err
		}
		timer := time.NewTimer(time.Duration(1<<n) * 200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func localError(ctx context.Context, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(message)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type uploadBody struct {
	mu     sync.Mutex
	ctx    context.Context
	reader io.Reader
	hash   hash.Hash
	size   int64
	closed bool
}

func (b *uploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.reader.Read(p)
	b.hash.Write(p[:n])
	b.size += int64(n)
	return n, err
}

func (b *uploadBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *uploadBody) matches(checksum string, size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size == size && hex.EncodeToString(b.hash.Sum(nil)) == checksum
}
