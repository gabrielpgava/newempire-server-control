package modrinth

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// This opt-in read-only smoke test uses upstream metadata, never a hard-coded Minecraft version.
func TestOfficialCatalogLive(t *testing.T) {
	if os.Getenv("MODRINTH_LIVE_TEST") != "1" {
		t.Skip("set MODRINTH_LIVE_TEST=1 to test the public official API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client := NewClient(nil)
	var project struct {
		Project
		Versions []string `json:"versions"`
	}
	if err := client.do(ctx, http.MethodGet, "/project/fabric-api", nil, &project); err != nil {
		t.Fatal(err)
	}
	if len(project.Versions) == 0 {
		t.Fatal("upstream returned no Fabric API versions")
	}
	v, err := client.Version(ctx, project.Versions[len(project.Versions)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(v.GameVersions) == 0 {
		t.Fatal("version is missing game-version metadata")
	}
	target := Target{MinecraftVersion: v.GameVersions[len(v.GameVersions)-1], Loader: "fabric"}
	if result, err := client.Search(ctx, "fabric-api", target, 0); err != nil || len(result.Hits) == 0 {
		t.Fatal("official search returned no matching Fabric server mods", result, err)
	}
	if _, err := client.Projects(ctx, []string{project.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Versions(ctx, project.ID, target, false); err != nil {
		t.Fatal(err)
	}
	file, err := PrimaryFile(v)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := client.VersionsFromHashes(ctx, []string{file.Hashes["sha512"]}); err != nil || found[file.Hashes["sha512"]].ID != v.ID {
		t.Fatal(found, err)
	}
	if _, err := client.UpdatesFromHashes(ctx, []string{file.Hashes["sha512"]}, target, false); err != nil {
		t.Fatal(err)
	}
}
