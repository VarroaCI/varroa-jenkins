package apikey

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// VerifyHandler returns an http.Handler that implements the gateway verify
// endpoint contract. See design.md D1 and specs/vk-token-jenkins-auth.
func VerifyHandler(v *Verifier, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if len(authHeader) < 7 || authHeader[:7] != "Bearer " {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		token := authHeader[7:]

		claims, err := v.VerifyFresh(r.Context(), token)
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				logger.Warn("verify unavailable", "error", err)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			logger.Debug("verify rejected", "error", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		groups := claims.Groups
		if groups == nil {
			groups = make([]string, 0)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"subject":           claims.Subject,
			"preferredUsername": claims.PreferredUsername,
			"email":             claims.Email,
			"name":              claims.Name,
			"groups":            groups,
		})
	})
}
