package servicehelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

const testID = "0123456789abcdef0123456789abcdef"

func TestHTTPRefusesRootIdentity(t *testing.T) {
	err := RefuseRootHTTP()
	root := os.Getuid() == 0 || os.Geteuid() == 0
	if root != (err != nil) {
		t.Fatalf("root HTTP gate: root=%v error=%v", root, err)
	}
}

func testPolicy() policy {
	return policy{PanelUser: "new-empire-control", ManagedRoot: "/srv/new-empire/instances", AdoptRoots: []string{"/srv/adopt"}, JavaPaths: []string{"/usr/lib/jvm/approved/bin/java"}, MaxMemoryMB: 8192}
}

func testInstance() model.Instance {
	return model.Instance{ID: testID, Name: "Test server", MinecraftVersion: "metadata-selected", LoaderVersion: "metadata-selected", CreatedAt: time.Unix(1700000000, 0).UTC(), Root: "/srv/new-empire/instances/" + testID, JavaPath: "/usr/lib/jvm/approved/bin/java", Jar: "fabric-server-launch.jar", MemoryMB: 2048, Port: 25565, RCONPort: 25575, EULA: true}
}

func TestRequestValidation(t *testing.T) {
	for _, args := range [][]string{
		{"start", testID}, {"stop", testID}, {"restart", testID}, {"status", testID}, {"logs", testID}, {"logs", testID, "1000"}, {"panel-update"}, {"protect", testID}, {"provision"}, {"reconfigure", testID}, {"prepare", testID},
	} {
		if _, err := parseRequest(args); err != nil {
			t.Errorf("valid args %q: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"start"}, {"start", "../../etc/passwd"}, {"start", strings.ToUpper(testID)}, {"start", testID, "/bin/sh"}, {"restart", testID, "--force"}, {"logs", testID, "0"}, {"logs", testID, "1001"}, {"logs", testID, "+5"}, {"logs", testID, "05"}, {"logs", testID, "2", "more"}, {"panel-update", testID}, {"provision", testID}, {"/bin/sh", testID},
	} {
		if _, err := parseRequest(args); err == nil {
			t.Errorf("accepted unsafe args %q", args)
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	p := testPolicy()
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*policy){
		"root account":      func(p *policy) { p.PanelUser = "root" },
		"account injection": func(p *policy) { p.PanelUser = "user\nUser=root" },
		"unsafe root":       func(p *policy) { p.ManagedRoot = "/etc/minecraft" },
		"broad root":        func(p *policy) { p.ManagedRoot = "/var" },
		"root traversal":    func(p *policy) { p.ManagedRoot = "/srv/a/../b" },
		"root overlap":      func(p *policy) { p.AdoptRoots = []string{p.ManagedRoot + "/adopt"} },
		"empty java":        func(p *policy) { p.JavaPaths = nil },
		"java arguments":    func(p *policy) { p.JavaPaths = []string{"/usr/bin/java -agentpath=evil"} },
		"java duplicate":    func(p *policy) { p.JavaPaths = append(p.JavaPaths, p.JavaPaths[0]) },
		"memory":            func(p *policy) { p.MaxMemoryMB = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			p := testPolicy()
			mutate(&p)
			if p.validate() == nil {
				t.Fatal("accepted unsafe root policy")
			}
		})
	}
}

func TestInstanceValidation(t *testing.T) {
	p := testPolicy()
	if err := validateInstance(p, testInstance()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*model.Instance){
		"root escape":              func(i *model.Instance) { i.Root = "/etc" },
		"wrong managed directory":  func(i *model.Instance) { i.Root = "/srv/new-empire/instances/other" },
		"external not adopted":     func(i *model.Instance) { i.Root = "/srv/adopt/server" },
		"adopt escape":             func(i *model.Instance) { i.Adopted = true; i.Root = "/srv/adopt-other/server" },
		"adopt allowroot itself":   func(i *model.Instance) { i.Adopted = true; i.Root = "/srv/adopt" },
		"arbitrary executable":     func(i *model.Instance) { i.JavaPath = "/bin/sh" },
		"executable arguments":     func(i *model.Instance) { i.JavaPath += " --bad" },
		"jar traversal":            func(i *model.Instance) { i.Jar = "../outside.jar" },
		"jar normalized traversal": func(i *model.Instance) { i.Jar = "dir/../outside.jar" },
		"jar absolute":             func(i *model.Instance) { i.Jar = "/etc/evil.jar" },
		"jar specifier":            func(i *model.Instance) { i.Jar = "%n.jar" },
		"jar newline":              func(i *model.Instance) { i.Jar = "evil.jar\nExecStart=/bin/sh" },
		"jar argfile":              func(i *model.Instance) { i.Jar = "@args.jar" },
		"memory overflow":          func(i *model.Instance) { i.MemoryMB = 8193 },
		"privileged port":          func(i *model.Instance) { i.Port = 22 },
		"same ports":               func(i *model.Instance) { i.RCONPort = i.Port },
		"eula":                     func(i *model.Instance) { i.EULA = false },
		"id":                       func(i *model.Instance) { i.ID = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			i := testInstance()
			mutate(&i)
			if validateInstance(p, i) == nil {
				t.Fatal("accepted unsafe instance")
			}
		})
	}
	i := testInstance()
	i.Adopted = true
	i.Root = "/srv/adopt/server"
	if err := validateInstance(p, i); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedJSON(t *testing.T) {
	for _, text := range []string{`{"unknown":"secret"}`, `{} {}`, `{"id":"x"} trailing`, `{"id":"a","ID":"b"}`, strings.Repeat(" ", maxJSON+1)} {
		var i model.Instance
		if err := decode(strings.NewReader(text), &i); err == nil {
			t.Fatal("accepted invalid/oversize JSON")
		}
	}
	var i model.Instance
	data, _ := json.Marshal(testInstance())
	if err := decode(bytes.NewReader(data), &i); err != nil {
		t.Fatal(err)
	}
}

func TestUnitHasOnlyFixedArguments(t *testing.T) {
	i := testInstance()
	i.Name = "\nExecStart=/bin/sh"
	i.MinecraftVersion = "$(command)"
	text := string(renderUnit(i))
	for _, want := range []string{
		"ExecStart=" + i.JavaPath + " -Xms256M -Xmx2048M -jar ./fabric-server-launch.jar nogui\n",
		"ExecStartPre=+" + controlPath + " runtime-helper protect " + testID,
		"ReadWritePaths=" + i.Root + "\n", "SendSIGKILL=no\n", "TimeoutStopSec=120s\n", "NoNewPrivileges=yes\n", "CapabilityBoundingSet=\n", "User=" + accountName(testID) + "\n",
		"MemoryMax=3072M\n", "MemorySwapMax=0\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(text, "/bin/sh") || strings.Contains(text, "$(command)") {
		t.Fatal("untrusted metadata entered unit")
	}
	if len(accountName(testID)) > 31 {
		t.Fatal("account name is not portable")
	}
}

type runCall struct {
	path  string
	input []byte
	args  []string
}
type fakeRunner struct {
	calls  []runCall
	handle func(runCall) ([]byte, error)
}

func (r *fakeRunner) run(_ context.Context, path string, input []byte, args ...string) ([]byte, error) {
	c := runCall{path: path, input: input, args: args}
	r.calls = append(r.calls, c)
	if r.handle != nil {
		return r.handle(c)
	}
	return nil, nil
}

func firewallJSON(port int) []byte {
	return []byte(fmt.Sprintf(`{"nftables":[{"metainfo":{"version":"test"}},{"table":{"family":"inet","name":"nec_%s","handle":1}},{"chain":{"family":"inet","table":"nec_%s","name":"rcon","type":"filter","hook":"input","prio":-150,"policy":"accept"}},{"rule":{"family":"inet","table":"nec_%s","chain":"rcon","expr":[{"match":{"op":"!=","left":{"meta":{"key":"iifname"}},"right":"lo"}},{"match":{"op":"==","left":{"payload":{"protocol":"tcp","field":"dport"}},"right":%d}},{"drop":null}]}}]}`, testID, testID, testID, port))
}

func TestFirewallFailsClosed(t *testing.T) {
	good := firewallJSON(25575)
	if !validFirewall(good, "nec_"+testID, 25575) {
		t.Fatal("valid firewall rejected")
	}
	for name, data := range map[string][]byte{
		"empty": nil, "wrong port": firewallJSON(25576), "accept rule": bytes.ReplaceAll(good, []byte(`"drop":null`), []byte(`"accept":null`)), "wrong hook": bytes.ReplaceAll(good, []byte(`"input"`), []byte(`"forward"`)), "wrong priority": bytes.ReplaceAll(good, []byte(`-150`), []byte(`0`)), "wrong interface": bytes.ReplaceAll(good, []byte(`"lo"`), []byte(`"eth0"`)),
	} {
		t.Run(name, func(t *testing.T) {
			if validFirewall(data, "nec_"+testID, 25575) {
				t.Fatal("unsafe firewall accepted")
			}
		})
	}
	r := &fakeRunner{handle: func(c runCall) ([]byte, error) { return nil, errors.New("blocked") }}
	_, err := dispatch(context.Background(), r, request{op: "start", id: testID}, manifest{Instance: testInstance()})
	if err == nil {
		t.Fatal("start succeeded without RCON protection")
	}
	for _, c := range r.calls {
		if c.path == systemctl {
			t.Fatal("systemctl invoked after failed protection")
		}
	}
	r = &fakeRunner{handle: func(c runCall) ([]byte, error) {
		if c.path == nft {
			return []byte(`{"nftables":[]}`), nil
		}
		return nil, nil
	}}
	if _, err = dispatch(context.Background(), r, request{op: "restart", id: testID}, manifest{Instance: testInstance()}); err == nil {
		t.Fatal("restart accepted unverifiable rules")
	}
}

func TestFirewallInstallAndLifecycle(t *testing.T) {
	lists := 0
	r := &fakeRunner{handle: func(c runCall) ([]byte, error) {
		if c.path == nft && len(c.input) == 0 {
			lists++
			if lists == 1 {
				return nil, errors.New("absent")
			}
			return firewallJSON(25575), nil
		}
		return nil, nil
	}}
	_, err := dispatch(context.Background(), r, request{op: "start", id: testID}, manifest{Instance: testInstance()})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 4 || r.calls[3].path != systemctl || strings.Join(r.calls[3].args, " ") != "start "+unitName(testID) {
		t.Fatalf("unexpected calls: %#v", r.calls)
	}
	if !strings.Contains(string(r.calls[1].input), `iifname != "lo" tcp dport 25575 drop`) {
		t.Fatal("filter does not enforce loopback-only RCON")
	}
	if bytes.Contains(r.calls[1].input, []byte("25565")) {
		t.Fatal("filter changed game port")
	}
}

func TestFixedStatusLogsAndUpdater(t *testing.T) {
	for _, op := range []string{"stop", "status", "logs", "panel-update"} {
		t.Run(op, func(t *testing.T) {
			r := &fakeRunner{handle: func(c runCall) ([]byte, error) {
				if len(c.args) > 1 && c.args[1] == "--property=LoadState" {
					return []byte("loaded\n"), nil
				}
				return []byte("output"), nil
			}}
			_, err := dispatch(context.Background(), r, request{op: op, id: testID, lines: 33}, manifest{Instance: testInstance()})
			if err != nil {
				t.Fatal(err)
			}
			last := r.calls[len(r.calls)-1]
			args := strings.Join(last.args, " ")
			switch op {
			case "stop":
				if args != "stop "+unitName(testID) {
					t.Fatal(args)
				}
			case "status":
				if args != "show --property="+statusFields+" "+unitName(testID) {
					t.Fatal(args)
				}
			case "logs":
				if last.path != journalctl || args != "--unit="+unitName(testID)+" --lines=33 --output=json --no-pager --quiet" {
					t.Fatal(args)
				}
			case "panel-update":
				if args != "start --no-block new-empire-update.service" {
					t.Fatal(args)
				}
			}
		})
	}
	r := &fakeRunner{handle: func(runCall) ([]byte, error) { return []byte("not-found\n"), nil }}
	if _, err := dispatch(context.Background(), r, request{op: "status", id: testID}, manifest{}); err == nil {
		t.Fatal("missing unit masqueraded as offline")
	}
}

func TestOutputBound(t *testing.T) {
	var b boundedBuffer
	if _, err := b.Write(make([]byte, maxOutput)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("output limit not enforced")
	}
}
