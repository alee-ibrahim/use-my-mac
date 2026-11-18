package fleet

import _ "embed"

// The managed scripts are embedded so the single `umm` binary carries
// everything it stages onto a Mac; nothing extra has to be shipped alongside.

//go:embed scripts/bootstrap.sh
var bootstrapScript []byte

//go:embed scripts/install.sh
var installScript []byte

//go:embed scripts/upgrade.sh
var upgradeScript []byte

//go:embed scripts/uninstall.sh
var uninstallScript []byte
