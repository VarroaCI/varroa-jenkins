package api

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ---------------------------------------------------------------------------
// §2.1 Response DTOs
// ---------------------------------------------------------------------------

// UpdateCenterConditionDTO is a BFF response DTO for update center conditions.
type UpdateCenterConditionDTO struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message"`
}

// UpdateCenterGapDTO is a BFF response DTO for update center gaps.
type UpdateCenterGapDTO struct {
	Plugin     string `json:"plugin"`
	Version    string `json:"version"`
	RequiredBy string `json:"requiredBy"`
}

// UpdateCenterStatusResponse is the BFF response DTO for update center status.
type UpdateCenterStatusResponse struct {
	Enabled            bool                       `json:"enabled"`
	Phase              string                     `json:"phase"`
	Conditions         []UpdateCenterConditionDTO `json:"conditions"`
	PluginCount        int                        `json:"pluginCount"`
	StoreBytes         int64                      `json:"storeBytes"`
	Gaps               []UpdateCenterGapDTO       `json:"gaps"`
	LastSyncTime       *time.Time                 `json:"lastSyncTime"` // nil when never synced
	StorageType        string                     `json:"storageType"`
	PullThroughEnabled bool                       `json:"pullThroughEnabled"`
}

// UpdateCenterPluginDTO is a BFF response DTO for a single plugin.
type UpdateCenterPluginDTO struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// UpdateCenterPluginsResponse is the BFF response envelope for plugin inventory.
type UpdateCenterPluginsResponse struct {
	Enabled bool                    `json:"enabled"`
	Plugins []UpdateCenterPluginDTO `json:"plugins"`
}

// ---------------------------------------------------------------------------
// §2.2 Mapping: CRD → DTO
// ---------------------------------------------------------------------------

// updateCenterStatusResponseFromCR maps a v1alpha1.UpdateCenter CRD to the
// BFF status response DTO. Sets Enabled: true. Does NOT include
// uc.Status.SeedImportedDigests.
func updateCenterStatusResponseFromCR(uc *v1alpha1.UpdateCenter) UpdateCenterStatusResponse {
	resp := UpdateCenterStatusResponse{
		Enabled:            true,
		Phase:              string(uc.Status.Phase),
		PluginCount:        uc.Status.PluginCount,
		StoreBytes:         uc.Status.StoreBytes,
		LastSyncTime:       nil,
		StorageType:        string(uc.Spec.Storage.Type),
		PullThroughEnabled: uc.Spec.PullThrough.Enabled,
	}

	// Map conditions.
	if len(uc.Status.Conditions) > 0 {
		resp.Conditions = make([]UpdateCenterConditionDTO, len(uc.Status.Conditions))
		for i, c := range uc.Status.Conditions {
			resp.Conditions[i] = UpdateCenterConditionDTO{
				Type:               c.Type,
				Status:             string(c.Status),
				LastTransitionTime: metav1TimeToPtr(c.LastTransitionTime),
				Reason:             c.Reason,
				Message:            c.Message,
			}
		}
	} else {
		resp.Conditions = []UpdateCenterConditionDTO{}
	}

	// Map gaps.
	if len(uc.Status.Gaps) > 0 {
		resp.Gaps = make([]UpdateCenterGapDTO, len(uc.Status.Gaps))
		for i, g := range uc.Status.Gaps {
			resp.Gaps[i] = UpdateCenterGapDTO{
				Plugin:     g.Plugin,
				Version:    g.Version,
				RequiredBy: g.RequiredBy,
			}
		}
	} else {
		resp.Gaps = []UpdateCenterGapDTO{}
	}

	// LastSyncTime: convert metav1.Time to *time.Time, nil if zero.
	if !uc.Status.LastSyncTime.IsZero() {
		t := uc.Status.LastSyncTime.Time
		resp.LastSyncTime = &t
	}

	return resp
}

// metav1TimeToPtr converts a metav1.Time to a *time.Time, returning nil for zero values.
func metav1TimeToPtr(t metav1.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// ---------------------------------------------------------------------------
// §2.3 HandleUpdateCenterStatus
// ---------------------------------------------------------------------------

// HandleUpdateCenterStatus returns the update center status or a disabled
// shape when the UpdateCenter CR does not exist.
func (s *Server) HandleUpdateCenterStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uc, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, s.deps.Store, "varroa-update-center", "")
	if err != nil {
		if k8serrors.IsNotFound(err) {
			s.writeJSON(w, http.StatusOK, UpdateCenterStatusResponse{
				Enabled: false, Conditions: []UpdateCenterConditionDTO{}, Gaps: []UpdateCenterGapDTO{},
			})
			return
		}
		s.deps.Logger.Error("failed to get UpdateCenter CR", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to read update center status")
		return
	}
	s.writeJSON(w, http.StatusOK, updateCenterStatusResponseFromCR(uc))
}

// ---------------------------------------------------------------------------
// §2.4 HandleUpdateCenterPlugins
// ---------------------------------------------------------------------------

// HandleUpdateCenterPlugins returns the plugin inventory from the update
// center service, filtered by the optional ?q= query parameter.
func (s *Server) HandleUpdateCenterPlugins(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, s.deps.Store, "varroa-update-center", "")
	if err != nil {
		if k8serrors.IsNotFound(err) {
			s.writeJSON(w, http.StatusOK, UpdateCenterPluginsResponse{Enabled: false, Plugins: []UpdateCenterPluginDTO{}})
			return
		}
		s.deps.Logger.Error("failed to get UpdateCenter CR", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to read update center status")
		return
	}

	if s.deps.UpdateCenterInventory == nil {
		s.deps.Logger.Error("UpdateCenter CR exists but UpdateCenterInventory dependency is not wired")
		s.writeJSONError(w, http.StatusBadGateway, "update center service unreachable")
		return
	}

	inv, err := s.deps.UpdateCenterInventory.List(ctx)
	if err != nil {
		s.deps.Logger.Error("failed to fetch update center inventory", "error", err)
		s.writeJSONError(w, http.StatusBadGateway, "update center service unreachable")
		return
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	plugins := make([]UpdateCenterPluginDTO, 0, len(inv))
	for _, p := range inv {
		if q != "" && !strings.Contains(strings.ToLower(p.Name), q) {
			continue
		}
		plugins = append(plugins, UpdateCenterPluginDTO(p))
	}
	s.writeJSON(w, http.StatusOK, UpdateCenterPluginsResponse{Enabled: true, Plugins: plugins})
}

// ---------------------------------------------------------------------------
// §2.5 HandleUpdateCenterUpload
// ---------------------------------------------------------------------------

// HandleUpdateCenterUpload relays a plugin upload to the update center service
// with the caller's identity attached.
//
// The BFF produces exactly five envelopes of its own — malformed-upload (400),
// forbidden (403), update-center-disabled (409),
// update-center-status-unavailable (500) and update-center-unreachable (502) —
// each corresponding to a condition under which no update-center response
// exists. Every other status and body is the update center's, passed through
// byte for byte so the per-dependency rejection diff survives the hop unaltered.
func (s *Server) HandleUpdateCenterUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	claims := auth.ClaimsFromContext(ctx)
	// Fail closed on an unwired authorizer rather than panicking: this handler
	// writes to the plugin store, so a nil Authorizer must deny, never crash.
	if s.deps.Authorizer == nil || !s.deps.Authorizer.CanUploadPlugin(claims) {
		s.writeJSONError(w, http.StatusForbidden, "forbidden")
		return
	}

	// Uploading to a disabled update center is a client error, not the
	// Enabled:false read shape the GET handlers return.
	if _, err := crdstore.Get[v1alpha1.UpdateCenter](ctx, s.deps.Store, "varroa-update-center", ""); err != nil {
		if k8serrors.IsNotFound(err) {
			s.writeJSONError(w, http.StatusConflict, "update-center-disabled")
			return
		}
		s.deps.Logger.Error("failed to get UpdateCenter CR", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "update-center-status-unavailable")
		return
	}

	if s.deps.UpdateCenterUploader == nil {
		s.deps.Logger.Error("UpdateCenter CR exists but UpdateCenterUploader dependency is not wired")
		s.writeJSONError(w, http.StatusBadGateway, "update-center-unreachable")
		return
	}

	// Streaming, not ParseMultipartForm: a 256 MiB upload is never buffered in
	// BFF memory or spilled to BFF disk. This is the one piece of request
	// validation the BFF performs itself — without a readable part there is
	// nothing to relay.
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "malformed-upload")
		return
	}
	var (
		part     *multipart.Part
		filename string
	)
	for {
		p, nextErr := mr.NextPart()
		if nextErr != nil {
			break
		}
		if p.FormName() == "file" {
			part = p
			filename = p.FileName()
			break
		}
		_ = p.Close()
	}
	if part == nil {
		s.writeJSONError(w, http.StatusBadRequest, "malformed-upload")
		return
	}
	defer func() { _ = part.Close() }()

	dryRun := r.URL.Query().Get("dryRun") == "true"
	actor := actorFrom(claims)

	resp, err := s.deps.UpdateCenterUploader.Upload(ctx, actor, dryRun, uploadFilenameFor(filename), part)
	if err != nil {
		// No response was ever produced, so there is nothing to relay verbatim.
		s.deps.Logger.Error("update center upload failed at the transport layer", "error", err)
		s.writeJSONError(w, http.StatusBadGateway, "update-center-unreachable")
		return
	}

	contentType := resp.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if event, ok := uploadActivityEvent(actor, dryRun, resp.Body); ok {
			s.notifyActivity(event)
		}
	}
}

// uploadActivityEvent builds the activity event for a successful upload, or
// reports ok=false when there is nothing to record. The plugin identity and the
// fetched-dependency count are read back out of the RELAYED body rather than
// re-derived, so the event can never disagree with what the caller was told.
func uploadActivityEvent(actor string, dryRun bool, body []byte) (activity.Event, bool) {
	var result struct {
		Plugin struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugin"`
		Closure []struct {
			Fetched bool `json:"fetched"`
		} `json:"closure"`
	}
	if dryRun {
		// A preview stored nothing; there is nothing to record.
		return activity.Event{}, false
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return activity.Event{}, false
	}
	fetched := 0
	for _, c := range result.Closure {
		if c.Fetched {
			fetched++
		}
	}
	return activity.Event{
		Type:  "updatecenter.plugin.uploaded",
		Actor: actor,
		Message: fmt.Sprintf("Plugin %s@%s uploaded to the update center (%d dependencies fetched)",
			result.Plugin.Name, result.Plugin.Version, fetched),
	}, true
}
