// Package winutil wraps the Windows specifics (DPAPI, named mutex, UAC
// elevation, console, browser). Non-Windows builds get portable fallbacks so
// the agent can be developed and tested on macOS/Linux.
package winutil

import "errors"

// ErrAlreadyRunning is returned by AcquireMutex when another instance holds it.
var ErrAlreadyRunning = errors.New("another SCLogSync instance is running")

// ErrElevationCancelled is returned when the user declines the UAC prompt.
var ErrElevationCancelled = errors.New("elevation cancelled by user")

// MutexName is the single-instance lock shared by console and service.
const MutexName = `Global\SCLogSync`
