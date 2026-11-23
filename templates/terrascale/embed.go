// Package terrascale embeds the guest startup script shared between the CLI
// binary and the TerraScale Agent37 template image.
package terrascale

import _ "embed"

//go:embed startup.py
var StartupScript []byte
