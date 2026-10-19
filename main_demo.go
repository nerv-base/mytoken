//go:build demo

package main

import (
	"os"

	"github.com/zzstar101/mytoken/internal/gui"
)

// A `go build -tags demo` binary runs the app on synthetic data, for the
// README images: MYTOKEN_DEMO_LANG=en|zh picks the language and
// MYTOKEN_DEMO_THEME=dark|light the appearance.
const demoBuild = true

func demoDeps(d gui.Deps) gui.Deps {
	d = gui.Demo(d, os.Getenv("MYTOKEN_DEMO_LANG"))
	if t := os.Getenv("MYTOKEN_DEMO_THEME"); t != "" {
		_ = d.Prefs.SetPref(gui.PrefAppearance, t)
	}
	return d
}
