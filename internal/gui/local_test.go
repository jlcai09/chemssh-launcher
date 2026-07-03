package gui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalWriteHandlersRejectEmptyPaths(t *testing.T) {
	server := &Server{transfers: newTransferProgressStore()}
	tests := []struct {
		name    string
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{
			name:    "delete empty path",
			path:    "/api/local/delete",
			body:    `{"path":""}`,
			handler: server.handleLocalDelete,
		},
		{
			name:    "rename empty source",
			path:    "/api/local/rename",
			body:    `{"path":"","new_path":"target.txt"}`,
			handler: server.handleLocalRename,
		},
		{
			name:    "rename empty target",
			path:    "/api/local/rename",
			body:    `{"path":"source.txt","new_path":""}`,
			handler: server.handleLocalRename,
		},
		{
			name:    "copy empty source",
			path:    "/api/local/copy",
			body:    `{"source_path":"","target_path":"target"}`,
			handler: server.handleLocalCopy,
		},
		{
			name:    "copy empty target",
			path:    "/api/local/copy",
			body:    `{"source_path":"source.txt","target_path":""}`,
			handler: server.handleLocalCopy,
		},
		{
			name:    "open empty path",
			path:    "/api/local/open",
			body:    `{"path":""}`,
			handler: server.handleLocalOpen,
		},
		{
			name:    "open text empty path",
			path:    "/api/local/open-text",
			body:    `{"path":""}`,
			handler: server.handleLocalOpenText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			tt.handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}
