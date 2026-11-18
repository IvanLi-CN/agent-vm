package native

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const HostObservationSocket = "/run/agent-vm-observe/observe.sock"
const hostObservationLimit = 1 << 20

type HostObservationRequest struct {
	Slot      string `json:"slot"`
	Operation string `json:"operation"`
}

func (r HostObservationRequest) Validate() error {
	if r.Slot != "blue" && r.Slot != "green" {
		return errors.New("invalid observation slot")
	}
	switch r.Operation {
	case "pool-capacity", "image-storage", "network-leases", "incus-quiet":
		return nil
	default:
		return errors.New("invalid host observation")
	}
}

type HostObservationResult struct {
	Capacity  StorageCapacity `json:"capacity"`
	Available *int64          `json:"available,omitempty"`
	Addresses []string        `json:"addresses"`
	Quiet     bool            `json:"quiet"`
}

func (c *IncusClient) readHostObservation(ctx context.Context, operation string) (HostObservationResult, error) {
	request := HostObservationRequest{Slot: c.HostObservationSlot, Operation: operation}
	if err := request.Validate(); err != nil {
		return HostObservationResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", HostObservationSocket)
	if err != nil {
		return HostObservationResult{}, errors.New("cannot connect to the host observation socket")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return HostObservationResult{}, err
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return HostObservationResult{}, err
	}
	data, err := io.ReadAll(io.LimitReader(conn, hostObservationLimit+1))
	var value *HostObservationResult
	if err != nil || len(data) > hostObservationLimit || json.Unmarshal(data, &value) != nil || value == nil {
		return HostObservationResult{}, errors.New("host observation could not be verified")
	}
	switch operation {
	case "pool-capacity":
		if value.Capacity.TotalBytes <= 0 || value.Capacity.FreeBytes < 0 || value.Capacity.FreeBytes > value.Capacity.TotalBytes {
			return HostObservationResult{}, errors.New("invalid host pool capacity")
		}
	case "image-storage":
		if value.Available == nil || *value.Available < 0 {
			return HostObservationResult{}, errors.New("invalid host image capacity")
		}
	case "network-leases":
		if value.Addresses == nil {
			return HostObservationResult{}, errors.New("host network leases are missing")
		}
	case "incus-quiet":
		if !value.Quiet {
			return HostObservationResult{}, errors.New("native background operations are not clear")
		}
	}
	return *value, nil
}
