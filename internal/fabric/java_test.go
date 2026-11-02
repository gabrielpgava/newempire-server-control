package fabric

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJavaVersions(t *testing.T) {
	for _, tc := range []struct {
		output  string
		major   int
		version string
	}{
		{`java version "1.8.0_422"`, 8, "1.8.0_422"},
		{"openjdk version \"17.0.14\" 2025-01-21\nOpenJDK Runtime Environment", 17, "17.0.14"},
		{`openjdk version "21.0.6+7-LTS"`, 21, "21.0.6+7-LTS"},
		{`openjdk version "25-ea" 2025-09-16`, 25, "25-ea"},
		{`java version "99"`, 99, "99"},
	} {
		version, major, err := ParseJavaVersion(tc.output)
		if err != nil || major != tc.major || version != tc.version {
			t.Fatalf("%q -> %q %d %v", tc.output, version, major, err)
		}
	}
	for _, output := range []string{"", "go version go1.27", `openjdk version "bad"`, `openjdk version "0.1"`, `openjdk version "9999999999999"`, `openjdk version "1.99999"`, strings.Repeat("x", 64<<10+1)} {
		if _, _, err := ParseJavaVersion(output); err == nil {
			t.Fatalf("accepted %q", output[:min(len(output), 100)])
		}
	}
}

func TestChooseJavaPrefersSmallestCompatibleWorkingMajor(t *testing.T) {
	installations := []JavaInstallation{{Path: "/jvm/25/bin/java", Major: 25}, {Path: "/jvm/17/bin/java", Major: 17}, {Path: "/jvm/21/bin/java", Major: 21}, {Path: "/broken/java", Major: 21, Error: "failed"}}
	for _, tc := range []struct{ minimum, want int }{{8, 17}, {17, 17}, {18, 21}, {21, 21}, {25, 25}} {
		chosen, err := ChooseJava(installations, tc.minimum)
		if err != nil || chosen.Major != tc.want {
			t.Fatalf("minimum %d: %+v %v", tc.minimum, chosen, err)
		}
	}
	for _, minimum := range []int{0, 26, 999} {
		if _, err := ChooseJava(installations, minimum); err == nil {
			t.Fatal("accepted incompatible/malformed minimum")
		}
	}
}

func TestJavaInspectionOnlyExecutesExplicitAllowlist(t *testing.T) {
	for _, path := range []string{"java", "./java", "/usr/bin/java"} {
		if _, err := InspectJava(t.Context(), path, nil); err == nil {
			t.Fatalf("executed unapproved %s", path)
		}
	}
	if _, err := InspectJavaInstallations(t.Context(), make([]string, 33)); err == nil {
		t.Fatal("accepted unbounded allowlist")
	}
	if _, err := InspectJavaInstallations(t.Context(), nil); err == nil {
		t.Fatal("guessed a Java installation from an empty allowlist")
	}
	installations, err := InspectJavaInstallations(t.Context(), []string{"/missing/java", "/missing/java"})
	if err != nil || len(installations) != 1 || installations[0].Error == "" {
		t.Fatalf("lost failed inspection: %+v %v", installations, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectJavaInstallations(ctx, []string{"/missing/java"}); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestJavaInspectionUsesVersionOnlyAndSanitizedEnvironment(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "main.go")
	program := `package main
import("fmt";"os";"path/filepath";"strings";"time")
func main(){
 if len(os.Args)!=2 || os.Args[1]!="-version" || os.Getenv("JAVA_TOOL_OPTIONS")!="" || os.Getenv("JDK_JAVA_OPTIONS")!="" { os.Exit(4) }
 switch filepath.Base(os.Args[0]) {
 case "java-failure": fmt.Fprintln(os.Stderr,"secret-output"); os.Exit(7)
 case "java-big": fmt.Fprint(os.Stderr,strings.Repeat("x",65537)); return
 case "java-unknown": fmt.Fprintln(os.Stderr,"not a recognized version"); return
 case "java-timeout": time.Sleep(10*time.Second)
 }
 fmt.Fprintln(os.Stderr, "openjdk version \"23.0.2\"")
}`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "java")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", executable, source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake Java executable: %v: %s", err, output)
	}
	t.Setenv("JAVA_TOOL_OPTIONS", "must-not-be-inherited")
	t.Setenv("JDK_JAVA_OPTIONS", "must-not-be-inherited")
	installation, err := InspectJava(t.Context(), executable, []string{executable})
	if err != nil || installation.Major != 23 || installation.Version != "23.0.2" {
		t.Fatalf("%+v %v", installation, err)
	}
	for _, mode := range []string{"java-failure", "java-big", "java-unknown", "java-timeout"} {
		path := filepath.Join(root, mode)
		if err := os.Link(executable, path); err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		if mode == "java-timeout" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
		}
		_, err := InspectJava(ctx, path, []string{path})
		if err == nil || strings.Contains(err.Error(), "secret-output") {
			t.Fatalf("unsafe Java probe failure %s: %v", mode, err)
		}
		if mode == "java-timeout" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lost cancellation: %v", err)
		}
	}
	if err := os.Chmod(executable, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectJava(t.Context(), executable, []string{executable}); err == nil {
		t.Fatal("executed writable Java binary")
	}
}
