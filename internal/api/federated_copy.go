package api

import (
	"encoding/json"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/internal/rbac"
)

// federatedCopyCode is the machine code of the 409 that refuses a write to a
// federated copy. Clients branch on it, so a repeat of the write does not
// succeed.
const federatedCopyCode = "federated_copy"

// refuseFederatedCopy answers true after it writes a 409 when the stored object
// is a federated copy. The decision reads the stored labels, because a request
// body can drop the label. The resourceVersion fence of the write closes the
// window after this read: a copy that appears after the read has a newer
// version, so the write answers 409 anyway. read returns the stored object. A
// missing object answers false, and the write follows the create rule.
func (s *Server) refuseFederatedCopy(w http.ResponseWriter, cluster, kind, name string, read func() (json.RawMessage, error)) bool {
	item, err := read()
	if err != nil {
		if isConfigNotFound(err) {
			return false
		}
		writeConfigBroodError(w, err, cluster)
		return true
	}
	var stored struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(item, &stored); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "decode stored "+kind+": "+err.Error())
		return true
	}
	msg := rbac.FederatedCopyMessage(kind, name, stored.Metadata.Labels, stored.Metadata.Annotations)
	if msg == "" {
		return false
	}
	s.writeJSONErrorStatus(w, http.StatusConflict, msg, map[string]any{"code": federatedCopyCode})
	return true
}

// federatedLabelCode is the machine code of the 400 that refuses a request body
// that sets the federation label. A repeat of the same body does not succeed.
const federatedLabelCode = "federated_label"

// refuseFederatedLabel answers true after it writes a 400 when the request body
// sets rbac.LabelFederatedFrom. The federation reconciler writes its copies
// through ConfigBrood directly, never through these routes, so this guard sees
// only callers. A PUT calls it after refuseFederatedCopy, so a write to a stored
// copy keeps the 409 federated_copy answer.
func (s *Server) refuseFederatedLabel(w http.ResponseWriter, kind string, labels map[string]string) bool {
	msg := rbac.FederatedLabelMessage(kind, labels)
	if msg == "" {
		return false
	}
	s.writeJSONErrorStatus(w, http.StatusBadRequest, msg, map[string]any{"code": federatedLabelCode})
	return true
}
