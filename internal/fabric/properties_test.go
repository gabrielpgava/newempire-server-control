package fabric

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func TestPropertiesPreserveUnknownCommentsDuplicatesAndLineEndings(t *testing.T) {
	original := []byte("# Operator note\r\n! another comment\\\r\nserver-port = 25565\r\nmod.custom: untouched\\ value\r\nserver-port:25566\r\nmotd=Hello\\\r\n  world\r\nname\\ with\\ spaces=value\\:x\r\nemoji=\\uD83D\\uDE00\r\n")
	values, err := ReadProperties(original)
	if err != nil {
		t.Fatal(err)
	}
	if values["server-port"] != "25566" || values["mod.custom"] != "untouched value" || values["motd"] != "Helloworld" || values["name with spaces"] != "value:x" || values["emoji"] != "😀" {
		t.Fatalf("incorrect Java properties semantics: %+v", values)
	}
	updated, err := UpdateProperties(original, map[string]string{"server-port": "25570", "motd": "A world = 😀", "max-players": "42"})
	if err != nil {
		t.Fatal(err)
	}
	for _, preserved := range []string{"# Operator note\r\n", "! another comment\\\r\n", "server-port = 25565\r\n", "mod.custom: untouched\\ value\r\n", "name\\ with\\ spaces=value\\:x\r\n", "emoji=\\uD83D\\uDE00\r\n"} {
		if !bytes.Contains(updated, []byte(preserved)) {
			t.Errorf("lost untouched record %q", preserved)
		}
	}
	values, err = ReadProperties(updated)
	if err != nil || values["server-port"] != "25570" || values["max-players"] != "42" || values["motd"] != "A world = 😀" {
		t.Fatalf("failed update roundtrip: %+v %v", values, err)
	}
	unchanged, err := UpdateProperties(updated, map[string]string{"server-port": "25570"})
	if err != nil || !bytes.Equal(unchanged, updated) {
		t.Fatal("no-op edit changed document")
	}
}

func TestPropertiesValidationAndMalformedEscapes(t *testing.T) {
	for _, data := range []string{`motd=\u00ZZ`, `motd=\u123`, `motd=\uD800`, `motd=\uDC00`, `motd=\uD800\u0041`, "motd=\xff", "motd=a\x00b", strings.Repeat("x", MaxPropertiesBytes+1)} {
		if _, err := ReadProperties([]byte(data)); err == nil {
			t.Fatalf("accepted malformed properties %.100s", data)
		}
	}
	for _, changes := range []map[string]string{{"level-name": "../world"}, {"level-name": "/world"}, {"level-name": "a/b"}, {"rcon.port": "0"}, {"server-port": "65536"}, {"enable-rcon": "yes"}, {"rcon.password": "short"}, {"motd": "one\nline"}, {"bad=key": "value"}, {"max-players": "-1"}, {"gamemode": "superuser"}, {"server-ip": "host.example"}} {
		if _, err := UpdateProperties(nil, changes); err == nil {
			t.Fatalf("accepted invalid change keys %+v", changes)
		}
	}
	updated, err := UpdateProperties([]byte("unknown=keep"), map[string]string{"motd": "Testing #!:= \\ x", "enable-mod-custom": "arbitrary"})
	if err != nil {
		t.Fatal(err)
	}
	values, err := ReadProperties(updated)
	if err != nil || values["unknown"] != "keep" || values["motd"] != "Testing #!:= \\ x" || values["enable-mod-custom"] != "arbitrary" {
		t.Fatalf("%+v %v", values, err)
	}
}

func TestManagedPropertiesIsAnExplicitPureTransformation(t *testing.T) {
	inst := model.Instance{Port: 25565, RCONPort: 25575, RCONSecret: "a-strong-rcon-secret", Adopted: true}
	root := t.TempDir()
	inst.Root = root
	original := []byte("# Do not discard\ncustom=external\n")
	updated, err := ManagedProperties(inst, original)
	if err != nil {
		t.Fatal(err)
	}
	values, err := ReadProperties(updated)
	if err != nil || values["rcon.password"] != inst.RCONSecret || values["rcon.port"] != "25575" || values["enable-rcon"] != "true" || values["custom"] != "external" {
		t.Fatalf("%+v %v", values, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("pure transformer modified adopted files")
	}
	inst.RCONPort = inst.Port
	if _, err := ManagedProperties(inst, original); err == nil {
		t.Fatal("accepted overlapping ports")
	}
}

func FuzzProperties(f *testing.F) {
	for _, data := range [][]byte{nil, []byte("a=b\r\n#c\n"), []byte(`x=\uD83D\uDE00`), []byte("x=one\\\n two")} {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		values, err := ReadProperties(data)
		if err != nil {
			return
		}
		unchanged, err := UpdateProperties(data, nil)
		if err != nil || !bytes.Equal(data, unchanged) {
			t.Fatal("no-op update changed valid input")
		}
		_ = values
	})
}
