package webadmin

import (
	"errors"
	"net/http"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

func TestProblemFromErrorPreservesAdmissionFailureSemantics(t *testing.T) {
	tests := []struct {
		code      string
		status    int
		retryable bool
	}{
		{code: "resource_insufficient", status: http.StatusConflict},
		{code: "watermarks_unreachable", status: http.StatusServiceUnavailable, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			problem := problemFromError(protocol.NewError(test.code, "native detail", nil), "admission", "")
			if problem.Code != test.code || problem.Status != test.status || problem.Retryable != test.retryable {
				t.Fatalf("problem = %#v", problem)
			}
		})
	}
}

func TestProblemFromErrorPreservesStringAdmissionFailureCodes(t *testing.T) {
	for _, test := range []struct {
		code   string
		status int
	}{
		{code: "resource_insufficient", status: http.StatusConflict},
		{code: "watermarks_unreachable", status: http.StatusServiceUnavailable},
	} {
		t.Run(test.code, func(t *testing.T) {
			problem := problemFromError(errors.New(test.code+": native detail"), "admission", "")
			if problem.Code != test.code || problem.Status != test.status {
				t.Fatalf("problem = %#v", problem)
			}
		})
	}
}
