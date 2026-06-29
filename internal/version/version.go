package version

// Version is injected at build time via -ldflags.
// Tagged releases produce clean semver (e.g. "v1.2.3"); untagged dev
// builds produce "dev" or a git-describe string.
var Version = "dev"
