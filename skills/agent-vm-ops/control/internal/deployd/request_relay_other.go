//go:build !linux

package deployd

import (
	"errors"
	"io"
	"os"
)

const RequestRelaySocket = "/run/agent-vm-native/control.sock"

func RunRequestRelay(*os.File) error { return errors.New("request relay requires Linux") }
func RunRequestRelayChild(string, io.Reader, io.Writer) error {
	return errors.New("request relay requires Linux")
}
