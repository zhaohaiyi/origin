package assets

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/golang/glog"
)

var varyHeaderRegexp = regexp.MustCompile("\\s*,\\s*")

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	sniffDone bool
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.sniffDone {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.sniffDone = true
	}
	return w.Writer.Write(b)
}

// GzipHandler wraps a http.Handler to support transparent gzip encoding.
func GzipHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}
		// Normalize the Accept-Encoding header for improved caching
		r.Header.Set("Accept-Encoding", "gzip")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(&gzipResponseWriter{Writer: gz, ResponseWriter: w}, r)
	})
}

func generateEtag(r *http.Request, version string, varyHeaders []string) string {
	varyHeaderValues := ""
	for _, varyHeader := range varyHeaders {
		varyHeaderValues += r.Header.Get(varyHeader)
	}
	return fmt.Sprintf("W/\"%s_%s\"", version, hex.EncodeToString([]byte(varyHeaderValues)))
}

func CacheControlHandler(version string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vary := w.Header().Get("Vary")
		varyHeaders := []string{}
		if vary != "" {
			varyHeaders = varyHeaderRegexp.Split(vary, -1)
		}
		etag := generateEtag(r, version, varyHeaders)

		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Add("ETag", etag)
		h.ServeHTTP(w, r)

	})
}

type LongestToShortest []string

func (s LongestToShortest) Len() int {
	return len(s)
}
func (s LongestToShortest) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s LongestToShortest) Less(i, j int) bool {
	return len(s[i]) > len(s[j])
}

// HTML5ModeHandler will serve any static assets we know about, all other paths
// are assumed to be HTML5 paths for the console application and index.html will
// be served.
// contextRoot must contain leading and trailing slashes, e.g. /console/
//
// subcontextMap is a map of keys (subcontexts, no leading or trailing slashes) to the asset path (no
// leading slash) to serve for that subcontext if a resource that does not exist is requested
func HTML5ModeHandler(contextRoot string, subcontextMap map[string]string, h http.Handler) (http.Handler, error) {
	subcontextData := map[string][]byte{}
	subcontexts := []string{}

	for subcontext, index := range subcontextMap {
		b, err := Asset(index)
		if err != nil {
			return nil, err
		}
		b = bytes.Replace(b, []byte(`<base href="/">`), []byte(fmt.Sprintf(`<base href="%s">`, path.Join(contextRoot, subcontext))), 1)
		subcontextData[subcontext] = b
		subcontexts = append(subcontexts, subcontext)
	}

	// Sort alphabetically
	sort.Strings(subcontexts)
	// Then sort by length
	sort.Sort(LongestToShortest(subcontexts))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := strings.TrimPrefix(r.URL.Path, "/")
		if _, err := Asset(urlPath); err != nil {
			// find the index we want to serve instead
			for _, subcontext := range subcontexts {
				if urlPath == subcontext || strings.HasPrefix(urlPath, subcontext+"/") {
					w.Write(subcontextData[subcontext])
					return
				}
			}
		}
		h.ServeHTTP(w, r)
	}), nil
}

var configTemplate = template.Must(template.New("webConsoleConfig").Parse(`
window.OPENSHIFT_CONFIG = {
  api: {
    openshift: {
      hostPort: "{{ .MasterAddr | js}}",
      prefix: "{{ .MasterPrefix | js}}"
    },
    k8s: {
      hostPort: "{{ .KubernetesAddr | js}}",
      prefix: "{{ .KubernetesPrefix | js}}"
    }
  },
  auth: {
  	oauth_authorize_uri: "{{ .OAuthAuthorizeURI | js}}",
  	oauth_redirect_base: "{{ .OAuthRedirectBase | js}}",
  	oauth_client_id: "{{ .OAuthClientID | js}}",
  	logout_uri: "{{ .LogoutURI | js}}",
  }
};
`))

type WebConsoleConfig struct {
	// MasterAddr is the host:port the UI should call the master API on. Scheme is derived from the scheme the UI is served on, so they must be the same.
	MasterAddr string
	// MasterPrefix is the OpenShift API context root
	MasterPrefix string
	// TODO this is probably unneeded since everything goes through the openshift master's proxy
	// KubernetesAddr is the host:port the UI should call the kubernetes API on. Scheme is derived from the scheme the UI is served on, so they must be the same.
	KubernetesAddr string
	// KubernetesPrefix is the Kubernetes API context root
	KubernetesPrefix string
	// OAuthAuthorizeURL is the OAuth2 endpoint to use to request an API token. It must support request_type=token.
	OAuthAuthorizeURI string
	// OAuthRedirectBase is the base URI of the web console. It must be a valid redirect_uri for the OAuthClientID
	OAuthRedirectBase string
	// OAuthClientID is the OAuth2 client_id to use to request an API token. It must be authorized to redirect to the web console URL.
	OAuthClientID string
	// LogoutURI is an optional (absolute) URI to redirect to after completing a logout. If not specified, the built-in logout page is shown.
	LogoutURI string
}

func GeneratedConfigHandler(config WebConsoleConfig, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/") == "config.js" {
			w.Header().Add("Cache-Control", "no-cache, no-store")
			if err := configTemplate.Execute(w, config); err != nil {
				glog.Errorf("Unable to render config template: %v", err)
			}
			return
		}
		h.ServeHTTP(w, r)
	})
}
