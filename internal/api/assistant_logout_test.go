package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleLogout_ClearsAssistantKey(t *testing.T) {
	s := oidcTestServer(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/logout", nil)
	s.HandleLogout(w, r)

	for _, c := range w.Result().Cookies() {
		if c.Name == AssistantKeyCookie {
			if c.MaxAge != -1 {
				t.Error("assistant key cookie should be cleared (MaxAge=-1)")
			}
			if c.Path != "/api/v1" {
				t.Errorf("assistant key cookie path = %q, want /api/v1", c.Path)
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("assistant key cookie SameSite = %v, want Strict", c.SameSite)
			}
			return
		}
	}
	t.Error("logout did not clear the assistant key cookie")
}
