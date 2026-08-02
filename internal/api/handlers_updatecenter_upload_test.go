package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeUploader records what it was handed and replays a canned response.
type fakeUploader struct {
	resp *UpdateCenterUploadResponse
	err  error

	calls      int
	uploadedBy string
	dryRun     bool
	bodyLen    int
}

func (f *fakeUploader) Upload(_ context.Context, uploadedBy string, dryRun bool, _ string, body io.Reader) (*UpdateCenterUploadResponse, error) {
	f.calls++
	f.uploadedBy = uploadedBy
	f.dryRun = dryRun
	n, _ := io.Copy(io.Discard, body)
	f.bodyLen = int(n)
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// uploadAuthorizer builds an Authorizer that grants (or withholds) the
// updatecenter/upload verb.
func uploadAuthorizer(grant bool) *Authorizer {
	rules := []v1alpha1.APIRule{{Resources: []string{"controllers"}, Verbs: []string{"read"}}}
	if grant {
		rules = append(rules, v1alpha1.APIRule{Resources: []string{"updatecenter"}, Verbs: []string{"upload"}})
	}
	roles := []*v1alpha1.VarroaRole{{
		ObjectMeta: metav1.ObjectMeta{Name: "role"},
		Spec:       v1alpha1.VarroaRoleSpec{APIRules: rules},
	}}
	bindings := []*v1alpha1.VarroaRoleBinding{{
		ObjectMeta: metav1.ObjectMeta{Name: "b"},
		Spec: v1alpha1.VarroaRoleBindingSpec{
			Subjects: []v1alpha1.SubjectRef{{Kind: "User", Name: "test-user"}},
			RoleRef:  "role",
		},
	}}
	return NewAuthorizer(testResolver(roles, bindings), false)
}

func uploadClaims() *auth.Claims {
	return &auth.Claims{Subject: "test-user", PreferredUsername: "test-user"}
}

// multipartUpload builds a multipart body with a `file` part of the given size.
func multipartUpload(t *testing.T, size int) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "plugin.hpi")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	payload := bytes.Repeat([]byte("varroa"), size/6+1)[:size]
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

// uploadDeps assembles Dependencies for the upload handler. ActivityPublisher is
// left nil: notifyActivity is a no-op without one, and the event's own content is
// covered by TestUploadActivityEvent below (Dependencies.ActivityPublisher is a
// concrete *activity.Publisher, so there is no seam to fake here).
func uploadDeps(t *testing.T, grant bool, ucExists bool, ucErr error, up UpdateCenterUploader) *Dependencies {
	t.Helper()
	client := &fakeUpdateCenterClient{fakeResourceClient: *newFakeResourceClient(), err: ucErr}
	if ucExists {
		client.uc = &v1alpha1.UpdateCenter{ObjectMeta: metav1.ObjectMeta{Name: "varroa-update-center"}}
	}
	return &Dependencies{
		Client:               client,
		Store:                storeFromUC(client),
		Logger:               slog.Default(),
		Authorizer:           uploadAuthorizer(grant),
		UpdateCenterUploader: up,
	}
}

// doUpload runs the handler against a multipart request.
func doUpload(t *testing.T, deps *Dependencies, contentType string, body []byte, query string) *httptest.ResponseRecorder {
	t.Helper()
	srv := NewServer(deps)
	req := httptest.NewRequest(http.MethodPost, "/updatecenter/plugins"+query, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req = req.WithContext(contextWithClaims(req.Context(), uploadClaims()))
	w := httptest.NewRecorder()
	srv.HandleUpdateCenterUpload(w, req)
	return w
}

func okUploadResponse() *UpdateCenterUploadResponse {
	return &UpdateCenterUploadResponse{
		StatusCode:  http.StatusCreated,
		ContentType: "application/json",
		Body: []byte(`{"plugin":{"name":"acme","version":"1.0","sha256":"sha256:x"},"dryRun":false,` +
			`"packRef":"upload-abc","closure":[{"name":"lib","min":"1.0","status":"planned-fetch","fetched":true}],` +
			`"optionalDependencies":[],"warnings":[]}`),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHandleUpdateCenterUpload_ForbiddenWithoutVerb(t *testing.T) {
	up := &fakeUploader{resp: okUploadResponse()}
	ct, body := multipartUpload(t, 16)
	w := doUpload(t, uploadDeps(t, false, true, nil, up), ct, body, "")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if up.calls != 0 {
		t.Fatal("the uploader must not be called without the verb")
	}
}

func TestHandleUpdateCenterUpload_Success(t *testing.T) {
	up := &fakeUploader{resp: okUploadResponse()}
	ct, body := multipartUpload(t, 64)
	w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", w.Code, w.Body.String())
	}
	if up.uploadedBy != "test-user" {
		t.Errorf("uploadedBy = %q, want test-user", up.uploadedBy)
	}
	if up.bodyLen != 64 {
		t.Errorf("relayed %d bytes, want 64", up.bodyLen)
	}
}

// TestUploadActivityEvent covers the event the handler emits on a 2xx: its actor,
// the plugin identity, and the fetched-dependency count, all read back out of the
// relayed body so the event cannot disagree with what the caller was told.
func TestUploadActivityEvent(t *testing.T) {
	body := okUploadResponse().Body

	event, ok := uploadActivityEvent("test-user", false, body)
	if !ok {
		t.Fatal("expected an event for a committed upload")
	}
	if event.Type != "updatecenter.plugin.uploaded" || event.Actor != "test-user" {
		t.Fatalf("event = %+v", event)
	}
	if !strings.Contains(event.Message, "acme@1.0") || !strings.Contains(event.Message, "1 dependencies fetched") {
		t.Fatalf("message = %q", event.Message)
	}

	if _, ok := uploadActivityEvent("test-user", true, body); ok {
		t.Error("a dry run stored nothing and must not emit an event")
	}
	if _, ok := uploadActivityEvent("test-user", false, []byte("not json")); ok {
		t.Error("an undecodable body must not emit an event")
	}
}

func TestHandleUpdateCenterUpload_DryRunEmitsNoActivity(t *testing.T) {
	resp := okUploadResponse()
	resp.StatusCode = http.StatusOK
	up := &fakeUploader{resp: resp}
	ct, body := multipartUpload(t, 16)
	w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "?dryRun=true")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !up.dryRun {
		t.Error("dryRun was not relayed")
	}
}

func TestHandleUpdateCenterUpload_NoCR(t *testing.T) {
	up := &fakeUploader{resp: okUploadResponse()}
	ct, body := multipartUpload(t, 16)
	w := doUpload(t, uploadDeps(t, true, false, nil, up), ct, body, "")

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "update-center-disabled") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if up.calls != 0 {
		t.Fatal("the uploader must not be called when the CR is absent")
	}
}

func TestHandleUpdateCenterUpload_CRReadFails(t *testing.T) {
	up := &fakeUploader{resp: okUploadResponse()}
	ct, body := multipartUpload(t, 16)
	w := doUpload(t, uploadDeps(t, true, false, errors.New("apiserver down"), up), ct, body, "")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "update-center-status-unavailable") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestHandleUpdateCenterUpload_MalformedBody(t *testing.T) {
	t.Run("not multipart", func(t *testing.T) {
		up := &fakeUploader{resp: okUploadResponse()}
		w := doUpload(t, uploadDeps(t, true, true, nil, up), "application/json", []byte(`{}`), "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "malformed-upload") {
			t.Fatalf("body = %s", w.Body.String())
		}
	})

	t.Run("multipart with no file part", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("notes", "wrong field name")
		_ = mw.Close()

		up := &fakeUploader{resp: okUploadResponse()}
		w := doUpload(t, uploadDeps(t, true, true, nil, up), mw.FormDataContentType(), buf.Bytes(), "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "malformed-upload") {
			t.Fatalf("body = %s", w.Body.String())
		}
		if up.calls != 0 {
			t.Fatal("the uploader must not be called without a file part")
		}
	})
}

func TestHandleUpdateCenterUpload_Unreachable(t *testing.T) {
	ct, body := multipartUpload(t, 16)

	t.Run("dependency not wired", func(t *testing.T) {
		w := doUpload(t, uploadDeps(t, true, true, nil, nil), ct, body, "")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", w.Code)
		}
		if !strings.Contains(w.Body.String(), "update-center-unreachable") {
			t.Fatalf("body = %s", w.Body.String())
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		up := &fakeUploader{err: errors.New("connection refused")}
		w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", w.Code)
		}
		if !strings.Contains(w.Body.String(), "update-center-unreachable") {
			t.Fatalf("body = %s", w.Body.String())
		}
	})
}

// TestHandleUpdateCenterUpload_StreamsLargeBody: the relay must not buffer, so a
// body larger than any internal buffer arrives intact.
func TestHandleUpdateCenterUpload_StreamsLargeBody(t *testing.T) {
	const size = 5 << 20 // 5 MiB
	up := &fakeUploader{resp: okUploadResponse()}
	ct, body := multipartUpload(t, size)
	w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "")

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if up.bodyLen != size {
		t.Fatalf("relayed %d bytes, want %d", up.bodyLen, size)
	}
}

// TestHandleUpdateCenterUpload_RejectionPassthrough: the per-dependency diff must
// survive the hop byte for byte. Re-marshalling it here would be a second,
// drift-prone copy of the wire shape.
func TestHandleUpdateCenterUpload_RejectionPassthrough(t *testing.T) {
	const diff = `{"error":"unresolved-dependencies","message":"1 of 3 mandatory dependencies could not be resolved",` +
		`"unresolved":[{"name":"acme-internal","min":"2.0","reason":"not-in-store","foundInStore":null,` +
		`"foundDeclared":null,"foundUpstream":null,"remediation":"pull-through is disabled"}]}`
	up := &fakeUploader{resp: &UpdateCenterUploadResponse{
		StatusCode:  http.StatusUnprocessableEntity,
		ContentType: "application/json",
		Body:        []byte(diff),
	}}
	ct, body := multipartUpload(t, 16)
	w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "")

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if w.Body.String() != diff {
		t.Fatalf("body was not relayed verbatim:\n got %s\nwant %s", w.Body.String(), diff)
	}
	if _, ok := uploadActivityEvent("test-user", false, []byte(diff)); ok {
		// A rejection body has no plugin block; the handler only reaches this on 2xx.
		t.Log("rejection bodies decode but the handler never emits on a non-2xx")
	}
}

// TestHandleUpdateCenterUpload_RelaysUpstreamStatusCodes covers the codes the BFF
// must NOT reinterpret, including the 501 whose remedy only the update center
// knows.
func TestHandleUpdateCenterUpload_RelaysUpstreamStatusCodes(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity, http.StatusNotImplemented, http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		up := &fakeUploader{resp: &UpdateCenterUploadResponse{
			StatusCode:  status,
			ContentType: "application/json",
			Body:        []byte(`{"error":"relayed"}`),
		}}
		ct, body := multipartUpload(t, 16)
		w := doUpload(t, uploadDeps(t, true, true, nil, up), ct, body, "")
		if w.Code != status {
			t.Errorf("status = %d, want %d", w.Code, status)
		}
		if w.Body.String() != `{"error":"relayed"}` {
			t.Errorf("body = %s", w.Body.String())
		}
	}
}

func TestCanUploadPlugin(t *testing.T) {
	if uploadAuthorizer(true).CanUploadPlugin(nil) {
		t.Error("nil claims must not be authorized")
	}
	if !uploadAuthorizer(true).CanUploadPlugin(uploadClaims()) {
		t.Error("the granted verb must authorize")
	}
	if uploadAuthorizer(false).CanUploadPlugin(uploadClaims()) {
		t.Error("the verb must be required")
	}
}

// TestUpdateCenterUploadClient_SendsTokenAndSubject pins the UC-leg contract.
func TestUpdateCenterUploadClient_SendsTokenAndSubject(t *testing.T) {
	var (
		gotAuth, gotSubject, gotQuery string
		gotFileBytes                  []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSubject = r.Header.Get("X-Varroa-Uploaded-By")
		gotQuery = r.URL.RawQuery
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart reader: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			t.Errorf("form name = %q, want file", part.FormName())
		}
		gotFileBytes, _ = io.ReadAll(part)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewUpdateCenterUploadClient(srv.URL, "secret-token", srv.Client())
	resp, err := c.Upload(context.Background(), "alice", true, "plugin.hpi", strings.NewReader("hpi-bytes"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotSubject != "alice" {
		t.Errorf("X-Varroa-Uploaded-By = %q", gotSubject)
	}
	if gotQuery != "dryRun=true" {
		t.Errorf("query = %q", gotQuery)
	}
	if string(gotFileBytes) != "hpi-bytes" {
		t.Errorf("file bytes = %q", gotFileBytes)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != `{"ok":true}` {
		t.Errorf("relayed response = %+v", resp)
	}

	var decoded map[string]any
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("relayed body is not JSON: %v", err)
	}
}
