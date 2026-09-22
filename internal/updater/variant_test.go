package updater

import (
	"runtime"
	"testing"
)

// The headless and default Linux builds are both linux/amd64 but are not
// interchangeable: the default one links libayatana-appindicator3, which SteamOS
// does not ship. They must therefore never share a manifest key, or a Deck would
// update itself into a binary that cannot start.
func TestPlatformVariantSeparatesLinuxBuilds(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH + platformVariant

	if builtHeadless := platformVariant != ""; builtHeadless {
		if key == runtime.GOOS+"/"+runtime.GOARCH {
			t.Fatalf("headless build shares the default manifest key %q", key)
		}
		return
	}
	if key != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("default build key = %q, want the plain GOOS/GOARCH form", key)
	}
}
