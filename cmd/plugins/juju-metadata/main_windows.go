// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package main

import (
	"os"

	"github.com/juju/utils/featureflag"

	"github.com/juju/juju/juju/osenv"
)

func init() {
	featureflag.SetFlagsFromRegistry(osenv.JujuRegistryKey, osenv.JujuFeatureFlagEnvKey)
}

func main() {
	Main(os.Args)
}
