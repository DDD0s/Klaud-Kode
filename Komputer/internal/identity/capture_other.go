//go:build !windows && !linux && !darwin

package identity

import "fmt"

func capturePlatform(*Profile) error {
	return fmt.Errorf("identity capture is not implemented for this operating system")
}
