//go:build linux

package servicehelper

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

func validateProperties(data []byte, gamePort, rconPort int) error {
	bad := errors.New("server.properties must explicitly enable RCON with a password and match the registered game/RCON ports; escaped keys, continuations, and duplicate keys are not supported")
	if !utf8.Valid(data) {
		return bad
	}
	props := map[string]string{}
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t\f")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			return bad
		}
		index := strings.IndexAny(line, "=: \t\f")
		if index < 1 {
			return bad
		}
		key := line[:index]
		if strings.Contains(key, "\\") {
			return bad
		}
		value := strings.TrimLeft(line[index:], " \t\f")
		if strings.HasPrefix(value, "=") || strings.HasPrefix(value, ":") {
			value = strings.TrimLeft(value[1:], " \t\f")
		}
		if _, exists := props[key]; exists {
			return bad
		}
		props[key] = value
	}
	if props["server-port"] != strconv.Itoa(gamePort) || props["rcon.port"] != strconv.Itoa(rconPort) || props["enable-rcon"] != "true" || strings.TrimSpace(props["rcon.password"]) == "" {
		return bad
	}
	for _, key := range []string{"server-port", "rcon.port", "enable-rcon", "rcon.password"} {
		if strings.Contains(props[key], "\\") {
			return bad
		}
	}
	return nil
}

func verifyProperties(m manifest) error {
	root, err := dataDir(m.Instance.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := launcherFile(root, "server.properties")
	if err != nil {
		return errors.New("server.properties must be a regular file without symlinks")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return errors.New("server.properties could not be read within its 64 KiB limit")
	}
	return validateProperties(data, m.Instance.Port, m.Instance.RCONPort)
}
