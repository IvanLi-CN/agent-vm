package native

import (
	"context"
	"errors"
)

func (c *IncusClient) HostObservation(ctx context.Context, request HostObservationRequest) (HostObservationResult, error) {
	if c.HostObservationSlot != "" {
		return HostObservationResult{}, errors.New("host observations require a local Incus client")
	}
	if err := request.Validate(); err != nil {
		return HostObservationResult{}, err
	}
	var value HostObservationResult
	var err error
	switch request.Operation {
	case "pool-capacity":
		value.Capacity, err = c.StoragePoolCapacity(ctx)
	case "image-storage":
		available, readErr := c.StoragePoolAvailableBytes(ctx)
		value.Available, err = &available, readErr
	case "network-leases":
		value.Addresses, err = c.networkLeases(ctx)
	case "incus-quiet":
		err = c.verifyIncusOperationsClear(ctx)
		value.Quiet = err == nil
	}
	return value, err
}
