package gui

import (
	"compress/gzip"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
)

// copyBufferSize is the buffer size for file transfer operations.
const copyBufferSize = 256 * 1024

var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(io.Discard)
	},
}

// compressibleExts lists URL extensions whose content benefits from gzip.
// Anything outside this set is served as-is (already compressed binary formats:
// png/jpg/woff2/zip, etc.). Empty extension is treated as compressible because
// directory roots typically serve HTML.
var compressibleExts = map[string]bool{
	"":      true, // "/" or "/static/" → index.html
	".css":  true,
	".html": true,
	".htm":  true,
	".js":   true,
	".json": true,
	".map":  true,
	".mjs":  true,
	".svg":  true,
	".txt":  true,
	".xml":  true,
}

// shouldCompressPath returns true when the URL path's extension is known to be
// worth gzipping. Use path.Ext (URL-aware) rather than filepath.Ext (OS-aware).
func shouldCompressPath(urlPath string) bool {
	return compressibleExts[strings.ToLower(path.Ext(urlPath))]
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer io.Writer
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// gzipHandler wraps a handler to add gzip compression
func gzipHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next(w, r)
			return
		}

		// Get gzip writer from pool
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			gz.Close()
			gzipWriterPool.Put(gz)
		}()

		// Set headers
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length") // Let gzip determine the length

		// Wrap response writer
		gzipWriter := gzipResponseWriter{ResponseWriter: w, Writer: gz}
		next(gzipWriter, r)
	}
}

// staticGzipHandler wraps http.Handler for static files. Only paths whose
// extension is in compressibleExts get wrapped — already-compressed formats
// like PNG/JPG/WOFF2 are served as-is so we don't burn CPU re-deflating them.
func staticGzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		// Skip already-compressed formats (PNG, JPG, WOFF2, ...).
		if !shouldCompressPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Get gzip writer from pool
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			gz.Close()
			gzipWriterPool.Put(gz)
		}()

		// Set headers
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")

		// Wrap response writer
		gzipWriter := gzipResponseWriter{ResponseWriter: w, Writer: gz}
		next.ServeHTTP(gzipWriter, r)
	})
}
