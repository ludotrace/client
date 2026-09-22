//go:build headless

package updater

// platformVariant keeps a headless build from updating itself into the tray
// build. Both are linux/amd64, but the tray build links libayatana-appindicator3
// and cannot start on a host that lacks it — a Steam Deck — so downloading it
// over a working headless binary would break the daemon on the next restart,
// exactly where nobody is watching.
//
// An older manifest simply has no "linux/amd64-headless" key, and a missing key
// is already a skip rather than a fallback, so the failure mode is "no auto
// update" rather than "wrong binary".
const platformVariant = "-headless"
