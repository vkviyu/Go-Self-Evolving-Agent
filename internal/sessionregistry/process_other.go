//go:build !darwin && !linux

package sessionregistry

// No portable process-liveness proof: fail closed until the caller explicitly
// reopens the session and reconciles its runtime owner.
func processAlive(pid int) (bool, error) { return true, nil }
