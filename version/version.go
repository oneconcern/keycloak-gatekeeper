/*
Package version holds build information defined at build time
*/
package version

import (
	"fmt"
	"strconv"
	"time"
)

var (
	// Release tag
	Release = "unreleased - dev"
	// Gitsha is the git hash
	Gitsha = "no gitsha provided"
	// Compiled is the build timestamp
	Compiled = "0"
	// Version overrides default settings with some arbitrary string, if defined
	Version = ""
)

// GetVersion returns the proxy version
func GetVersion() string {
	if Version == "" {
		tm, err := strconv.ParseInt(Compiled, 10, 64)
		if err != nil {
			return "unable to parse build time"
		}
		Version = fmt.Sprintf("%s (git+sha: %s, built: %s)", Release, Gitsha, time.Unix(tm, 0).Format("02-01-2006"))
	}

	return Version
}
