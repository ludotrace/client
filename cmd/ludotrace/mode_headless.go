//go:build headless

package main

// builtHeadless reports whether this binary was compiled without tray support.
//
// True here, and main ORs it with the --headless flag, so a headless build runs
// headless whether or not the flag was passed. Without that, omitting the flag
// from a unit file would reach tray.Run(), which this build stubs out — the
// daemon would exit immediately and Restart=on-failure would loop on it. The
// flag stays meaningful for the default build, which can do either.
const builtHeadless = true
