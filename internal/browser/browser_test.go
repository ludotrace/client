package browser

import (
	"strings"
	"testing"
)

// A URL whose query string contains "&" must reach the launcher as a single,
// verbatim argv entry on every platform. The Windows regression this guards
// against: routing through `cmd /c start <url>` let cmd.exe split on "&", so
// the sign-in URL was truncated before "&state=" and Core answered "missing
// state".
func TestBuildCmdForPreservesAmpersandURL(t *testing.T) {
	const signInURL = "https://core.ludotrace.com/auth/signin?" +
		"redirect_uri=http%3A%2F%2F127.0.0.1%3A50942%2Fcallback&state=DEADBEEF"

	for _, goos := range []string{"windows", "darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			cmd := buildCmdFor(goos, signInURL)

			var whole bool
			for _, a := range cmd.Args {
				if a == signInURL {
					whole = true
					break
				}
			}
			if !whole {
				t.Fatalf("%s: full URL not passed as a single arg; args = %q", goos, cmd.Args)
			}

			// Never route a URL through cmd.exe on Windows — its parser is the
			// thing that splits on "&".
			if goos == "windows" && strings.EqualFold(strings.TrimSuffix(cmd.Args[0], ".exe"), "cmd") {
				t.Fatalf("windows launch must not go through cmd.exe; args = %q", cmd.Args)
			}
		})
	}
}
