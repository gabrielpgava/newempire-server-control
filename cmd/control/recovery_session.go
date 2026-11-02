package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

func validateCLIFlags(command string, flags *flag.FlagSet, args []string) error {
	allowed := map[string]bool{"config": true, "json": command != "login"}
	if command == "login" {
		for _, name := range []string{"username", "password-stdin", "code", "code-stdin"} {
			allowed[name] = true
		}
	}
	if command == "recover" {
		allowed["password-stdin"] = true
	}
	allowed["confirm"] = command == "backup" || command == "update" || command == "recover"
	allowed["independent"] = command == "update"
	allowed["offline"] = command == "backup"
	var invalid error
	flags.Visit(func(f *flag.Flag) {
		if !allowed[f.Name] {
			invalid = fmt.Errorf("--%s is not supported by %s", f.Name, command)
		}
	})
	if invalid != nil {
		return invalid
	}
	switch command {
	case "serve", "doctor", "version", "login", "update-helper":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no positional arguments; put flags before subcommands", command)
		}
	}
	return nil
}

func validateBackupArgs(args []string, confirm bool) error {
	if len(args) == 0 {
		return errors.New("backup requires create, list, verify, preview, or restore")
	}
	count := 2
	switch args[0] {
	case "create", "list":
	case "verify", "preview", "restore":
		count = 3
	default:
		return errors.New("unsupported backup subcommand")
	}
	if len(args) != count {
		return errors.New("provide exactly one instance ID, plus one backup ID for verify, preview, or restore")
	}
	for _, id := range args[1:] {
		if !model.ValidID(id) {
			return errors.New("instance and backup IDs must contain exactly 32 lowercase hexadecimal characters")
		}
	}
	if args[0] == "restore" && !confirm {
		return errors.New("restore replaces world progress; review backup preview, then repeat with --confirm before the subcommand")
	}
	return nil
}

func readCLICredentials(input io.Reader, code string, secondLine bool) (string, string, error) {
	if secondLine && code != "" {
		return "", "", errors.New("use either --code or --code-stdin, not both")
	}
	b, err := io.ReadAll(io.LimitReader(input, 2049))
	if err != nil {
		return "", "", err
	}
	if len(b) > 2048 {
		return "", "", errors.New("credential input exceeds the size limit")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	count := 1
	if secondLine {
		count = 2
	}
	if len(lines) != count {
		return "", "", errors.New("provide one password line, with a second authentication-code line only for --code-stdin")
	}
	password := strings.TrimSuffix(lines[0], "\r")
	if len(password) == 0 || len(password) > 1024 || !utf8.ValidString(password) || strings.ContainsAny(password, "\r\x00") {
		return "", "", errors.New("password must be one nonempty UTF-8 line of at most 1024 bytes")
	}
	if secondLine {
		code = strings.TrimSuffix(lines[1], "\r")
	}
	if code != "" || secondLine {
		if len(code) != 6 || strings.IndexFunc(code, func(c rune) bool { return c < '0' || c > '9' }) >= 0 {
			return "", "", errors.New("authentication code must contain exactly six digits")
		}
	}
	return password, code, nil
}

func rejectCLIRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("CLI authentication requests must not redirect; configure the exact panel origin")
}

func privateDataRoot(c config.Config, create bool) (*os.Root, error) {
	if os.Geteuid() == 0 || os.Getuid() != os.Geteuid() {
		return nil, errors.New("run this command as the dedicated panel account, not root: sudo -u new-empire-control /opt/new-empire-control/control COMMAND --config /etc/new-empire-control/config.json")
	}
	if create {
		if err := os.MkdirAll(c.DataDir, 0700); err != nil {
			return nil, err
		}
	}
	before, err := os.Lstat(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("existing panel data directory is required: %w", err)
	}
	root, err := os.OpenRoot(c.DataDir)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err == nil {
		stat, ok := after.Sys().(*syscall.Stat_t)
		if !before.IsDir() || !os.SameFile(before, after) || after.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
			err = errors.New("panel data directory must be a real, owner-only directory owned by the executing panel account")
		}
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func openPrivateFile(root *os.Root, name string) (*os.File, error) {
	f, err := files.OpenRegular(root, name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
			err = errors.New("private panel files must be owner-only and owned by the executing panel account")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func validCLISession(s cliSession) bool {
	for _, value := range []string{s.Token, s.CSRF} {
		b, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || len(b) != 32 || len(value) != 43 {
			return false
		}
	}
	return true
}

func readCLISession(c config.Config) (cliSession, error) {
	root, err := privateDataRoot(c, false)
	if err != nil {
		return cliSession{}, err
	}
	defer root.Close()
	f, err := openPrivateFile(root, "cli-session.json")
	if err != nil {
		return cliSession{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return cliSession{}, err
	}
	var session cliSession
	if len(b) > 4096 || json.Unmarshal(b, &session) != nil || !validCLISession(session) {
		return cliSession{}, errors.New("invalid CLI session file; sign in again")
	}
	return session, nil
}

func writeCLISession(c config.Config, session cliSession) error {
	if !validCLISession(session) {
		return errors.New("server returned an invalid CLI session")
	}
	root, err := privateDataRoot(c, true)
	if err != nil {
		return err
	}
	defer root.Close()
	if old, err := openPrivateFile(root, "cli-session.json"); err == nil {
		if err := old.Close(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("refusing to replace unsafe CLI session file: %w", err)
	}
	name := ".nesc-cli-session-" + model.NewID()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(name)
	b, err := json.Marshal(session)
	if err == nil {
		_, err = f.Write(b)
	}
	if err := errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := root.Rename(name, "cli-session.json"); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
