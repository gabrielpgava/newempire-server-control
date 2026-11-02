package web

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDistributionContainsBuiltEntryAndAssets(t *testing.T) {
	files := FS()
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `id="root"`) || !strings.Contains(string(index), "/assets/") {
		t.Fatal("embedded index is not the built administration application")
	}
	assets, err := fs.ReadDir(files, "assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("embedded build assets are missing: %v", err)
	}
}
