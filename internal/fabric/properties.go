package fabric

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const MaxPropertiesBytes = 1 << 20

type propertyRecord struct {
	raw      string
	key      string
	value    string
	property bool
}

// ReadProperties implements Java properties escapes, continuations, comments,
// and last-key-wins semantics without discarding the original document.
func ReadProperties(data []byte) (map[string]string, error) {
	records, err := propertyRecords(data)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, record := range records {
		if record.property {
			values[record.key] = record.value
		}
	}
	return values, nil
}

// UpdateProperties retains untouched records byte-for-byte, including comments,
// unknown keys, CRLF, and duplicate keys. Only the last occurrence is replaced.
func UpdateProperties(data []byte, changes map[string]string) ([]byte, error) {
	records, err := propertyRecords(data)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(changes))
	for key, value := range changes {
		if err := ValidateProperty(key, value); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	last := make(map[string]int)
	for i, record := range records {
		if record.property {
			last[record.key] = i
		}
	}
	newline := "\n"
	if strings.Contains(string(data), "\r\n") {
		newline = "\r\n"
	} else if strings.Contains(string(data), "\r") && !strings.Contains(string(data), "\n") {
		newline = "\r"
	}
	for _, key := range keys {
		if i, exists := last[key]; exists {
			if records[i].value == changes[key] {
				continue
			}
			ending := ""
			if strings.HasSuffix(records[i].raw, "\r\n") {
				ending = "\r\n"
			} else if strings.HasSuffix(records[i].raw, "\n") {
				ending = "\n"
			} else if strings.HasSuffix(records[i].raw, "\r") {
				ending = "\r"
			}
			records[i].raw = escapeProperty(key) + "=" + escapeProperty(changes[key]) + ending
		}
	}
	var result strings.Builder
	for _, record := range records {
		result.WriteString(record.raw)
	}
	for _, key := range keys {
		if _, exists := last[key]; exists {
			continue
		}
		if result.Len() > 0 && !strings.HasSuffix(result.String(), "\n") && !strings.HasSuffix(result.String(), "\r") {
			result.WriteString(newline)
		}
		result.WriteString(escapeProperty(key) + "=" + escapeProperty(changes[key]) + newline)
	}
	if result.Len() > MaxPropertiesBytes {
		return nil, errors.New("server.properties exceeds size limit")
	}
	return []byte(result.String()), nil
}

var propertyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

func ValidateProperty(key, value string) error {
	if !propertyKeyPattern.MatchString(key) || len(value) > 8192 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("property key or value is invalid or exceeds its size limit")
	}
	switch key {
	case "enable-rcon", "enable-query", "enable-command-block", "enable-jmx-monitoring", "enable-status", "broadcast-rcon-to-ops", "broadcast-console-to-ops", "online-mode", "white-list", "enforce-whitelist", "pvp", "hardcore", "allow-flight", "allow-nether", "spawn-animals", "spawn-monsters", "spawn-npcs", "generate-structures", "use-native-transport", "sync-chunk-writes", "prevent-proxy-connections", "enforce-secure-profile", "force-gamemode", "hide-online-players", "require-resource-pack", "accepts-transfers":
		if value != "true" && value != "false" {
			return fmt.Errorf("property %s requires true or false", key)
		}
	}
	switch key {
	case "server-port", "rcon.port", "query.port":
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("property %s requires a port from 1 to 65535", key)
		}
	case "rcon.password":
		if len(value) < 16 || len(value) > 1446 {
			return errors.New("RCON password must contain 16 to 1446 bytes")
		}
	case "level-name":
		if value == "" || len(value) > 255 || strings.ContainsAny(value, "/\\") || value == "." || value == ".." {
			return errors.New("level-name must be a single safe relative folder name")
		}
	case "server-ip":
		if value != "" && net.ParseIP(value) == nil {
			return errors.New("server-ip must be empty or a numeric IP address")
		}
	case "max-players":
		count, err := strconv.Atoi(value)
		if err != nil || count < 1 || count > 100000 {
			return errors.New("max-players must be from 1 to 100000")
		}
	case "gamemode":
		if value != "survival" && value != "creative" && value != "adventure" && value != "spectator" {
			return errors.New("invalid gamemode")
		}
	case "difficulty":
		if value != "peaceful" && value != "easy" && value != "normal" && value != "hard" {
			return errors.New("invalid difficulty")
		}
	}
	return nil
}

// ManagedProperties sets the ports/credential used by the runtime adapter. It
// does not bind/firewall RCON: the deployment must deny non-loopback access.
func ManagedProperties(inst model.Instance, existing []byte) ([]byte, error) {
	if inst.Port == inst.RCONPort {
		return nil, errors.New("Minecraft and RCON ports must be distinct")
	}
	return UpdateProperties(existing, map[string]string{
		"server-port":           strconv.Itoa(inst.Port),
		"enable-rcon":           "true",
		"rcon.port":             strconv.Itoa(inst.RCONPort),
		"rcon.password":         inst.RCONSecret,
		"broadcast-rcon-to-ops": "false",
	})
}

func propertyRecords(data []byte) ([]propertyRecord, error) {
	if len(data) > MaxPropertiesBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return nil, errors.New("invalid UTF-8 server.properties or size limit exceeded")
	}
	text := string(data)
	records := []propertyRecord{}
	for offset := 0; offset < len(text); {
		start := offset
		var logical strings.Builder
		first := true
		for {
			end := offset
			for end < len(text) && text[end] != '\n' && text[end] != '\r' {
				end++
			}
			line := text[offset:end]
			offset = end
			if offset < len(text) && text[offset] == '\r' {
				offset++
			}
			if offset < len(text) && text[offset] == '\n' {
				offset++
			}
			trimmed := strings.TrimLeft(line, " \t\f")
			comment := first && (strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!"))
			if !first {
				line = trimmed
			}
			slashes := 0
			for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
				slashes++
			}
			continued := !comment && slashes%2 == 1
			if continued {
				line = line[:len(line)-1]
			}
			logical.WriteString(line)
			if !continued || offset >= len(text) {
				break
			}
			first = false
		}
		record := propertyRecord{raw: text[start:offset]}
		line := strings.TrimLeft(logical.String(), " \t\f")
		if line == "" || line[0] == '#' || line[0] == '!' {
			records = append(records, record)
			continue
		}
		keyEnd := 0
		for keyEnd < len(line) {
			if line[keyEnd] == '\\' {
				keyEnd += 2
				continue
			}
			if strings.ContainsRune("=:\t\f ", rune(line[keyEnd])) {
				break
			}
			keyEnd++
		}
		keyEnd = min(keyEnd, len(line))
		value := strings.TrimLeft(line[keyEnd:], " \t\f")
		if strings.HasPrefix(value, "=") || strings.HasPrefix(value, ":") {
			value = value[1:]
		}
		value = strings.TrimLeft(value, " \t\f")
		var err error
		record.key, err = unescapeProperty(line[:keyEnd])
		if err != nil {
			return nil, err
		}
		record.value, err = unescapeProperty(value)
		if err != nil {
			return nil, err
		}
		record.property = true
		records = append(records, record)
	}
	return records, nil
}

func unescapeProperty(value string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			result.WriteByte(value[i])
			continue
		}
		i++
		if i == len(value) {
			return "", errors.New("incomplete property escape")
		}
		switch value[i] {
		case 't':
			result.WriteByte('\t')
		case 'r':
			result.WriteByte('\r')
		case 'n':
			result.WriteByte('\n')
		case 'f':
			result.WriteByte('\f')
		case 'u':
			if i+4 >= len(value) {
				return "", errors.New("incomplete property Unicode escape")
			}
			n, err := strconv.ParseUint(value[i+1:i+5], 16, 16)
			if err != nil {
				return "", errors.New("invalid property Unicode escape")
			}
			i += 4
			r := rune(n)
			if utf16.IsSurrogate(r) {
				if r > 0xDBFF || i+6 >= len(value) || value[i+1:i+3] != `\u` {
					return "", errors.New("invalid property Unicode surrogate")
				}
				low, err := strconv.ParseUint(value[i+3:i+7], 16, 16)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", errors.New("invalid property Unicode surrogate")
				}
				r = utf16.DecodeRune(r, rune(low))
				i += 6
			}
			result.WriteRune(r)
		default:
			result.WriteByte(value[i])
		}
	}
	return result.String(), nil
}

func escapeProperty(value string) string {
	var result strings.Builder
	for _, r := range value {
		switch r {
		case '\\', '=', ':', '#', '!', ' ':
			result.WriteByte('\\')
			result.WriteRune(r)
		case '\t':
			result.WriteString(`\t`)
		case '\f':
			result.WriteString(`\f`)
		default:
			if r < 0x20 || r > 0x7E {
				for _, code := range utf16.Encode([]rune{r}) {
					fmt.Fprintf(&result, `\u%04X`, code)
				}
			} else {
				result.WriteRune(r)
			}
		}
	}
	return result.String()
}
