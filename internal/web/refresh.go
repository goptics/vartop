package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
)

// seenHeader carries the hash pair of a page's regions: "<live>-<rest>".
// The server sends it with each htmx GET of a page; a poll sends back the
// pair of what the browser shows (static/app.js).
const seenHeader = "X-Seen"

// skipUnchanged spares a poll what the browser has already. Same #live,
// #banner and #status: 204, htmx swaps nothing. Same #live only: #banner
// and #status alone, with HX-Reswap none, so #live keeps focus, selection
// and scroll. Else the page. Only htmx GETs are held back to look.
func skipUnchanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !isHTMX(r) {
			next.ServeHTTP(w, r)
			return
		}
		held := &heldResponse{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(held, r)
		page := held.body.Bytes()
		live, banner, status := region(page, "live"), region(page, "banner"), region(page, "status")
		if held.status == http.StatusOK && live != nil && status != nil && w.Header().Get("HX-Redirect") == "" {
			rest := append(bytes.Clone(banner), status...)
			liveSum, restSum := shortSum(live), shortSum(rest)
			w.Header().Set(seenHeader, liveSum+"-"+restSum)
			switch seen := r.Header.Get(seenHeader); {
			case seen == liveSum+"-"+restSum:
				w.Header().Del("Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			case strings.HasPrefix(seen, liveSum+"-"):
				w.Header().Set("HX-Reswap", "none")
				page = rest
			}
		}
		w.WriteHeader(held.status)
		_, _ = w.Write(page)
	})
}

// heldResponse keeps a response's status and body back; headers go
// straight to the real writer's.
type heldResponse struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (h *heldResponse) WriteHeader(status int)      { h.status = status }
func (h *heldResponse) Write(b []byte) (int, error) { return h.body.Write(b) }

// region is the <div id="id"> element of page, nil when page has none.
// Templates escape their data, so every "<div" in page is a tag.
func region(page []byte, id string) []byte {
	start := bytes.Index(page, []byte(`<div id="`+id+`"`))
	if start < 0 {
		return nil
	}
	for i, depth := start, 0; ; {
		open, end := bytes.Index(page[i:], []byte("<div")), bytes.Index(page[i:], []byte("</div>"))
		switch {
		case end < 0:
			return nil
		case open >= 0 && open < end:
			depth++
			i += open + len("<div")
		default:
			depth--
			i += end + len("</div>")
			if depth == 0 {
				return page[start:i]
			}
		}
	}
}

// shortSum is a short hash of b: enough to tell two renders apart.
func shortSum(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:6])
}

// gzipMin is the smallest body worth compressing.
const gzipMin = 1024

// gzipped compresses text bodies (HTML, CSS, JS, SVG) of gzipMin bytes and
// up, status 200, for a client that accepts gzip.
func gzipped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, status: http.StatusOK}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipWriter holds the status back until the first Write, which decides:
// handlers set their headers and write the body whole, the file server
// sets Content-Length first.
// ponytail: decides on first Write, buffer up to gzipMin if a handler ever writes in small pieces.
type gzipWriter struct {
	http.ResponseWriter
	status  int
	started bool
	zip     *gzip.Writer
}

func (g *gzipWriter) WriteHeader(status int) { g.status = status }

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.started {
		h := g.Header()
		size := len(b)
		if n, err := strconv.Atoi(h.Get("Content-Length")); err == nil {
			size = n
		}
		kind := h.Get("Content-Type")
		text := strings.HasPrefix(kind, "text/") || strings.HasPrefix(kind, "image/svg")
		if g.status == http.StatusOK && text && size >= gzipMin && h.Get("Content-Encoding") == "" {
			h.Set("Content-Encoding", "gzip")
			h.Del("Content-Length")
			g.zip = gzip.NewWriter(g.ResponseWriter)
		}
		g.start()
	}
	if g.zip != nil {
		return g.zip.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) start() {
	g.started = true
	g.ResponseWriter.WriteHeader(g.status)
}

func (g *gzipWriter) close() {
	if !g.started {
		g.start() // no body: 204, 304, HEAD
	}
	if g.zip != nil {
		_ = g.zip.Close()
	}
}

// staticFiles serves files with a weak ETag of each one's content and
// "no-cache": the browser keeps a file but asks on each use, so a new
// binary never runs with old CSS or JS.
func staticFiles(files fs.FS) http.Handler {
	tags := map[string]string{}
	err := fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(files, path)
		tags[path] = `W/"` + shortSum(b) + `"`
		return err
	})
	if err != nil {
		panic(err) // static is embedded: it is always there
	}
	serve := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		serve.ServeHTTP(w, r)
	})
}
