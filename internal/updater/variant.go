//go:build !headless

package updater

// platformVariant distinguishes builds that share a GOOS/GOARCH but are not
// interchangeable binaries. Empty for the default build, whose manifest key is
// the plain "linux/amd64".
const platformVariant = ""
