package webadmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/management"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const maxManagementJSONBytes = 256 * 1024

var managementFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type AuthorityOptions struct {
	Config         config.ManagementConfig
	Management     *management.Service
	Lifecycle      *lifecycle.Service
	Store          *state.Store
	Snapshots      *observe.SnapshotStore
	ProductVersion string
	Clock          func() time.Time
}

type Authority struct {
	Config         config.ManagementConfig
	Management     *management.Service
	Lifecycle      *lifecycle.Service
	Store          *state.Store
	Snapshots      *observe.SnapshotStore
	ProductVersion string
	Clock          func() time.Time
	uploadMu       sync.Mutex
}

func NewAuthority(options AuthorityOptions) (*Authority, error) {
	if !options.Config.Enabled {
		return nil, errors.New("management_disabled: management authority is disabled")
	}
	if err := options.Config.Validate(true); err != nil {
		return nil, err
	}
	if options.Management == nil || options.Store == nil {
		return nil, errors.New("authority dependencies are incomplete")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &Authority{
		Config:         options.Config,
		Management:     options.Management,
		Lifecycle:      options.Lifecycle,
		Store:          options.Store,
		Snapshots:      options.Snapshots,
		ProductVersion: options.ProductVersion,
		Clock:          options.Clock,
	}, nil
}

func (authority *Authority) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeProblem(c.Writer, newAPIError("internal_error", "management authority failed", http.StatusInternalServerError, "internal", true, ""))
		c.Abort()
	}))
	router.Any("/internal/v1/management/*path", func(c *gin.Context) {
		authority.handle(c.Writer, c.Request)
	})
	return router
}

func (authority *Authority) Serve(ctx context.Context) error {
	return serveUnix(ctx, authority.Config.Authority.SocketPath, authority.Handler())
}

func (authority *Authority) handle(writer http.ResponseWriter, request *http.Request) {
	actor, authErr := authority.authorize(request)
	if authErr != nil {
		writeProblem(writer, authErr)
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/internal/v1/management")
	if path == "" {
		path = "/"
	}
	authority.route(writer, request, path, actor)
}

func (authority *Authority) authorize(request *http.Request) (string, *APIError) {
	if err := requirePeerUID(request.Context(), authority.Config.Authority.ExpectedWebUID); err != nil {
		return "", newAPIError("permission_denied", "management authority accepts only the configured Web peer", http.StatusForbidden, "authorization", false, "")
	}
	actor := singleHeader(request, forwardedActorHeader)
	if actor == "" {
		return "", newAPIError("authentication_required", "administrator authentication is required", http.StatusUnauthorized, "authentication", false, "")
	}
	if !containsString(authority.Config.AllowedAdminSubjects, actor) {
		return "", newAPIError("administrator_required", "administrator access is required", http.StatusForbidden, "authorization", false, "")
	}
	return actor, nil
}

func (authority *Authority) route(writer http.ResponseWriter, request *http.Request, path, actor string) {
	switch {
	case request.Method == http.MethodGet && path == "/overview":
		authority.getOverview(writer, request)
	case request.Method == http.MethodGet && path == "/vms":
		authority.listVMs(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/vms/") && !strings.HasSuffix(path, "/actions") && !strings.HasSuffix(path, "/actions/preview"):
		authority.getVM(writer, request, strings.TrimPrefix(path, "/vms/"))
	case request.Method == http.MethodPost && strings.HasSuffix(path, "/actions/preview"):
		authority.previewVMAction(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/vms/"), "/actions/preview"), actor)
	case request.Method == http.MethodPost && strings.HasSuffix(path, "/actions"):
		authority.submitVMAction(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/vms/"), "/actions"), actor)
	case request.Method == http.MethodGet && path == "/images":
		authority.listImages(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && !strings.Contains(strings.TrimPrefix(path, "/images/"), "/"):
		authority.getImage(writer, request, strings.TrimPrefix(path, "/images/"))
	case request.Method == http.MethodPost && path == "/images/imports":
		authority.startImageImport(writer, request, actor)
	case request.Method == http.MethodPut && strings.HasPrefix(path, "/images/imports/") && strings.HasSuffix(path, "/content"):
		authority.uploadImageImport(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/images/imports/"), "/content"), actor)
	case request.Method == http.MethodPost && path == "/images/default/preview":
		authority.previewDefaultImage(writer, request, actor)
	case request.Method == http.MethodPost && path == "/images/default":
		authority.submitDefaultImage(writer, request, actor)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/retire/preview"):
		authority.previewImageMutation(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/retire/preview"), "image_retire", actor)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/retire"):
		authority.submitImageMutation(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/retire"), "image_retire", actor)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/delete/preview"):
		authority.previewImageMutation(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/delete/preview"), "image_delete", actor)
	case request.Method == http.MethodPost && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/delete"):
		authority.submitImageMutation(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/delete"), "image_delete", actor)
	case request.Method == http.MethodGet && path == "/operations":
		authority.listOperations(writer, request)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/operations/"):
		authority.getOperation(writer, request, strings.TrimPrefix(path, "/operations/"))
	case request.Method == http.MethodGet && path == "/audit":
		authority.listAudit(writer, request)
	case request.Method == http.MethodGet && path == "/system":
		authority.getSystem(writer, request)
	case request.Method == http.MethodPost && path == "/system/maintenance/preview":
		authority.previewMaintenance(writer, request, actor)
	case request.Method == http.MethodPost && path == "/system/maintenance":
		authority.submitMaintenance(writer, request, actor)
	default:
		writeProblem(writer, newAPIError("not_found", "management route was not found", http.StatusNotFound, "request_validation", false, ""))
	}
}

func (authority *Authority) getOverview(writer http.ResponseWriter, request *http.Request) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if authority.Lifecycle == nil {
		writeProblem(writer, problemFromError(errors.New("authority_unavailable: lifecycle service is not configured"), "internal", ""))
		return
	}
	if authority.Snapshots == nil {
		writeProblem(writer, problemFromError(errors.New("observation_unavailable: capacity snapshot is not configured"), "internal", ""))
		return
	}
	raw, err := authority.Snapshots.Read("capacity")
	if err != nil {
		writeProblem(writer, problemFromError(err, "request_validation", ""))
		return
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		writeProblem(writer, problemFromError(errors.New("invalid_state: capacity snapshot is invalid"), "internal", ""))
		return
	}
	metrics, ok := envelope["result"].(map[string]any)
	if !ok {
		metrics = envelope
		ok = true
	}
	if !ok {
		writeProblem(writer, newAPIError("invalid_state", "metrics response is invalid", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	overview, err := authority.overview(metrics, request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, overview)
}

func (authority *Authority) overview(metrics map[string]any, ctx context.Context) (map[string]any, error) {
	epoch, err := authority.Store.StateEpoch(ctx)
	if err != nil {
		return nil, err
	}
	now := authority.Clock().UTC()
	observedAt := stringValue(metrics["observed_at"])
	observedTime, parseErr := time.Parse(time.RFC3339Nano, observedAt)
	fresh := observedAt != "" && parseErr == nil && !observedTime.After(now.Add(5*time.Second)) && now.Sub(observedTime) <= 60*time.Second
	if observedAt == "" {
		observedAt = now.Format(time.RFC3339Nano)
	}
	resources, _ := metrics["resources"].(map[string]any)
	remaining, _ := resources["remaining"].(map[string]any)
	ctCPU, _ := metrics["ct_cpu"].(map[string]any)
	startupBlocked := nestedBool(metrics, "ct_cpu", "startup_pressure", "startup_blocked")
	capacity := map[string]any{
		"logical_cpu_remaining_vcpu": observedNumber(numberValue(remaining["cpu"]), observedAt, fresh),
		"ct_cpu_capacity_vcpu":       observedNumber(numberValue(ctCPU["capacity_vcpu"]), observedAt, fresh),
		"logical_memory_remaining":   observedBytes(decimalBytes(numberValue(remaining["memory_mib"]), 1024*1024), observedAt, fresh),
		"ct_memory_available":        observedBytes(nil, observedAt, fresh),
		// The logical admission budget is not physical disk telemetry. Keep the
		// physical field unverified until the native snapshot supplies it.
		"physical_disk_available": observedBytes(nil, observedAt, fresh),
	}
	pressure := map[string]any{
		"cpu_some_avg10":                         observedNumber(nestedNumber(resources, "cpu_pressure", "some", "avg10"), observedAt, fresh),
		"cpu_full_avg10":                         observedNumber(nestedNumber(resources, "cpu_pressure", "full", "avg10"), observedAt, fresh),
		"cpu_startup_pressure_threshold_percent": observedNumber(nestedNumber(resources, "policy", "cpu_startup_pressure_full_avg10_percent"), observedAt, fresh),
		"cpu_startup_blocked":                    observedBoolean(startupBlocked, observedAt, fresh),
		"memory_pressure":                        observedBoolean(nil, observedAt, fresh),
	}
	var admissionReady, draining any
	admissionBlocked := false
	if fresh {
		admissionReady, _ = metrics["admission_ready"].(bool)
		draining, _ = metrics["draining"].(bool)
		if value, ok := draining.(bool); ok && value {
			admissionBlocked = true
		}
	}
	reasons := []string{}
	if !fresh {
		reasons = append(reasons, "observation_stale")
	}
	if defaultAdmission, ok := metrics["default_admission"].(map[string]any); ok {
		if ready, ok := defaultAdmission["ready"].(bool); ok && !ready {
			admissionBlocked = true
		}
		if rawReasons, ok := defaultAdmission["reasons"].([]any); ok {
			for _, rawReason := range rawReasons {
				if reason, ok := rawReason.(map[string]any); ok && stringValue(reason["code"]) != "" {
					code := stringValue(reason["code"])
					reasons = append(reasons, code)
					switch code {
					case "native_unavailable", "resource_pressure", "resource_unverified", "startup_reconcile_required":
						admissionBlocked = true
					}
				}
			}
		}
	}
	if fresh {
		nativeUp, nativeOK := metrics["up"].(bool)
		if !nativeOK || !nativeUp {
			admissionBlocked = true
			if !containsString(reasons, "native_unavailable") {
				reasons = append(reasons, "native_unavailable")
			}
		}
		if admissionBlocked {
			admissionReady = false
		}
	}
	stateNames := []string{"active", "preparing", "cached", "closing", "deleting", "failed", "absent"}
	states := map[string]any{}
	durableObservedAt := observedAt
	if fresh {
		for _, name := range stateNames {
			states[name] = int64Value(metricsMapValue(metrics, "task_states", name))
		}
	} else {
		tasks, listErr := authority.Store.ListTasks(ctx)
		if listErr != nil {
			return nil, listErr
		}
		counts := make(map[string]int64, len(stateNames))
		for _, name := range stateNames {
			counts[name] = 0
		}
		for _, task := range tasks {
			if _, ok := counts[task.State]; ok {
				counts[task.State]++
			}
		}
		for _, name := range stateNames {
			states[name] = counts[name]
		}
		durableObservedAt = now.Format(time.RFC3339Nano)
	}
	durableState := "available"
	nativeSafeCode := any(nil)
	nativeUnavailable := false
	if native, ok := metrics["up"].(bool); !ok || !native {
		nativeUnavailable = true
	}
	if !fresh {
		nativeSafeCode = "observation_stale"
	} else if nativeUnavailable {
		nativeSafeCode = "native_unavailable"
	}
	return map[string]any{
		"resource_version": epoch,
		"observed_at":      observedAt,
		"admission":        map[string]any{"ready": admissionReady, "draining": draining, "reasons": reasons},
		"capacity":         capacity,
		"pressure":         pressure,
		"task_counts":      states,
		"sources": []any{
			map[string]any{"name": "durable_state", "state": durableState, "observed_at": durableObservedAt, "stale_after_seconds": 60, "safe_code": nil},
			map[string]any{"name": "native", "state": sourceStateWithFreshness(metrics["up"], fresh), "observed_at": observedAt, "stale_after_seconds": 60, "safe_code": nativeSafeCode},
		},
	}, nil
}

func (authority *Authority) listVMs(writer http.ResponseWriter, request *http.Request) {
	if err := validateVMQuery(request.URL.Query()); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	query := request.URL.Query()
	limit, cursor, err := pageRequest(query)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	tasks, more, err := authority.Store.ListTasksPage(request.Context(), state.TaskPageFilter{
		States: query["state"], SessionID: query.Get("session_id"), ImageFingerprint: query.Get("image_fingerprint"), Query: query.Get("q"),
		LeaseState: query.Get("lease_state"), LeaseNow: authority.Clock().UTC().Format(time.RFC3339Nano), AfterTaskID: cursor, Limit: limit,
	})
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	items := make([]any, 0, len(tasks))
	for index := range tasks {
		item, itemErr := authority.vmValue(request.Context(), &tasks[index])
		if itemErr != nil {
			writeProblem(writer, problemFromError(itemErr, "internal", ""))
			return
		}
		items = append(items, item)
	}
	nextKey := ""
	if len(tasks) > 0 {
		nextKey = tasks[len(tasks)-1].TaskID
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "page": pageValue(limit, more, nextKey)})
}

func (authority *Authority) getVM(writer http.ResponseWriter, request *http.Request, taskID string) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if !protocol.TaskIDPattern.MatchString(taskID) || len(taskID) > 128 {
		writeProblem(writer, invalidRequest("task_id is invalid"))
		return
	}
	task, err := authority.Store.GetTask(request.Context(), map[string]any{"task_id": taskID})
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	if task == nil {
		writeProblem(writer, newAPIError("not_found", "the requested VM was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	value, err := authority.vmValue(request.Context(), task)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func (authority *Authority) vmValue(ctx context.Context, task *state.Task) (map[string]any, error) {
	ports, err := authority.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return nil, err
	}
	resources := map[string]int64{}
	if task.ResourcesJSON != "" {
		if err := json.Unmarshal([]byte(task.ResourcesJSON), &resources); err != nil {
			return nil, errors.New("invalid_state: VM resources are invalid")
		}
	}
	var imageLabel *string
	if image, imageErr := authority.Management.GetImage(ctx, task.ImageFingerprint); imageErr != nil {
		return nil, imageErr
	} else if image != nil {
		imageLabel = image.VersionLabel
	}
	leaseState := "unknown"
	if task.LeaseExpiresAt == nil {
		leaseState = "absent"
	} else if parsed, parseErr := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt); parseErr == nil {
		leaseState = "expired"
		if parsed.After(authority.Clock().UTC()) {
			leaseState = "effective"
		}
	}
	allowed := []string{}
	if task.State == "active" {
		allowed = []string{"restart", "resize", "release", "destroy", "port_add"}
		for _, port := range ports {
			if port.PortName != "ssh" {
				allowed = append(allowed, "port_remove")
				break
			}
		}
	} else if task.State == "preparing" || task.State == "cached" || task.State == "closing" || task.State == "failed" {
		allowed = []string{"release", "destroy"}
	}
	var lastError any
	if task.LastError != nil && strings.TrimSpace(*task.LastError) != "" {
		lastError = map[string]any{"code": safeErrorCode(errors.New(*task.LastError)), "phase": "internal", "retryable": false}
	}
	portValues := make([]any, 0, len(ports))
	for _, port := range ports {
		portValues = append(portValues, map[string]any{"name": port.PortName, "protocol": port.Protocol, "host_port": port.HostPort, "guest_port": port.VMPort})
	}
	return map[string]any{
		"task_id": task.TaskID, "session_id": task.SessionID, "generation": strconv.FormatInt(task.Generation, 10),
		"resource_version": "vm:" + task.TaskID + ":" + task.UpdatedAt, "state": task.State,
		"native_state": "unknown", "guest_availability": "unknown",
		"image":         map[string]any{"fingerprint": task.ImageFingerprint, "version_label": imageLabel},
		"resources":     map[string]any{"cpu_vcpu": numberOrNil(resources["cpu"]), "memory_bytes": decimalBytes(resources["memory_mib"], 1024*1024), "disk_bytes": decimalBytes(resources["disk_gib"], 1024*1024*1024)},
		"lease":         map[string]any{"state": leaseState, "expires_at": task.LeaseExpiresAt},
		"port_mappings": portValues, "last_error": lastError, "updated_at": task.UpdatedAt, "allowed_actions": allowed,
	}, nil
}

func (authority *Authority) listImages(writer http.ResponseWriter, request *http.Request) {
	if err := validateImageQuery(request.URL.Query()); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	query := request.URL.Query()
	limit, cursor, err := pageRequest(query)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	createdAt, fingerprint := pageKeyParts(cursor)
	images, more, selectionVersion, err := authority.Management.ListImagesPage(request.Context(), state.ImageFilter{State: query.Get("state"), Architecture: query.Get("architecture"), Query: query.Get("q"), AfterCreatedAt: createdAt, AfterFingerprint: fingerprint, Limit: limit})
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	items := make([]any, 0, len(images))
	for _, image := range images {
		items = append(items, imageValue(image))
	}
	defaultFingerprint, exists, err := authority.Store.CurrentDefaultImage(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	var defaultValue any
	if exists {
		defaultValue = defaultFingerprint
	}
	catalogVersion, err := authority.Management.ImageCatalogVersion(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	nextKey := ""
	if len(images) > 0 {
		nextKey = imagePageKey(images[len(images)-1])
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "page": pageValue(limit, more, nextKey), "default_fingerprint": defaultValue, "selection_version": selectionVersion, "catalog_version": catalogVersion})
}

func (authority *Authority) getImage(writer http.ResponseWriter, request *http.Request, fingerprint string) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if !managementFingerprintPattern.MatchString(fingerprint) {
		writeProblem(writer, invalidRequest("fingerprint must be lowercase SHA-256"))
		return
	}
	image, err := authority.Management.GetImage(request.Context(), fingerprint)
	if err != nil {
		writeProblem(writer, problemFromError(err, "request_validation", ""))
		return
	}
	if image == nil {
		writeProblem(writer, newAPIError("not_found", "the requested image was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	writeJSON(writer, http.StatusOK, imageValue(*image))
}

func (authority *Authority) listOperations(writer http.ResponseWriter, request *http.Request) {
	if err := validateOperationQuery(request.URL.Query()); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	query := request.URL.Query()
	limit, cursor, err := pageRequest(query)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	status := query.Get("status")
	phase := ""
	if status == "reconciling" {
		status = "running"
		phase = "reconciliation"
	}
	createdAt, operationID := pageKeyParts(cursor)
	values, more, err := authority.Management.ListOperationsPage(request.Context(), state.ManagementOperationFilter{
		Status: status, Phase: phase, Action: query.Get("action"), Actor: query.Get("actor"), TargetID: query.Get("target_id"), Query: query.Get("q"),
		AfterCreatedAt: createdAt, AfterOperationID: operationID, Limit: limit,
	})
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, operationValue(value))
	}
	nextKey := ""
	if len(values) > 0 {
		last := values[len(values)-1]
		nextKey = last.CreatedAt + "\x00" + last.OperationID
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "page": pageValue(limit, more, nextKey)})
}

func (authority *Authority) getOperation(writer http.ResponseWriter, request *http.Request, operationID string) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(operationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	value, err := authority.Management.GetOperation(request.Context(), operationID)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", operationID))
		return
	}
	if value == nil {
		writeProblem(writer, newAPIError("not_found", "the requested operation was not found", http.StatusNotFound, "request_validation", false, operationID))
		return
	}
	writeJSON(writer, http.StatusOK, operationValue(*value))
}

func (authority *Authority) listAudit(writer http.ResponseWriter, request *http.Request) {
	from, to, err := auditBounds(request.URL.Query())
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	query := request.URL.Query()
	limit, cursor, err := pageRequest(query)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	occurredAt, eventID := pageKeyParts(cursor)
	fromValue, toValue := "", ""
	if from != nil {
		fromValue = from.UTC().Format(time.RFC3339Nano)
	}
	if to != nil {
		toValue = to.UTC().Format(time.RFC3339Nano)
	}
	values, more, err := authority.Management.ListAuditPage(request.Context(), state.ManagementAuditFilter{
		From: fromValue, To: toValue, Actor: query.Get("actor"), Action: query.Get("action"), OperationID: query.Get("operation_id"), TargetID: query.Get("target_id"),
		AfterOccurredAt: occurredAt, AfterEventID: eventID, Limit: limit,
	})
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, auditValue(value))
	}
	nextKey := ""
	if len(values) > 0 {
		last := values[len(values)-1]
		nextKey = last.OccurredAt + "\x00" + last.EventID
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "page": pageValue(limit, more, nextKey)})
}

func (authority *Authority) getSystem(writer http.ResponseWriter, request *http.Request) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	epoch, err := authority.Store.StateEpoch(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	capabilities, err := authority.Management.Capabilities(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	drainingValue, found, err := authority.Store.Meta(request.Context(), "draining")
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	var draining any
	if found {
		draining = drainingValue == "1"
	}
	operations, err := authority.Management.ListOperations(request.Context(), 100)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	reconciliation, garbage := 0, 0
	for _, operation := range operations {
		if operation.Status == "queued" || operation.Status == "running" {
			switch operation.Action {
			case "reconcile":
				reconciliation++
			case "gc":
				garbage++
			}
		}
	}
	stateCapabilities := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		stateCapabilities = append(stateCapabilities, capability.Name+"@"+strconv.FormatInt(capability.Version, 10))
	}
	now := authority.Clock().UTC().Format(time.RFC3339Nano)
	writeJSON(writer, http.StatusOK, map[string]any{
		"product_version":    authority.ProductVersion,
		"resource_version":   "system:" + epoch,
		"state_capabilities": stateCapabilities,
		"maintenance":        map[string]any{"draining": draining, "reconciliation_pending": reconciliation, "garbage_collection_pending": garbage},
		"components": []any{
			map[string]any{"name": "web", "state": "unknown", "version": nil, "observed_at": now},
			map[string]any{"name": "authority", "state": "ready", "version": authority.ProductVersion, "observed_at": now},
			map[string]any{"name": "observer", "state": "unknown", "version": nil, "observed_at": now},
		},
		"sources": []any{map[string]any{"name": "durable_state", "state": "available", "observed_at": now, "stale_after_seconds": 60, "safe_code": nil}},
	})
}

type vmActionPayload struct {
	Action             string         `json:"action"`
	ExpectedGeneration string         `json:"expected_generation"`
	TargetVersion      string         `json:"target_version"`
	Parameters         map[string]any `json:"parameters"`
	OperationID        string         `json:"operation_id"`
	Reason             string         `json:"reason"`
	ConfirmationToken  string         `json:"confirmation_token"`
}

func (authority *Authority) previewVMAction(writer http.ResponseWriter, request *http.Request, taskID, actor string) {
	var payload vmActionPayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	parameters, err := normalizeVMParameters(payload.Action, payload.Parameters)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	task, err := authority.validateVMTarget(request.Context(), taskID, payload.Action, payload.TargetVersion, payload.ExpectedGeneration, parameters)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", ""))
		return
	}
	token, expiresAt, err := issueDurableConfirmation(request.Context(), authority.Store, authority.Clock(), actor, payload.Action, "vm:"+taskID, payload.TargetVersion, parameters)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, actionPreviewValue(payload.TargetVersion, token, expiresAt, vmImpact(task, payload.Action)))
}

func (authority *Authority) submitVMAction(writer http.ResponseWriter, request *http.Request, taskID, actor string) {
	var payload vmActionPayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateConfirmationToken(payload.ConfirmationToken); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	generation, err := parseDecimalInt(payload.ExpectedGeneration)
	if err != nil {
		writeProblem(writer, invalidRequest("expected_generation must be a canonical non-negative decimal string"))
		return
	}
	parameters, err := normalizeVMParameters(payload.Action, payload.Parameters)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	actionRequest := map[string]any{"action": payload.Action, "task_id": taskID, "target_version": payload.TargetVersion, "generation": generation, "parameters": parameters, "actor": actor, "reason": payload.Reason, "target_kind": "vm", "target_id": taskID}
	existing, err := authority.matchingOperation(request.Context(), payload.OperationID, actionRequest)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	if existing == nil {
		if _, err := authority.validateVMTarget(request.Context(), taskID, payload.Action, payload.TargetVersion, payload.ExpectedGeneration, parameters); err != nil {
			writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
			return
		}
		if err := consumeDurableConfirmation(request.Context(), authority.Store, authority.Clock(), payload.ConfirmationToken, actor, payload.Action, "vm:"+taskID, payload.TargetVersion, parameters); err != nil {
			writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
			return
		}
	} else if existing.Status != "queued" {
		writeJSON(writer, http.StatusAccepted, operationValue(*existing))
		return
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	operation, err := authority.Management.ExecuteVMAction(operationCtx, management.VMActionRequest{OperationID: payload.OperationID, Actor: actor, Reason: payload.Reason, TaskID: taskID, TargetVersion: payload.TargetVersion, Action: payload.Action, ExpectedGeneration: generation, Parameters: parameters})
	if err != nil {
		writeProblem(writer, problemFromError(err, "native_action", payload.OperationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*operation))
}

func (authority *Authority) validateVMTarget(ctx context.Context, taskID, action, targetVersion, generation string, parameters map[string]any) (*state.Task, error) {
	if !protocol.TaskIDPattern.MatchString(taskID) {
		return nil, errors.New("invalid_request: task_id is invalid")
	}
	if targetVersion == "" || len(targetVersion) > 256 {
		return nil, errors.New("invalid_request: target_version is required")
	}
	expectedGeneration, err := parseDecimalInt(generation)
	if err != nil {
		return nil, errors.New("invalid_request: expected_generation must be a non-negative decimal string")
	}
	task, err := authority.Store.GetTask(ctx, map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, errors.New("not_found: VM does not exist")
	}
	if task.Generation != expectedGeneration {
		return nil, errors.New("stale_target: VM generation changed")
	}
	if targetVersion != "vm:"+task.TaskID+":"+task.UpdatedAt {
		return nil, errors.New("stale_target: VM resource version changed")
	}
	if action == "" || !containsString([]string{"restart", "resize", "release", "destroy", "port_add", "port_remove"}, action) {
		return nil, errors.New("unsupported_action: VM action is not allowed")
	}
	if action != "release" && action != "destroy" {
		if task.State != "active" {
			return nil, errors.New("stale_target: VM is not active")
		}
		if task.LeaseExpiresAt == nil {
			return nil, errors.New("lease_expired: VM lease is not active")
		}
		lease, parseErr := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
		if parseErr != nil || !lease.After(authority.Clock().UTC()) {
			return nil, errors.New("lease_expired: VM lease has expired")
		}
	}
	if guard, ok := authority.Management.Options.Actions.(management.VMActionGuard); ok {
		if err := guard.ValidateVMAction(ctx, *task, action, parameters); err != nil {
			return nil, err
		}
	}
	return task, nil
}

type imageImportPayload struct {
	OperationID   string          `json:"operation_id"`
	Reason        string          `json:"reason"`
	TargetVersion string          `json:"target_version"`
	VersionLabel  string          `json:"version_label"`
	Source        json.RawMessage `json:"source"`
}

type importSourcePayload struct {
	Kind                string          `json:"kind"`
	Format              string          `json:"format"`
	SourceID            string          `json:"source_id"`
	ExpectedFingerprint string          `json:"expected_fingerprint"`
	ExpectedSHA256      string          `json:"expected_sha256"`
	ExpectedSizeBytes   string          `json:"expected_size_bytes"`
	Metadata            json.RawMessage `json:"metadata"`
	Rootfs              json.RawMessage `json:"rootfs"`
}

type importPartPayload struct {
	ExpectedSHA256    string `json:"expected_sha256"`
	ExpectedSizeBytes string `json:"expected_size_bytes"`
}

func (authority *Authority) startImageImport(writer http.ResponseWriter, request *http.Request, actor string) {
	var payload imageImportPayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	source, err := decodeImportSource(payload.Source)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if source.Kind == "local_upload" && !authority.Config.Upload.Enabled {
		writeProblem(writer, newAPIError("permission_denied", "local image uploads are disabled", http.StatusForbidden, "authorization", false, payload.OperationID))
		return
	}
	if payload.VersionLabel == "" {
		payload.VersionLabel = "unlabeled"
	}
	if err := validateVersionLabel(payload.VersionLabel); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	operation, err := authority.Management.StartImageImport(request.Context(), management.ImportRequest{OperationID: payload.OperationID, Actor: actor, Reason: payload.Reason, TargetVersion: payload.TargetVersion, VersionLabel: payload.VersionLabel, Source: source})
	if err != nil {
		writeProblem(writer, problemFromError(err, "request_validation", payload.OperationID))
		return
	}
	if source.Kind == "trusted_remote" && operation.Status == "queued" {
		operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		operation, err = authority.Management.ProcessTrustedImport(operationCtx, operation.OperationID)
		if err != nil {
			writeProblem(writer, problemFromError(err, "image_import", payload.OperationID))
			return
		}
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*operation))
}

func (authority *Authority) uploadImageImport(writer http.ResponseWriter, request *http.Request, operationID, actor string) {
	resourceUnlock, err := authority.Management.AcquireResourceLock(request.Context(), "image-staging")
	if err != nil {
		writeProblem(writer, problemFromError(err, "upload", operationID))
		return
	}
	defer resourceUnlock()
	authority.uploadMu.Lock()
	defer authority.uploadMu.Unlock()
	if !authority.Config.Upload.Enabled {
		writeProblem(writer, newAPIError("permission_denied", "local image uploads are disabled", http.StatusForbidden, "authorization", false, operationID))
		return
	}
	if err := ensureOperationID(operationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	operation, err := authority.Management.GetOperation(request.Context(), operationID)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", operationID))
		return
	}
	if operation == nil {
		writeProblem(writer, newAPIError("not_found", "the requested operation was not found", http.StatusNotFound, "request_validation", false, operationID))
		return
	}
	if operation.Actor != actor {
		writeProblem(writer, newAPIError("permission_denied", "the operation belongs to a different administrator", http.StatusForbidden, "authorization", false, operationID))
		return
	}
	if operation.Status != "awaiting_upload" {
		if operation.Status == "running" || operation.Status == "succeeded" || operation.Status == "failed" || operation.Status == "outcome_unknown" {
			writeJSON(writer, http.StatusAccepted, operationValue(*operation))
			return
		}
		writeProblem(writer, problemFromError(errors.New("operation_conflict: image upload is not awaiting content"), "upload", operationID))
		return
	}
	source, err := importSourceFromOperation(operation)
	if err != nil {
		writeProblem(writer, problemFromError(err, "invalid_state", operationID))
		return
	}
	if source.Kind != "local_upload" {
		writeProblem(writer, invalidRequest("trusted imports do not accept upload content"))
		return
	}
	declaredSize := source.ExpectedSize
	if source.Format == "split" {
		if source.Metadata.ExpectedSize > math.MaxInt64-source.Rootfs.ExpectedSize {
			writeProblem(writer, invalidRequest("declared image size overflows"))
			return
		}
		declaredSize = source.Metadata.ExpectedSize + source.Rootfs.ExpectedSize
	}
	if err := authority.Management.CheckStagingCapacity(declaredSize); err != nil {
		writeProblem(writer, problemFromError(err, "upload", operationID))
		return
	}
	staged, err := authority.stageUpload(request, source)
	if err != nil {
		// A capacity probe can fail after the stream has begun. Keep the
		// operation awaiting content so the administrator can retry once the
		// staging reserve recovers; malformed or interrupted streams remain
		// durably failed as before.
		if !strings.HasPrefix(err.Error(), "staging_capacity_low:") {
			_, _ = authority.Management.FailImageImportUpload(context.Background(), operationID, err)
		}
		writeProblem(writer, problemFromError(err, "upload", operationID))
		return
	}
	defer staged.cleanup()
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := authority.Management.UploadImageImportFiles(operationCtx, operationID, staged.metadataPath, staged.rootfsPath)
	if err != nil {
		writeProblem(writer, problemFromError(err, "image_import", operationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*result))
}

type stagedUpload struct {
	metadataPath string
	rootfsPath   string
	cleanup      func()
}

func (authority *Authority) stageUpload(request *http.Request, source management.ImportSource) (*stagedUpload, error) {
	if request.Body == nil {
		return nil, errors.New("invalid_request: upload body is required")
	}
	if source.Format == "unified" {
		mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/octet-stream" {
			return nil, errors.New("invalid_request: unified uploads require application/octet-stream")
		}
		path, err := stageReaderFileWithCapacity(authority.Config.Upload.StagingPath, request.Body, source.ExpectedSize, authority.Config.Upload.MaxBytes, func(remaining int64) error { return authority.Management.CheckStagingCapacity(remaining) })
		if err != nil {
			return nil, err
		}
		return &stagedUpload{metadataPath: path, cleanup: func() { _ = os.Remove(path) }}, nil
	}
	if source.Metadata.ExpectedSize > math.MaxInt64-source.Rootfs.ExpectedSize || authority.Config.Upload.MaxBytes > 0 && source.Metadata.ExpectedSize+source.Rootfs.ExpectedSize > authority.Config.Upload.MaxBytes {
		return nil, errors.New("upload_too_large: declared image exceeds the configured limit")
	}
	declaredSize := source.Metadata.ExpectedSize + source.Rootfs.ExpectedSize
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("invalid_request: split uploads require multipart/form-data")
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	paths := make([]string, 0, 2)
	pathByName := map[string]string{}
	cleanup := func() {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
	seen := map[string]bool{}
	consumed := int64(0)
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			cleanup()
			return nil, errors.New("invalid_request: multipart upload is malformed")
		}
		name := part.FormName()
		if name != "metadata" && name != "rootfs" || seen[name] {
			part.Close()
			cleanup()
			return nil, errors.New("invalid_request: multipart upload must contain exactly metadata and rootfs")
		}
		seen[name] = true
		expected := source.Metadata.ExpectedSize
		if name == "rootfs" {
			expected = source.Rootfs.ExpectedSize
		}
		path, readErr := stageReaderFileWithCapacity(authority.Config.Upload.StagingPath, part, expected, authority.Config.Upload.MaxBytes, func(remaining int64) error {
			written := expected - remaining
			if written < 0 {
				written = 0
			}
			totalRemaining := declaredSize - consumed - written
			if totalRemaining <= 0 {
				return nil
			}
			return authority.Management.CheckStagingCapacity(totalRemaining)
		})
		part.Close()
		if readErr != nil {
			cleanup()
			return nil, readErr
		}
		paths = append(paths, path)
		pathByName[name] = path
		consumed += expected
	}
	if !seen["metadata"] || !seen["rootfs"] {
		cleanup()
		return nil, errors.New("invalid_request: multipart upload must contain exactly metadata and rootfs")
	}
	metadataPath, rootfsPath := pathByName["metadata"], pathByName["rootfs"]
	return &stagedUpload{metadataPath: metadataPath, rootfsPath: rootfsPath, cleanup: cleanup}, nil
}

func stageReader(stagingPath string, reader io.Reader, expected, maximum int64) ([]byte, error) {
	return stageReaderWithCapacity(stagingPath, reader, expected, maximum, nil)
}

func stageReaderWithCapacity(stagingPath string, reader io.Reader, expected, maximum int64, capacity func(int64) error) ([]byte, error) {
	path, err := stageReaderFileWithCapacity(stagingPath, reader, expected, maximum, capacity)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("authority_unavailable: upload staging could not be read")
	}
	return value, nil
}

func stageReaderFileWithCapacity(stagingPath string, reader io.Reader, expected, maximum int64, capacity func(int64) error) (name string, err error) {
	if expected < 0 || maximum > 0 && expected > maximum {
		return "", errors.New("upload_too_large: declared image exceeds the configured limit")
	}
	if expected == math.MaxInt64 {
		return "", errors.New("upload_too_large: declared image exceeds the configured limit")
	}
	if stagingPath == "" || !filepath.IsAbs(stagingPath) {
		return "", errors.New("authority_unavailable: upload staging is not configured")
	}
	info, err := os.Lstat(stagingPath)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(stagingPath, 0o700); err != nil {
			return "", errors.New("authority_unavailable: upload staging is unavailable")
		}
		info, err = os.Lstat(stagingPath)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return "", errors.New("authority_unavailable: upload staging is unavailable")
	}
	file, err := os.CreateTemp(stagingPath, ".agent-vm-upload-*")
	if err != nil {
		return "", errors.New("authority_unavailable: upload staging is unavailable")
	}
	name = file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(name)
		}
	}()
	_ = file.Chmod(0o600)
	if capacity != nil {
		if err := capacity(expected); err != nil {
			_ = file.Close()
			return "", err
		}
	}
	count, copyErr := io.Copy(file, io.LimitReader(&capacityReader{reader: reader, expected: expected, check: capacity}, expected+1))
	var syncErr, closeErr error
	if copyErr == nil {
		syncErr = file.Sync()
	}
	closeErr = file.Close()
	if syncErr != nil {
		return "", errors.New("authority_unavailable: upload staging could not be finalized")
	}
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		if strings.HasPrefix(copyErr.Error(), "staging_capacity_low:") {
			return "", copyErr
		}
		return "", errors.New("invalid_request: upload stream could not be read")
	}
	if count != expected {
		if count > expected {
			return "", errors.New("upload_too_large: uploaded bytes exceed the declared size")
		}
		return "", errors.New("invalid_request: uploaded bytes do not match the declared size")
	}
	// Ownership transfers to the caller after a complete write.
	cleanup = false
	return name, nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(os.Getuid())
}

// CleanupUploadStaging removes only stale files created by the upload path.
// It is intentionally strict about ownership and file type so startup recovery
// cannot delete an attacker-controlled path.
func CleanupUploadStaging(stagingPath string) error {
	if stagingPath == "" {
		return nil
	}
	if !filepath.IsAbs(stagingPath) {
		return errors.New("upload staging path is not absolute")
	}
	info, err := os.Lstat(stagingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return errors.New("upload staging path is not a private directory owned by the service")
	}
	entries, err := os.ReadDir(stagingPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".agent-vm-upload-") {
			continue
		}
		path := filepath.Join(stagingPath, entry.Name())
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() || entryInfo.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(entryInfo) {
			return errors.New("upload staging contains an unsafe stale entry")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

type capacityReader struct {
	reader   io.Reader
	expected int64
	read     int64
	check    func(int64) error
}

func (reader *capacityReader) Read(buffer []byte) (int, error) {
	if reader.check != nil {
		remaining := reader.expected - reader.read
		if remaining > 0 {
			if err := reader.check(remaining); err != nil {
				return 0, err
			}
		}
	}
	count, err := reader.reader.Read(buffer)
	reader.read += int64(count)
	return count, err
}

func (authority *Authority) previewDefaultImage(writer http.ResponseWriter, request *http.Request, actor string) {
	var payload struct {
		Fingerprint   string `json:"fingerprint"`
		TargetVersion string `json:"target_version"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateFingerprint(payload.Fingerprint); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	image, err := authority.Management.GetImage(request.Context(), payload.Fingerprint)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", ""))
		return
	}
	if image == nil {
		writeProblem(writer, newAPIError("not_found", "the requested image was not found", http.StatusNotFound, "precondition", false, ""))
		return
	}
	selectionVersionValue, err := authority.Store.ImageSelectionVersion(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	if payload.TargetVersion != selectionVersionValue {
		writeProblem(writer, problemFromError(fmt.Errorf("stale_target: expected %s, current %s", payload.TargetVersion, selectionVersionValue), "precondition", ""))
		return
	}
	if image.State != "available" || image.Integrity != "passed" || image.Suitability != "passed" {
		writeProblem(writer, errorsAPI("image_not_applicable", "image is not fully verified", http.StatusConflict, "suitability_check", ""))
		return
	}
	params := map[string]any{"fingerprint": payload.Fingerprint}
	token, expiresAt, err := issueDurableConfirmation(request.Context(), authority.Store, authority.Clock(), actor, "default_image_select", "image:"+payload.Fingerprint, payload.TargetVersion, params)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, actionPreviewValue(payload.TargetVersion, token, expiresAt, imageImpact(*image, "default_image_select")))
}

func (authority *Authority) submitDefaultImage(writer http.ResponseWriter, request *http.Request, actor string) {
	var payload struct {
		OperationID       string `json:"operation_id"`
		Reason            string `json:"reason"`
		TargetVersion     string `json:"target_version"`
		Fingerprint       string `json:"fingerprint"`
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateFingerprint(payload.Fingerprint); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateConfirmationToken(payload.ConfirmationToken); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	params := map[string]any{"fingerprint": payload.Fingerprint}
	requestValue := map[string]any{"action": "default_image_select", "target_id": payload.Fingerprint, "target_version": payload.TargetVersion, "actor": actor, "reason": payload.Reason}
	existing, err := authority.matchingOperation(request.Context(), payload.OperationID, requestValue)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	if existing != nil && existing.Status != "queued" {
		writeJSON(writer, http.StatusAccepted, operationValue(*existing))
		return
	}
	if existing == nil {
		if err := consumeDurableConfirmation(request.Context(), authority.Store, authority.Clock(), payload.ConfirmationToken, actor, "default_image_select", "image:"+payload.Fingerprint, payload.TargetVersion, params); err != nil {
			writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
			return
		}
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	operation, err := authority.Management.SelectDefault(operationCtx, payload.OperationID, actor, payload.Reason, payload.Fingerprint, payload.TargetVersion)
	if err != nil {
		writeProblem(writer, problemFromError(err, "native_mutation", payload.OperationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*operation))
}

func (authority *Authority) previewImageMutation(writer http.ResponseWriter, request *http.Request, fingerprint, action, actor string) {
	if err := validateFingerprint(fingerprint); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	image, err := authority.Management.GetImage(request.Context(), fingerprint)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", ""))
		return
	}
	if image == nil {
		writeProblem(writer, newAPIError("not_found", "the requested image was not found", http.StatusNotFound, "precondition", false, ""))
		return
	}
	var payload struct {
		TargetVersion string `json:"target_version"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	current := "image:" + fingerprint + ":revision-" + strconv.FormatInt(image.Revision, 10)
	if payload.TargetVersion != current {
		writeProblem(writer, problemFromError(fmt.Errorf("stale_target: expected %s, current %s", payload.TargetVersion, current), "precondition", ""))
		return
	}
	params := map[string]any{"fingerprint": fingerprint}
	token, expiresAt, err := issueDurableConfirmation(request.Context(), authority.Store, authority.Clock(), actor, action, "image:"+fingerprint, payload.TargetVersion, params)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, actionPreviewValue(payload.TargetVersion, token, expiresAt, imageImpact(*image, action)))
}

func (authority *Authority) submitImageMutation(writer http.ResponseWriter, request *http.Request, fingerprint, action, actor string) {
	if err := validateFingerprint(fingerprint); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	var payload struct {
		OperationID       string `json:"operation_id"`
		Reason            string `json:"reason"`
		TargetVersion     string `json:"target_version"`
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateConfirmationToken(payload.ConfirmationToken); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	params := map[string]any{"fingerprint": fingerprint}
	requestValue := map[string]any{"action": action, "target_id": fingerprint, "target_version": payload.TargetVersion, "actor": actor, "reason": payload.Reason}
	existing, err := authority.matchingOperation(request.Context(), payload.OperationID, requestValue)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	if existing != nil && existing.Status != "queued" {
		writeJSON(writer, http.StatusAccepted, operationValue(*existing))
		return
	}
	if existing == nil {
		if err := consumeDurableConfirmation(request.Context(), authority.Store, authority.Clock(), payload.ConfirmationToken, actor, action, "image:"+fingerprint, payload.TargetVersion, params); err != nil {
			writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
			return
		}
	}
	var operation *state.ManagementOperation
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if action == "image_retire" {
		operation, err = authority.Management.RetireImage(operationCtx, payload.OperationID, actor, payload.Reason, fingerprint, payload.TargetVersion)
	} else {
		operation, err = authority.Management.DeleteImage(operationCtx, payload.OperationID, actor, payload.Reason, fingerprint, payload.TargetVersion)
	}
	if err != nil {
		writeProblem(writer, problemFromError(err, "native_mutation", payload.OperationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*operation))
}

func (authority *Authority) previewMaintenance(writer http.ResponseWriter, request *http.Request, actor string) {
	var payload maintenancePayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	parameters, err := normalizeMaintenanceParameters(payload.Action, payload.Parameters)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	current, err := authority.currentSystemVersion(request.Context())
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", ""))
		return
	}
	if payload.TargetVersion != current {
		writeProblem(writer, problemFromError(fmt.Errorf("stale_target: expected %s, current %s", payload.TargetVersion, current), "precondition", ""))
		return
	}
	token, expiresAt, err := issueDurableConfirmation(request.Context(), authority.Store, authority.Clock(), actor, payload.Action, "system", payload.TargetVersion, parameters)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, actionPreviewValue(payload.TargetVersion, token, expiresAt, map[string]any{"summary_code": payload.Action, "affected_session_count": 0, "affected_image_references": emptyReferences(), "changes": []any{map[string]any{"code": payload.Action, "count": 0, "value": nil}}}))
}

type maintenancePayload struct {
	Action            string         `json:"action"`
	TargetVersion     string         `json:"target_version"`
	Parameters        map[string]any `json:"parameters"`
	OperationID       string         `json:"operation_id"`
	Reason            string         `json:"reason"`
	ConfirmationToken string         `json:"confirmation_token"`
}

func (authority *Authority) submitMaintenance(writer http.ResponseWriter, request *http.Request, actor string) {
	var payload maintenancePayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateTargetVersion(payload.TargetVersion); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateConfirmationToken(payload.ConfirmationToken); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	parameters, err := normalizeMaintenanceParameters(payload.Action, payload.Parameters)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	requestValue := map[string]any{"action": payload.Action, "target_version": payload.TargetVersion, "parameters": parameters, "actor": actor, "reason": payload.Reason, "target_kind": "system", "target_id": "system"}
	existing, err := authority.matchingOperation(request.Context(), payload.OperationID, requestValue)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	if existing != nil && existing.Status != "queued" {
		writeJSON(writer, http.StatusAccepted, operationValue(*existing))
		return
	}
	if existing == nil {
		if err := consumeDurableConfirmation(request.Context(), authority.Store, authority.Clock(), payload.ConfirmationToken, actor, payload.Action, "system", payload.TargetVersion, parameters); err != nil {
			writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
			return
		}
	}
	operationCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	operation, err := authority.Management.ExecuteMaintenance(operationCtx, management.MaintenanceRequest{OperationID: payload.OperationID, Actor: actor, Reason: payload.Reason, TargetVersion: payload.TargetVersion, Action: payload.Action, Parameters: parameters})
	if err != nil {
		writeProblem(writer, problemFromError(err, "native_action", payload.OperationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*operation))
}

func (authority *Authority) currentSystemVersion(ctx context.Context) (string, error) {
	epoch, err := authority.Store.StateEpoch(ctx)
	if err != nil {
		return "", err
	}
	return "system:" + epoch, nil
}

func (authority *Authority) matchingOperation(ctx context.Context, operationID string, request map[string]any) (*state.ManagementOperation, error) {
	if operationID == "" {
		return nil, nil
	}
	operation, err := authority.Management.GetOperation(ctx, operationID)
	if err != nil || operation == nil {
		return operation, err
	}
	digest, err := protocol.Digest(protocol.Request(request))
	if err != nil {
		return nil, errors.New("invalid_request: operation request cannot be canonicalized")
	}
	if stringValue(operation.Request["_request_digest"]) != digest {
		return nil, errors.New("operation_conflict: operation ID belongs to a different request")
	}
	return operation, nil
}

func decodeJSON(request *http.Request, target any) error {
	if request.Body == nil {
		return errors.New("request body is required")
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxManagementJSONBytes+1))
	if len(raw) > maxManagementJSONBytes {
		return errors.New("request body exceeds the limit")
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func decodeImportSource(raw json.RawMessage) (management.ImportSource, error) {
	if len(raw) == 0 {
		return management.ImportSource{}, errors.New("source is required")
	}
	var value importSourcePayload
	if err := decodeRaw(raw, &value); err != nil {
		return management.ImportSource{}, err
	}
	result := management.ImportSource{Kind: value.Kind, Format: value.Format, SourceID: value.SourceID, ExpectedFingerprint: value.ExpectedFingerprint, ExpectedSHA256: value.ExpectedSHA256}
	switch value.Kind {
	case "trusted_remote":
		if value.Format != "" || value.SourceID == "" || len(value.SourceID) > 128 || !managementFingerprintPattern.MatchString(value.ExpectedFingerprint) || value.ExpectedSHA256 != "" || value.ExpectedSizeBytes != "" || len(value.Metadata) > 0 || len(value.Rootfs) > 0 {
			return management.ImportSource{}, errors.New("trusted_remote source contains unsupported or invalid fields")
		}
		return result, nil
	case "local_upload":
		if value.SourceID != "" || value.ExpectedFingerprint != "" {
			return management.ImportSource{}, errors.New("local_upload source contains unsupported fields")
		}
	case "":
		return management.ImportSource{}, errors.New("source.kind is required")
	default:
		return management.ImportSource{}, errors.New("source.kind is unsupported")
	}
	switch value.Format {
	case "unified":
		if value.ExpectedSizeBytes == "" || value.ExpectedSHA256 == "" || len(value.Metadata) > 0 || len(value.Rootfs) > 0 {
			return management.ImportSource{}, errors.New("unified source requires exactly expected_sha256 and expected_size_bytes")
		}
		size, err := parseDecimalInt(value.ExpectedSizeBytes)
		if err != nil {
			return management.ImportSource{}, errors.New("expected_size_bytes must be a canonical non-negative decimal string")
		}
		result.ExpectedSize = size
	case "split":
		if value.ExpectedSHA256 != "" || value.ExpectedSizeBytes != "" || len(value.Metadata) == 0 || len(value.Rootfs) == 0 {
			return management.ImportSource{}, errors.New("split source requires exactly metadata and rootfs")
		}
		metadata, metadataErr := decodeImportPart(value.Metadata, "metadata")
		if metadataErr != nil {
			return management.ImportSource{}, metadataErr
		}
		rootfs, rootfsErr := decodeImportPart(value.Rootfs, "rootfs")
		if rootfsErr != nil {
			return management.ImportSource{}, rootfsErr
		}
		result.Metadata, result.Rootfs = metadata, rootfs
	default:
		return management.ImportSource{}, errors.New("source.format is unsupported")
	}
	return result, nil
}

func decodeImportPart(raw json.RawMessage, name string) (management.ImportPart, error) {
	var value importPartPayload
	if err := decodeRaw(raw, &value); err != nil {
		return management.ImportPart{}, err
	}
	if value.ExpectedSizeBytes == "" || !managementFingerprintPattern.MatchString(value.ExpectedSHA256) {
		return management.ImportPart{}, fmt.Errorf("%s requires a lowercase SHA-256 and expected_size_bytes", name)
	}
	size, err := parseDecimalInt(value.ExpectedSizeBytes)
	if err != nil {
		return management.ImportPart{}, fmt.Errorf("%s.expected_size_bytes must be a canonical non-negative decimal string", name)
	}
	return management.ImportPart{ExpectedSHA256: value.ExpectedSHA256, ExpectedSize: size}, nil
}

func importSourceFromOperation(operation *state.ManagementOperation) (management.ImportSource, error) {
	requestValue, ok := operation.Request["request"].(map[string]any)
	if !ok {
		return management.ImportSource{}, errors.New("invalid_state: import request is missing")
	}
	sourceValue, ok := requestValue["source"].(map[string]any)
	if !ok {
		return management.ImportSource{}, errors.New("invalid_state: import source is missing")
	}
	result := management.ImportSource{Kind: stringValue(sourceValue["kind"]), Format: stringValue(sourceValue["format"]), SourceID: stringValue(sourceValue["source_id"]), ExpectedFingerprint: stringValue(sourceValue["expected_fingerprint"]), ExpectedSHA256: stringValue(sourceValue["expected_sha256"]), ExpectedSize: int64Value(sourceValue["expected_size_bytes"])}
	if value, ok := sourceValue["metadata"].(map[string]any); ok {
		result.Metadata = management.ImportPart{ExpectedSHA256: stringValue(value["expected_sha256"]), ExpectedSize: int64Value(value["expected_size_bytes"])}
	}
	if value, ok := sourceValue["rootfs"].(map[string]any); ok {
		result.Rootfs = management.ImportPart{ExpectedSHA256: stringValue(value["expected_sha256"]), ExpectedSize: int64Value(value["expected_size_bytes"])}
	}
	return result, nil
}

func decodeRaw(raw []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request contains trailing JSON")
	}
	return nil
}

func normalizeVMParameters(action string, parameters map[string]any) (map[string]any, error) {
	if parameters == nil {
		parameters = map[string]any{}
	}
	allowed := map[string]struct{}{}
	switch action {
	case "restart", "release", "destroy":
	case "resize":
		allowed = map[string]struct{}{"cpu_vcpu": {}, "memory_bytes": {}, "disk_bytes": {}, "live": {}, "allow_restart": {}}
	case "port_add":
		allowed = map[string]struct{}{"port_name": {}, "protocol": {}, "host_port": {}, "guest_port": {}}
	case "port_remove":
		allowed = map[string]struct{}{"port_name": {}, "protocol": {}}
	default:
		return nil, errors.New("unsupported action")
	}
	for key := range parameters {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("parameters contains unsupported field %q", key)
		}
	}
	result := map[string]any{}
	switch action {
	case "resize":
		if value, ok := parameters["cpu_vcpu"]; ok {
			cpu, err := decimalNumber(value)
			if err != nil || cpu <= 0 {
				return nil, errors.New("cpu_vcpu must be a positive integer")
			}
			result["cpu"] = cpu
		}
		if value, ok := parameters["memory_bytes"]; ok {
			bytes, err := decimalString(value)
			if err != nil || bytes == 0 || bytes%bytesPerMiB != 0 {
				return nil, errors.New("memory_bytes must be a positive whole MiB count")
			}
			result["memory_mib"] = bytes / bytesPerMiB
		}
		if value, ok := parameters["disk_bytes"]; ok {
			bytes, err := decimalString(value)
			if err != nil || bytes == 0 || bytes%bytesPerGiB != 0 {
				return nil, errors.New("disk_bytes must be a positive whole GiB count")
			}
			result["disk_gib"] = bytes / bytesPerGiB
		}
		if value, ok := parameters["live"]; ok {
			live, ok := value.(bool)
			if !ok {
				return nil, errors.New("live must be boolean")
			}
			result["live_memory"] = live
		}
		if value, ok := parameters["allow_restart"]; ok {
			allow, ok := value.(bool)
			if !ok {
				return nil, errors.New("allow_restart must be boolean")
			}
			result["allow_restart"] = allow
		}
	case "port_add":
		name, _ := parameters["port_name"].(string)
		protocolName, _ := parameters["protocol"].(string)
		if protocolName == "" {
			protocolName = "tcp"
		}
		if !protocol.PortNamePattern.MatchString(name) || name == "ssh" || (protocolName != "tcp" && protocolName != "udp") {
			return nil, errors.New("port_name and protocol are invalid")
		}
		guest, err := decimalNumber(parameters["guest_port"])
		if err != nil || guest < 1 || guest > 65535 {
			return nil, errors.New("guest_port must be between 1 and 65535")
		}
		result["name"], result["port_protocol"], result["vm_port"] = name, protocolName, guest
		if value, exists := parameters["host_port"]; exists && value != nil {
			host, hostErr := decimalNumber(value)
			if hostErr != nil || host < 1024 || host > 65535 {
				return nil, errors.New("host_port must be between 1024 and 65535")
			}
			result["host_port"] = host
		}
	case "port_remove":
		name, _ := parameters["port_name"].(string)
		protocolName, _ := parameters["protocol"].(string)
		if protocolName == "" {
			protocolName = "tcp"
		}
		if !protocol.PortNamePattern.MatchString(name) || name == "ssh" || (protocolName != "tcp" && protocolName != "udp") {
			return nil, errors.New("port_name and protocol are invalid")
		}
		result["name"], result["port_protocol"] = name, protocolName
	}
	return result, nil
}

func normalizeMaintenanceParameters(action string, parameters map[string]any) (map[string]any, error) {
	if parameters == nil {
		parameters = map[string]any{}
	}
	if action != "drain_enter" && action != "reconcile" && action != "gc" {
		return nil, errors.New("maintenance action is not supported")
	}
	for key := range parameters {
		if key != "apply" && key != "selection" {
			return nil, fmt.Errorf("parameters contains unsupported field %q", key)
		}
	}
	result := map[string]any{"apply": true}
	if value, ok := parameters["selection"]; ok {
		selection, ok := value.(string)
		if !ok || !containsString([]string{"expired", "released", "eligible_cache"}, selection) {
			return nil, errors.New("selection is invalid")
		}
		result["selection"] = selection
	}
	if value, ok := parameters["apply"]; ok && value != true {
		return nil, errors.New("apply must be true")
	}
	return result, nil
}

const (
	bytesPerMiB = int64(1024 * 1024)
	bytesPerGiB = int64(1024 * 1024 * 1024)
)

func decimalString(value any) (int64, error) {
	text, ok := value.(string)
	if !ok || text == "" || strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		return 0, errors.New("not a decimal string")
	}
	return parseDecimalInt(text)
}

func decimalNumber(value any) (int64, error) {
	switch parsed := value.(type) {
	case json.Number:
		return parseDecimalInt(parsed.String())
	case int:
		return int64(parsed), nil
	case int64:
		return parsed, nil
	case float64:
		if parsed != math.Trunc(parsed) || parsed > math.MaxInt64 || parsed < 0 {
			return 0, errors.New("not an integer")
		}
		return int64(parsed), nil
	default:
		return 0, errors.New("not a number")
	}
}

func parseDecimalInt(value string) (int64, error) {
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || len(value) > 1 && value[0] == '0' {
		return 0, errors.New("not a non-negative decimal")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("not a non-negative decimal")
	}
	return parsed, nil
}

func validateFingerprint(value string) error {
	if !managementFingerprintPattern.MatchString(value) {
		return errors.New("fingerprint must be lowercase SHA-256")
	}
	return nil
}

func validateReason(value string) error {
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("reason must not be blank")
	}
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 512 {
		return errors.New("reason must not exceed 512 Unicode characters")
	}
	return nil
}

func validateTargetVersion(value string) error {
	if value == "" {
		return errors.New("target_version is required")
	}
	if len(value) > 256 {
		return errors.New("target_version must not exceed 256 characters")
	}
	return nil
}

func validateVersionLabel(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("version_label must be between 1 and 128 characters")
	}
	return nil
}

func validateConfirmationToken(value string) error {
	if len(value) < 32 {
		return errors.New("confirmation_token is invalid")
	}
	return nil
}

type pageAfter func(key, cursor string) bool

func pageRequest(query url.Values) (int, string, error) {
	limit := 25
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return 0, "", errors.New("limit must be between 1 and 100")
		}
		limit = parsed
	}
	value := query.Get("cursor")
	if value == "" {
		return limit, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, "", errors.New("cursor is invalid")
	}
	var cursor struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Key == "" {
		return 0, "", errors.New("cursor is invalid")
	}
	return limit, cursor.Key, nil
}

func pageBounds(query url.Values, keys []string, after pageAfter) (int, int, error) {
	limit, cursorKey, err := pageRequest(query)
	if err != nil {
		return 0, 0, err
	}
	start := 0
	if cursorKey != "" {
		for index, key := range keys {
			if after(key, cursorKey) {
				start = index
				break
			}
		}
		if start == 0 && len(keys) > 0 && !after(keys[0], cursorKey) {
			start = len(keys)
		}
	}
	return start, limit, nil
}

func pageAfterAscending(key, cursor string) bool {
	return key > cursor
}

func pageAfterImage(key, cursor string) bool {
	keyCreated, keyFingerprint := pageKeyParts(key)
	cursorCreated, cursorFingerprint := pageKeyParts(cursor)
	if keyCreated != cursorCreated {
		return keyCreated < cursorCreated
	}
	return keyFingerprint > cursorFingerprint
}

func pageAfterDescendingPair(key, cursor string) bool {
	keyCreated, keyID := pageKeyParts(key)
	cursorCreated, cursorID := pageKeyParts(cursor)
	if keyCreated != cursorCreated {
		return keyCreated < cursorCreated
	}
	return keyID < cursorID
}

func pageKeyParts(value string) (string, string) {
	created, id, found := strings.Cut(value, "\x00")
	if !found {
		return value, ""
	}
	return created, id
}

func validateVMQuery(query url.Values) error {
	if err := rejectUnknownQueryKeys(query, map[string]bool{"q": true, "session_id": true, "image_fingerprint": true, "state": true, "lease_state": true, "limit": true, "cursor": true}); err != nil {
		return err
	}
	if len(query.Get("q")) > 200 {
		return errors.New("q must not exceed 200 characters")
	}
	if value := query.Get("session_id"); value != "" && len(value) > 256 {
		return errors.New("session_id is too long")
	}
	if value := query.Get("image_fingerprint"); value != "" && !managementFingerprintPattern.MatchString(value) {
		return errors.New("image_fingerprint must be lowercase SHA-256")
	}
	for _, value := range query["state"] {
		if !containsString([]string{"preparing", "active", "cached", "closing", "deleting", "failed", "absent"}, value) {
			return errors.New("state is invalid")
		}
	}
	if value := query.Get("lease_state"); value != "" && !containsString([]string{"effective", "expired", "absent", "unknown"}, value) {
		return errors.New("lease_state is invalid")
	}
	return nil
}

func validateImageQuery(query url.Values) error {
	if err := rejectUnknownQueryKeys(query, map[string]bool{"q": true, "state": true, "architecture": true, "limit": true, "cursor": true}); err != nil {
		return err
	}
	if len(query.Get("q")) > 200 {
		return errors.New("q must not exceed 200 characters")
	}
	if value := query.Get("state"); value != "" && !containsString([]string{"available", "verifying", "retired", "failed"}, value) {
		return errors.New("state is invalid")
	}
	if value := query.Get("architecture"); value != "" && len(value) > 128 {
		return errors.New("architecture is too long")
	}
	return nil
}

func validateOperationQuery(query url.Values) error {
	if err := rejectUnknownQueryKeys(query, map[string]bool{"q": true, "status": true, "action": true, "actor": true, "target_id": true, "limit": true, "cursor": true}); err != nil {
		return err
	}
	if len(query.Get("q")) > 200 {
		return errors.New("q must not exceed 200 characters")
	}
	if value := query.Get("status"); value != "" && !containsString([]string{"awaiting_upload", "queued", "running", "reconciling", "succeeded", "failed", "outcome_unknown"}, value) {
		return errors.New("status is invalid")
	}
	if value := query.Get("action"); value != "" && !containsString([]string{"restart", "resize", "release", "destroy", "port_add", "port_remove", "image_import", "default_image_select", "image_retire", "image_delete", "drain_enter", "reconcile", "gc"}, value) {
		return errors.New("action is invalid")
	}
	return nil
}

func rejectUnknownQueryKeys(query url.Values, allowed map[string]bool) error {
	for key := range query {
		if !allowed[key] {
			return errors.New("unknown query parameter: " + key)
		}
	}
	return nil
}

func auditBounds(query url.Values) (*time.Time, *time.Time, error) {
	if err := rejectUnknownQueryKeys(query, map[string]bool{"from": true, "to": true, "actor": true, "action": true, "operation_id": true, "target_id": true, "limit": true, "cursor": true}); err != nil {
		return nil, nil, err
	}
	var from, to *time.Time
	for key, target := range map[string]**time.Time{"from": &from, "to": &to} {
		value := query.Get(key)
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, nil, errors.New(key + " must be an RFC 3339 timestamp")
		}
		*target = &parsed
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, nil, errors.New("from must be earlier than to")
	}
	return from, to, nil
}

func pageValue(limit int, more bool, nextKey string) map[string]any {
	var cursor any
	if more && nextKey != "" {
		value, _ := json.Marshal(map[string]string{"key": nextKey})
		cursor = base64.RawURLEncoding.EncodeToString(value)
	}
	return map[string]any{"limit": limit, "next_cursor": cursor}
}

func pageKey(keys []string, end int) string {
	if end <= 0 || end > len(keys) {
		return ""
	}
	return keys[end-1]
}

func imagePageKey(image state.ManagedImage) string {
	return image.CreatedAt + "\x00" + image.Fingerprint
}

func imageValue(image state.ManagedImage) map[string]any {
	return map[string]any{
		"fingerprint": image.Fingerprint, "resource_version": "image:" + image.Fingerprint + ":revision-" + strconv.FormatInt(image.Revision, 10), "version_label": image.VersionLabel,
		"architecture": image.Architecture, "instance_type": image.InstanceType, "source": map[string]any{"kind": image.SourceKind, "source_id": image.SourceID}, "state": image.State,
		"verification": map[string]any{"integrity": image.Integrity, "suitability": image.Suitability, "findings": safeFindings(image.Findings)}, "is_default": image.IsDefault, "default_compatibility": image.DefaultCompatibility,
		"references": map[string]any{"default": image.References.Default, "active": image.References.Active, "cached": image.References.Cached, "releasing": image.References.Releasing, "pending_creation": image.References.PendingCreation, "verification": image.References.Verification}, "created_at": image.CreatedAt,
	}
}

func safeFindings(values []string) []string {
	allowed := map[string]struct{}{
		"fingerprint_mismatch": {}, "unsupported_architecture": {}, "unsupported_instance_type": {},
		"missing_runtime": {}, "missing_sync_tool": {}, "guest_not_ready": {},
		"insufficient_staging_space": {}, "validation_failed": {},
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; ok {
			result = append(result, value)
		}
	}
	return result
}

func operationValue(operation state.ManagementOperation) map[string]any {
	result := safeOperationResult(operation.Result)
	var resultValue any = result
	if result == nil {
		resultValue = nil
	}
	var errorValue any
	if operation.Error != nil {
		errorValue = safeOperationError(operation.Error, operation.OperationID, operation.Phase)
	}
	var reason any
	if operation.Reason != "" {
		reason = operation.Reason
	}
	return map[string]any{"operation_id": operation.OperationID, "action": operation.Action, "status": operationStatus(operation), "phase": operationPhase(operation), "target": map[string]any{"kind": operation.TargetKind, "id": operation.TargetID}, "actor": actorValue(operation.Actor), "reason": reason, "created_at": operation.CreatedAt, "updated_at": operation.UpdatedAt, "result": resultValue, "error": errorValue}
}

func operationStatus(operation state.ManagementOperation) string {
	if operation.Status == "running" && operation.Phase == "reconciliation" {
		return "reconciling"
	}
	return operation.Status
}

func operationPhase(operation state.ManagementOperation) string {
	phase := operation.Phase
	if phase == "native_mutation" {
		phase = "native_action"
	}
	if phase == "" {
		phase = "reconciliation"
	}
	return phase
}

func safeOperationResult(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := map[string]any{}
	for _, key := range []string{"image_fingerprint", "default_fingerprint", "affected_session_count", "effect", "plaintext_disclosure"} {
		if item, ok := value[key]; ok {
			result[key] = item
		}
	}
	if metadata, ok := value["token_metadata"].(map[string]any); ok {
		result["token_metadata"] = map[string]any{
			"token_id": metadata["token_id"], "resource_version": metadata["resource_version"],
			"scopes": metadata["scopes"], "created_at": metadata["created_at"],
			"expires_at": metadata["expires_at"], "revoked_at": metadata["revoked_at"],
		}
	}
	if value["fingerprint"] != nil && result["image_fingerprint"] == nil {
		result["image_fingerprint"] = value["fingerprint"]
	}
	if result["effect"] == nil {
		if stateValue, ok := value["state"].(string); ok {
			if effect := map[string]string{"available": "verified", "deleted": "deleted", "retired": "retired", "succeeded": "completed"}[stateValue]; effect != "" {
				result["effect"] = effect
			}
		}
	}
	return result
}

func safeOperationError(value map[string]any, operationID, fallbackPhase string) map[string]any {
	code := safeCode(stringValue(value["code"]))
	phase := stringValue(value["phase"])
	if phase == "" {
		phase = fallbackPhase
	}
	phase = normalizePhase(phase)
	retryable, _ := value["retryable"].(bool)
	result := map[string]any{"code": code, "phase": phase, "retryable": retryable, "operation_id": operationID}
	safeFields := map[string]any{}
	if refs, ok := value["references"]; ok {
		safeFields["affected_references"] = refs
	}
	if fingerprint, ok := value["imported_fingerprint"].(string); ok && managementFingerprintPattern.MatchString(fingerprint) {
		safeFields["imported_fingerprint"] = fingerprint
	}
	if fingerprint, ok := value["expected_fingerprint"].(string); ok && managementFingerprintPattern.MatchString(fingerprint) {
		safeFields["expected_fingerprint"] = fingerprint
	}
	if cleanup, ok := value["cleanup"].(string); ok && containsString([]string{"verified", "unverified", "blocked_catalog_unverified", "backend_unavailable"}, cleanup) {
		safeFields["cleanup"] = cleanup
	}
	if len(safeFields) > 0 {
		result["safe_fields"] = safeFields
	}
	return result
}

func auditValue(value state.ManagementAudit) map[string]any {
	var operationID any
	if value.OperationID != nil {
		operationID = *value.OperationID
	}
	phase := value.Phase
	if phase == "native_mutation" {
		phase = "native_action"
	}
	if phase == "" {
		phase = "reconciliation"
	}
	var reason any
	if value.Reason != "" {
		reason = value.Reason
	}
	result := map[string]any{"event_id": value.EventID, "operation_id": operationID, "action": value.Action, "actor": actorValue(value.Actor), "reason": reason, "target": value.Target, "outcome": value.Outcome, "phase": phase, "occurred_at": value.OccurredAt}
	if value.Detail != nil {
		result["safe_code"] = safeCode(stringValue(value.Detail["code"]))
	} else {
		result["safe_code"] = nil
	}
	return result
}

func actorValue(value string) any {
	parts := strings.SplitN(value, "\x00", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return map[string]any{"issuer": parts[0], "subject": parts[1], "role": "administrator"}
	}
	return value
}

func actionPreviewValue(targetVersion, token string, expiresAt time.Time, impact map[string]any) map[string]any {
	return map[string]any{"target_version": targetVersion, "confirmation_token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339Nano), "impact": impact}
}

func vmImpact(task *state.Task, action string) map[string]any {
	count := 0
	if task != nil && task.TaskID != "" {
		count = 1
	}
	return map[string]any{"summary_code": action, "affected_session_count": count, "affected_image_references": emptyReferences(), "changes": []any{map[string]any{"code": action, "count": count, "value": nil}}}
}

func imageImpact(image state.ManagedImage, action string) map[string]any {
	return map[string]any{"summary_code": action, "affected_session_count": image.References.Active, "affected_image_references": map[string]any{"default": image.References.Default, "active": image.References.Active, "cached": image.References.Cached, "releasing": image.References.Releasing, "pending_creation": image.References.PendingCreation, "verification": image.References.Verification}, "changes": []any{map[string]any{"code": action, "count": image.References.Total(), "value": image.Fingerprint}}}
}

func emptyReferences() map[string]any {
	return map[string]any{"default": 0, "active": 0, "cached": 0, "releasing": 0, "pending_creation": 0, "verification": 0}
}

func observedNumber(value any, observedAt string, fresh bool) map[string]any {
	if value == nil {
		return map[string]any{"state": "unverified", "value": nil, "observed_at": nil}
	}
	if !fresh {
		return map[string]any{"state": "stale", "value": nil, "observed_at": observedAt}
	}
	return map[string]any{"state": "available", "value": value, "observed_at": observedAt}
}

func observedBytes(value any, observedAt string, fresh bool) map[string]any {
	if value == nil {
		return map[string]any{"state": "unverified", "value_bytes": nil, "observed_at": nil}
	}
	if !fresh {
		return map[string]any{"state": "stale", "value_bytes": nil, "observed_at": observedAt}
	}
	return map[string]any{"state": "available", "value_bytes": value, "observed_at": observedAt}
}

func observedBoolean(value any, observedAt string, fresh bool) map[string]any {
	if value == nil {
		return map[string]any{"state": "unverified", "value": nil, "observed_at": nil}
	}
	if !fresh {
		return map[string]any{"state": "stale", "value": nil, "observed_at": observedAt}
	}
	return map[string]any{"state": "available", "value": value, "observed_at": observedAt}
}

func decimalBytes(value any, multiplier int64) any {
	parsed := int64Value(value)
	if value == nil || parsed < 0 || parsed > math.MaxInt64/multiplier {
		return nil
	}
	return strconv.FormatInt(parsed*multiplier, 10)
}

func numberOrNil(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func numberValue(value any) any {
	if value == nil {
		return nil
	}
	return value
}

func nestedNumber(root map[string]any, keys ...string) any {
	var current any = root
	for _, key := range keys {
		mapValue, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapValue[key]
	}
	return current
}

func nestedBool(root map[string]any, keys ...string) any {
	return nestedNumber(root, keys...)
}

func metricsMapValue(root map[string]any, first, second string) any {
	value, ok := root[first].(map[string]int64)
	if ok {
		return value[second]
	}
	if value, ok := root[first].(map[string]any); ok {
		return value[second]
	}
	return nil
}

func int64Value(value any) int64 {
	if value == nil {
		return 0
	}
	if parsed, ok := value.(int64); ok {
		return parsed
	}
	if parsed, ok := value.(int); ok {
		return int64(parsed)
	}
	if parsed, ok := value.(json.Number); ok {
		result, _ := parsed.Int64()
		return result
	}
	if parsed, ok := value.(float64); ok && parsed == math.Trunc(parsed) {
		return int64(parsed)
	}
	return 0
}

func stringValue(value any) string {
	parsed, _ := value.(string)
	return parsed
}

func matchesAny(values []string, candidate string) bool {
	if len(values) == 0 {
		return true
	}
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func matchesLease(filter string, task state.Task, now time.Time) bool {
	if filter == "" {
		return true
	}
	stateValue := "unknown"
	if task.LeaseExpiresAt == nil {
		stateValue = "absent"
	} else if expires, err := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt); err == nil {
		stateValue = "expired"
		if expires.After(now.UTC()) {
			stateValue = "effective"
		}
	}
	return stateValue == filter
}

func sourceState(value any) string {
	if up, ok := value.(bool); ok && up {
		return "available"
	}
	return "failed"
}

func sourceStateWithFreshness(value any, fresh bool) string {
	if !fresh {
		return "stale"
	}
	return sourceState(value)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func errorsAPI(code, message string, status int, phase, operationID string) *APIError {
	return newAPIError(code, message, status, phase, false, operationID)
}
