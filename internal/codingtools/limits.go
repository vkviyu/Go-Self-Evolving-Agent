package codingtools

import "time"

const (
	defaultReadBytes    = 256 << 10
	maximumReadBytes    = 4 << 20
	defaultListEntries  = 200
	maximumListEntries  = 2000
	defaultSearchBytes  = 512 << 10
	maximumSearchBytes  = 4 << 20
	defaultResults      = 100
	maximumResults      = 1000
	defaultShellBytes   = 256 << 10
	maximumShellBytes   = 2 << 20
	defaultShellTimeout = 120 * time.Second
)
