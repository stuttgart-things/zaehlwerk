package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stuttgart-things/zaehlwerk/internal/api"
	"github.com/stuttgart-things/zaehlwerk/internal/match"
	"github.com/stuttgart-things/zaehlwerk/internal/ui"
)

// The two servers share one address, and which of them answers what is decided
// in exactly one place. Registering the patterns is also where a conflict
// between them would panic, which is worth finding here rather than at startup.
func TestRoutes(t *testing.T) {
	registry := match.NewRegistry()
	apiSrv := api.New(registry)
	handler := routes(apiSrv, ui.New(registry, apiSrv))

	for _, tc := range []struct {
		name     string
		path     string
		status   int
		location string
	}{
		{"the root is the page", "/", http.StatusFound, "/ui"},
		{"the page", "/ui", http.StatusOK, ""},
		{"the page, with a slash", "/ui/", http.StatusOK, ""},
		{"everything else is the API's", "/healthz", http.StatusOK, ""},
		{"including what it does not serve", "/nope", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			require.Equal(t, tc.status, rec.Code)
			if tc.location != "" {
				require.Equal(t, tc.location, rec.Header().Get("Location"))
			}
		})
	}
}
