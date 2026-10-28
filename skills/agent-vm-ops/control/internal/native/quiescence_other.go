//go:build !linux

package native

import (
	"context"
	"errors"
)

func (*IncusClient) VerifyRecoveryQuiescence(context.Context) error {
	return errors.New("native recovery quiescence is supported only on Linux")
}
