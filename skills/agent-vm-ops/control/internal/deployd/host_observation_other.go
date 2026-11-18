//go:build !linux

package deployd

import (
	"errors"
	"os"
)

func RunHostObservation(*os.File) error {
	return errors.New("host observations require Linux")
}
