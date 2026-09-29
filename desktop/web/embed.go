// Package web embeds the Probe Desktop interface.
//
// The pages ship inside the binary: the application works offline and loads
// nothing from the network, which the Content-Security-Policy enforces.
package web

import "embed"

// Assets holds the served files under public/.
//
//go:embed public
var Assets embed.FS
