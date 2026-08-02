package api

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
)

// UpdateCenterUploadResponse is the relayed update-center response: its status
// and its body, byte for byte.
//
// The BFF deliberately does NOT decode the envelope. The update center owns
// every code below the five the BFF originates, and the per-dependency rejection
// diff has to survive the hop unaltered — re-marshalling it here would be a
// second, drift-prone copy of the wire shape.
type UpdateCenterUploadResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// UpdateCenterUploader uploads a plugin artifact to the update center service.
type UpdateCenterUploader interface {
	Upload(ctx context.Context, uploadedBy string, dryRun bool, filename string, body io.Reader) (*UpdateCenterUploadResponse, error)
}

// updatecenterUploadClient is the HTTP implementation, sibling to
// updatecenterHTTPClient. It holds the import token: the BFF is the only
// component that authenticates a real user, so it is the only one that can
// attribute an upload.
type updatecenterUploadClient struct {
	baseURL     string
	importToken string
	httpClient  *http.Client
}

// NewUpdateCenterUploadClient creates an HTTP-backed UpdateCenterUploader.
func NewUpdateCenterUploadClient(baseURL, importToken string, httpClient *http.Client) UpdateCenterUploader {
	return &updatecenterUploadClient{baseURL: baseURL, importToken: importToken, httpClient: httpClient}
}

func (c *updatecenterUploadClient) Upload(ctx context.Context, uploadedBy string, dryRun bool, filename string, body io.Reader) (*UpdateCenterUploadResponse, error) {
	// Stream the re-encoded multipart body through a pipe: a 256 MiB upload must
	// never be buffered in BFF memory or spilled to BFF disk.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := mw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	target := c.baseURL + "/api/v1/plugins"
	if dryRun {
		target += "?dryRun=true"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, pr)
	if err != nil {
		return nil, fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.importToken)
	if uploadedBy != "" {
		req.Header.Set("X-Varroa-Uploaded-By", uploadedBy)
	}

	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upload response: %w", err)
	}
	return &UpdateCenterUploadResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        payload,
	}, nil
}

// uploadFilenameFor returns a safe filename for the relayed part. The update
// center reads the bytes, not the name, so this only has to be non-empty and
// free of path separators.
func uploadFilenameFor(name string) string {
	if name == "" {
		return "plugin.hpi"
	}
	return url.PathEscape(name)
}
