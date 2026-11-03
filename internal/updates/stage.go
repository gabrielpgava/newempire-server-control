package updates

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
)

func stageDir(t *Transaction) string    { return "updates/staging/" + t.ID }
func snapshotDir(t *Transaction) string { return "updates/snapshots/" + t.ID }

func fail(r *os.Root, j *Journal, cause error) error {
	j.Transaction.Error = cause.Error()
	return errors.Join(cause, transition(r, j, Failed, cause.Error()))
}

func (m *Manager) Stage(ctx context.Context, requested string, c Channel) (Transaction, error) {
	var zero Transaction
	c, err := channel(c)
	if err != nil {
		return zero, err
	}
	v, err := parseVersion(requested)
	if err != nil || len(requested) > 120 {
		return zero, problem(ErrInvalid, "stage requires a SemVer version of at most 120 bytes, without v")
	}
	r, err := m.lock(ctx)
	if err != nil {
		return zero, err
	}
	defer r.close()
	j, err := readJournal(r.Root)
	if err != nil {
		return zero, err
	}
	if j.Transaction != nil && (inProgress(j.Transaction.State) || j.Transaction.State == Downloading) {
		return zero, problem(ErrConflict, "recover the interrupted update before staging another release")
	}
	previous, err := currentVersion(r.Root)
	if err != nil {
		return zero, err
	}
	old, _ := parseVersion(previous)
	if v.compare(old) <= 0 {
		return zero, problem(ErrConflict, "staging requires a newer version; downgrade and equal-precedence builds are refused")
	}
	b, err := m.fetch(ctx, releasesAPI+"/tags/"+url.PathEscape("v"+requested), 4<<20)
	if err != nil {
		return zero, err
	}
	var release githubRelease
	if err := json.Unmarshal(b, &release); err != nil {
		return zero, problem(ErrUpstream, "invalid GitHub release response")
	}
	if _, ok := eligible(release, c); !ok || release.Tag != "v"+requested {
		return zero, problem(ErrVerification, "release is a draft, outside the selected channel, or has a different tag")
	}
	mf, raw, sig, err := m.releaseManifest(ctx, release)
	if err != nil {
		return zero, err
	}
	if mf.Schema != m.opts.Schema {
		return zero, problem(ErrConflict, "release database schema %d differs from installed schema %d; a manual migration plan is required", mf.Schema, m.opts.Schema)
	}
	asset, err := mf.asset(m.arch)
	if err != nil {
		return zero, err
	}
	t := &Transaction{ID: newID(), Version: requested, PreviousVersion: previous, Schema: mf.Schema, Channel: c}
	j.Transaction = t
	if err := transition(r.Root, &j, Downloading, "Downloading a signed official panel release"); err != nil {
		return *t, err
	}
	dir := stageDir(t)
	if err := r.Mkdir(dir, 0700); err != nil {
		return *t, fail(r.Root, &j, err)
	}
	for name, data := range map[string][]byte{"release.json": raw, "release.json.sig": sig} {
		if err := atomicWrite(r.Root, dir+"/"+name, data, 0600); err != nil {
			return *t, fail(r.Root, &j, err)
		}
	}
	if err := m.download(ctx, r.Root, dir+"/archive.tar.gz", requested, asset); err != nil {
		return *t, fail(r.Root, &j, err)
	}
	hash, err := m.extract(ctx, r.Root, dir+"/archive.tar.gz", dir+"/payload", asset)
	if err != nil {
		return *t, fail(r.Root, &j, err)
	}
	t.BinarySHA256 = hash
	if err := ctx.Err(); err != nil {
		return *t, fail(r.Root, &j, err)
	}
	if err := syncDir(r.Root, dir); err != nil {
		return *t, fail(r.Root, &j, err)
	}
	if err := syncDir(r.Root, "updates/staging"); err != nil {
		return *t, fail(r.Root, &j, err)
	}
	if err := transition(r.Root, &j, Staged, "Signature, archive hash, and executable architecture verified; ready for independent helper"); err != nil {
		return *t, err
	}
	return *t, nil
}
