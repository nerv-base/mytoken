//go:build !demo

package main

import "github.com/nerv-base/mytoken/internal/gui"

const demoBuild = false

func demoDeps(d gui.Deps) gui.Deps { return d }
