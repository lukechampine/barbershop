package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"sync"
	"text/template"
	"time"

	"github.com/julienschmidt/httprouter"
	"lukechampine.com/barbershop/shazam"
)

var templRoot = template.Must(template.New("root").Parse(`
<!DOCTYPE html>
<html>
	<script src="/static/htmx.min.js"></script>
	<div id="result">
		<img class="htmx-indicator" src="/static/bars.svg" style="background-color: coral">
		<p class="htmx-indicator">Identifying...</p>
	</div>
	<input id="uri" name="uri" type="text" hx-post="/identify" hx-target="#result" hx-indicator="#result">
	<button>Submit</button>
</html>
`))

var templIdentify = template.Must(template.New("identify").Parse(`
{{ if .Found }}
	<div>
		<p>{{ .Artist }} - {{ .Title }}</p>
		<p>Links: 
			{{ range $key, $value := .Links }}
				 <a href="{{ $value }}">{{ $key }}
			{{ end }}
		</p>
	</div>
{{ else }}
	<div>Sample not found :(</div>
{{ end }}
`))

func uriKey(uri mediaURI) string {
	switch uri := uri.(type) {
	case mediaFile:
		return "file:///" + uri.Path
	case mediaBandcamp:
		return "bandcamp:///" + uri.ArtistID + "/" + uri.Slug
	case mediaYouTube:
		return "youtube:///" + uri.ID
	default:
		panic("unreachable")
	}
}

type sampleEntry struct {
	Found  bool `json:"found"`
	Params struct {
		Speed     float64 `json:"speed"`
		Timestamp int64   `json:"timestamp"`
	} `json:"params,omitempty"`
	Artist string            `json:"artist,omitempty"`
	Title  string            `json:"title,omitempty"`
	Album  string            `json:"album,omitempty"`
	Year   string            `json:"year,omitempty"`
	Links  map[string]string `json:"links,omitempty"`
}

func identifySample(uri mediaURI) (sampleEntry, error) {
	path, err := fetchTrack(uri)
	if err != nil {
		return sampleEntry{}, err
	}
	id := newTrackIdentifier(path)
	for {
		res, err := identifyPath(path, id.currentParams())
		if err != nil {
			return sampleEntry{}, err
		}
		if id.handleResult(res) == nil {
			break
		}
	}
	if id.sample == nil {
		return sampleEntry{Found: false}, nil
	}
	links, _ := shazam.Links(id.sample.res.AppleID)
	return sampleEntry{
		Found: true,
		Params: struct {
			Speed     float64 `json:"speed"`
			Timestamp int64   `json:"timestamp"`
		}{
			Speed:     id.sample.params.ratio,
			Timestamp: id.sample.params.offset.Milliseconds(),
		},
		Artist: id.sample.res.Artist,
		Title:  id.sample.res.Title,
		Album:  id.sample.res.Album,
		Year:   id.sample.res.Year,
		Links:  links,
	}, nil
}

type logLine struct {
	Start        time.Time    `json:"start"`
	End          time.Time    `json:"end"`
	Error        string       `json:"error,omitempty"`
	Request      string       `json:"request,omitempty"`
	URIKnown     *bool        `json:"uriKnown,omitempty"`
	URI          string       `json:"uri,omitempty"`
	ResolveTime  string       `json:"resolveTime,omitempty"`
	SampleKnown  *bool        `json:"sampleKnown,omitempty"`
	Sample       *sampleEntry `json:"sample,omitempty"`
	IdentifyTime string       `json:"identifyTime,omitempty"`
}

func (ll *logLine) setErr(err error)                { ll.Error = err.Error() }
func (ll *logLine) setResolveTime(d time.Duration)  { ll.ResolveTime = d.String() }
func (ll *logLine) setURI(uri mediaURI)             { ll.URI = uriKey(uri) }
func (ll *logLine) setURIKnown(known bool)          { ll.URIKnown = &known }
func (ll *logLine) setSampleKnown(known bool)       { ll.SampleKnown = &known }
func (ll *logLine) setIdentifyTime(d time.Duration) { ll.IdentifyTime = d.String() }
func (ll *logLine) setSample(se sampleEntry)        { ll.Sample = &se }
func (ll *logLine) writeTo(w io.Writer) {
	ll.End = time.Now()
	json.NewEncoder(w).Encode(ll)
}

func newLogLine(req *http.Request) logLine {
	return logLine{
		Start:   time.Now(),
		Request: req.URL.String(),
	}
}

type server struct {
	samples  map[string]sampleEntry
	uriCache map[string]mediaURI
	log      io.Writer
	mu       sync.Mutex
}

func (s *server) handleIdentify(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	ll := newLogLine(req)
	defer ll.writeTo(s.log)

	reqURI := req.FormValue("uri")
	s.mu.Lock()
	uri, ok := s.uriCache[reqURI]
	s.mu.Unlock()
	ll.setURIKnown(ok)
	if !ok {
		start := time.Now()
		var isAlbum bool
		var err error
		uri, isAlbum, err = resolveURI(reqURI)
		ll.setResolveTime(time.Since(start))
		if _, ok := uri.(mediaFile); ok {
			err = errors.New("local files are not supported")
		} else if isAlbum {
			err = errors.New("albums are not supported")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			ll.setErr(err)
			return
		}
		ll.setURI(uri)
		s.mu.Lock()
		s.uriCache[reqURI] = uri
		s.mu.Unlock()
	}
	s.mu.Lock()
	se, ok := s.samples[uriKey(uri)]
	s.mu.Unlock()
	ll.setSampleKnown(ok)
	if !ok {
		start := time.Now()
		var err error
		se, err = identifySample(uri)
		ll.setIdentifyTime(time.Since(start))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			ll.setErr(err)
			return
		}
		ll.setSample(se)
		s.mu.Lock()
		s.samples[uriKey(uri)] = se
		s.mu.Unlock()
	}

	if req.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(se)
	} else {
		templIdentify.Execute(w, se)
	}
}

func (s *server) handleRoot(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	templRoot.Execute(w, nil)
}

func newServer(dir string) (http.Handler, error) {
	logFile, err := os.OpenFile(path.Join(dir, "barbershop.log"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	samples := make(map[string]sampleEntry)

	// scan log lines as json
	s := bufio.NewScanner(logFile)
	for s.Scan() {
		var ll logLine
		if err := json.Unmarshal(s.Bytes(), &ll); err != nil {
			return nil, err
		} else if ll.Sample != nil {
			samples[ll.URI] = *ll.Sample
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	srv := &server{
		samples:  samples,
		uriCache: make(map[string]mediaURI),
		log:      logFile,
	}
	mux := httprouter.New()
	mux.GET("/", srv.handleRoot)
	mux.POST("/identify", srv.handleIdentify)
	mux.GET("/static/*path", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		http.ServeFile(w, req, path.Join("static", ps.ByName("path")))
	})
	return mux, nil
}
