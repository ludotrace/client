//go:build !headless

package main

// builtHeadless reports whether this binary was compiled without tray support.
// False here: the default build links the tray, so headless is opt-in per run
// via --headless.
const builtHeadless = false
