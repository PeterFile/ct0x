//go:build !linux && !darwin

package skills

// Fail closed until handle-relative containment and special-file opens are tested.
const readFlags = 0
const supportedPlatform = false
