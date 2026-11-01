package main

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var versionFile string

// version comes from the VERSION file so Windows and Linux builds always agree.
var version = strings.TrimSpace(versionFile)