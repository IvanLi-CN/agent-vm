package protocol

import "net/http"

const DeploymentDrainSession = "release-agent-vm-deployd"
const DeploymentSlotBodyLimit = 4096

func DeploymentSlotRoute(method, path string) bool {
	switch path {
	case "/internal/v1/health", "/internal/v1/deployment-observation":
		return method == http.MethodGet
	case "/internal/v1/controller-role", "/internal/v1/traffic-role", "/internal/v1/recovery-marker", "/internal/v1/control":
		return method == http.MethodPost
	default:
		return false
	}
}

func DeploymentSlotAction(request Request) bool {
	action, _ := request["action"].(string)
	switch action {
	case "admin-metrics", "admin-status", "doctor", "drain-status":
		return true
	case "drain-enter", "drain-exit":
		return request["session_id"] == DeploymentDrainSession
	default:
		return false
	}
}
