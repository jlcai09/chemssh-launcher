package gui

import (
	"errors"
	"net/http"
	"strconv"

	"chemssh-launcher/internal/fileicon"
)

func (s *Server) handleFileIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}

	query := r.URL.Query()
	name := query.Get("name")
	isDir := query.Get("is_dir") == "1" || query.Get("is_dir") == "true"
	size, _ := strconv.Atoi(query.Get("size"))

	// Lazy initialize icon service on first use
	s.iconsOnce.Do(func() {
		s.icons = fileicon.NewService()
	})

	data, err := s.icons.IconPNG(name, isDir, size)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, fileicon.ErrUnsupported) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}
