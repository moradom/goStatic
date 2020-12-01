// This small program is just a small web server created in static mode
// in order to provide the smallest docker image possible

package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	// Def of flags
	portPtr                  = flag.Int("port", 1080, "The listening port")
	context                  = flag.String("context", "", "The 'context' path on which files are served, e.g. 'doc' will serve the files at 'http://localhost:<port>/doc/'")
	basePath                 = flag.String("path", "/srv/http", "The path for the static files")
	fallbackPath             = flag.String("fallback", "/index.html", "Default fallback file. Either absolute for a specific asset (/index.html), or relative to recursively resolve (index.html)")
	headerFlag               = flag.String("append-header", "", "HTTP response header, specified as `HeaderName:Value` that should be added to all responses.")
	basicAuth                = flag.Bool("enable-basic-auth", false, "Enable basic auth. By default, password are randomly generated. Use --set-basic-auth to set it.")
	healthCheck              = flag.Bool("enable-health", false, "Enable health check endpoint. You can call /health to get a 200 response. Useful for Kubernetes, OpenFaas, etc.")
	setBasicAuth             = flag.String("set-basic-auth", "", "Define the basic auth. Form must be user:password")
	defaultUsernameBasicAuth = flag.String("default-user-basic-auth", "gopher", "Define the user")
	sizeRandom               = flag.Int("password-length", 16, "Size of the randomized password")
	logRequest               = flag.Bool("enable-logging", false, "Enable log request")
	httpsPromote             = flag.Bool("https-promote", false, "All HTTP requests should be redirected to HTTPS")
	headerConfigPath         = flag.String("header-config-path", "/config/headerConfig.json", "Path to the config file for custom response headers")

	username string
	password string

	defaultPageBytes []byte
)

func parseHeaderFlag(headerFlag string) (string, string) {
	if len(headerFlag) == 0 {
		return "", ""
	}
	pieces := strings.SplitN(headerFlag, ":", 2)
	if len(pieces) == 1 {
		return pieces[0], ""
	}
	return pieces[0], pieces[1]
}

var gzPool = sync.Pool{
	New: func() interface{} {
		w := gzip.NewWriter(ioutil.Discard)
		return w
	},
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func handleReq(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *httpsPromote && r.Header.Get("X-Forwarded-Proto") == "http" {
			http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
			if *logRequest {
				log.Println(301, r.Method, r.URL.Path)
			}
			return
		}

		if *logRequest {
			log.Println(r.Method, r.URL.Path)
		}

		h.ServeHTTP(w, r)
	})
}

func parseFallbackPage() {
	if len(flag.Args()) % 2 != 0 {
		log.Println("Passing variables to be replaced on base file needs to be done by pair var value")
		os.Exit(1)
	}

	data, err := ioutil.ReadFile(*basePath + *fallbackPath)

	if err != nil {
		log.Println("Unable to open file " + *basePath + *fallbackPath)
		os.Exit(2)
	}

	page := string(data)

	for i := len(flag.Args()) -1; i >= 1; i -= 2 {
		regex := regexp.MustCompile(`'`+flag.Arg(i-1)+`' *: *'[^']*'`)
		page = regex.ReplaceAllString(page, `'`+flag.Arg(i-1)+`':'`+flag.Arg(i)+`'`)
	}

	defaultPageBytes = []byte(page)

	err = ioutil.WriteFile(*basePath + *fallbackPath, defaultPageBytes, 0644)

	if err != nil {
		log.Println("Unable to write file " + *basePath + *fallbackPath)
		os.Exit(3)
	}

}

func defaultPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if len(r.RequestURI) == 0 || r.URL.RequestURI() == "/" || r.URL.RequestURI() == *fallbackPath  {
			log.Println("Passing here " + r.URL.RequestURI())
			w.Header().Set("Content-Type", "text/html") // clarify return type (MIME)
			_, _ = w.Write(defaultPageBytes)
		} else {
			next.ServeHTTP(w, r)
		}
	})
}

func main() {

	flag.Parse()

	// sanity check
	if len(*setBasicAuth) != 0 && !*basicAuth {
		*basicAuth = true
	}

	port := ":" + strconv.FormatInt(int64(*portPtr), 10)

	var fileSystem http.FileSystem = http.Dir(*basePath)

	if *fallbackPath != "" {
		fileSystem = fallback{
			defaultPath: *fallbackPath,
			fs:          fileSystem,
		}
	}

	handler := handleReq(http.FileServer(fileSystem))

	if *fallbackPath != "" {
		parseFallbackPage()
		handler = defaultPage(handler)
	}

	pathPrefix := "/"
	if len(*context) > 0 {
		pathPrefix = "/" + *context + "/"
		handler = http.StripPrefix(pathPrefix, handler)
	}

	if *basicAuth {
		log.Println("Enabling Basic Auth")
		if len(*setBasicAuth) != 0 {
			parseAuth(*setBasicAuth)
		} else {
			generateRandomAuth()
		}
		handler = authMiddleware(handler)
	}

	headerConfigValid := initHeaderConfig(*headerConfigPath)
	if headerConfigValid {
		handler = customHeadersMiddleware(handler)
	}

	// Extra headers.
	if len(*headerFlag) > 0 {
		header, headerValue := parseHeaderFlag(*headerFlag)
		if len(header) > 0 && len(headerValue) > 0 {
			fileServer := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(header, headerValue)
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
					fileServer.ServeHTTP(w, r)
				} else {
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzPool.Get().(*gzip.Writer)
					defer gzPool.Put(gz)

					gz.Reset(w)
					defer gz.Close()
					fileServer.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
				}

			})
		} else {
			log.Println("appendHeader misconfigured; ignoring.")
		}
	}

	if *healthCheck {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "Ok")
		})
	}

	http.Handle(pathPrefix, handler)

	log.Printf("Listening at 0.0.0.0%v %v...", port, pathPrefix)
	log.Fatalln(http.ListenAndServe(port, nil))
}
