package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
		<p class="htmx-indicator">Queued...</p>
	</div>
	<input id="uri" name="uri" type="text" hx-post="/identify" hx-target="#result" hx-indicator="#result">
	<button>Submit</button>
</html>
`))

var templJob = template.Must(template.New("job").Parse(`
{{ if ne .State "done" }}
	<div hx-get="/job/{{ .ID }}" hx-trigger="load delay:1s" hx-swap="outerHTML">
		<img src="/static/bars.svg" style="background-color: coral">
		<p>{{ .State }}...</p>
	</div>
{{ else if .Error }}
	<div>Error: {{ .Error }}</div>
{{ else }}
	{{ with .Sample }}
		{{ if .Found }}
			<div>
				<p>{{ .Artist }} - {{ .Title }}</p>
				<p>Links:
					{{ range $key, $value := .Links }}
						<a href="{{ $value }}">{{ $key }}</a>
					{{ end }}
				</p>
			</div>
		{{ else }}
			<div>Sample not found :(</div>
		{{ end }}
	{{ end}}
{{ end }}
`))

func jobID(uri string) string {
	h := sha256.Sum256([]byte(uri))
	return fmt.Sprintf("%x", h[:8])
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

type identifyJob struct {
	ID     string      `json:"id"`
	State  string      `json:"state"`
	URI    string      `json:"uri"`
	Sample sampleEntry `json:"sample"`
	Error  string      `json:"error,omitempty"`
}

type requestLogLine struct {
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Error       string    `json:"error,omitempty"`
	Request     string    `json:"request,omitempty"`
	URIKnown    bool      `json:"uriKnown,omitempty"`
	URI         string    `json:"uri,omitempty"`
	ResolveTime string    `json:"resolveTime,omitempty"`
	SampleKnown bool      `json:"sampleKnown,omitempty"`
}

type identifyJobLogLine struct {
	Start time.Time    `json:"start"`
	End   time.Time    `json:"end"`
	Job   *identifyJob `json:"job"`
}

type logLine struct {
	Type    string              `json:"type"`
	Request *requestLogLine     `json:"request,omitempty"`
	Job     *identifyJobLogLine `json:"job,omitempty"`
}

type server struct {
	jobs     map[string]*identifyJob
	uriCache map[string]mediaURI
	jobQueue []string
	log      io.Writer
	mu       sync.Mutex
}

func (s *server) handleJob(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	s.mu.Lock()
	j, ok := s.jobs[ps.ByName("id")]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if req.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(j)
	} else {
		templJob.Execute(w, j)
	}
}

func (s *server) handleIdentify(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
	rll := &requestLogLine{
		Start:   time.Now(),
		Request: req.URL.String(),
		URI:     req.FormValue("uri"),
	}
	defer func() {
		rll.End = time.Now()
		json.NewEncoder(s.log).Encode(logLine{Type: "request", Request: rll})
	}()

	jobID := jobID(req.FormValue("uri"))
	s.mu.Lock()
	j, ok := s.jobs[jobID]
	if !ok {
		j = &identifyJob{
			ID:    jobID,
			State: "queued",
			URI:   req.FormValue("uri"),
		}
		s.jobs[j.ID] = j
		s.jobQueue = append(s.jobQueue, j.ID)
	}
	s.mu.Unlock()
	rll.SampleKnown = j.Sample.Found
	if req.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(j)
	} else {
		templJob.Execute(w, j)
	}
}

func (s *server) handleRoot(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	templRoot.Execute(w, nil)
}

func (s *server) doJob(j *identifyJob) {
	setState := func(state string) {
		s.mu.Lock()
		j.State = state
		s.mu.Unlock()
	}
	defer setState("done")

	s.mu.Lock()
	uri, ok := s.uriCache[j.URI]
	s.mu.Unlock()
	if !ok {
		setState("resolving")
		var isAlbum bool
		var err error
		uri, isAlbum, err = resolveURI(j.URI)
		if _, ok := uri.(mediaFile); ok {
			err = errors.New("local files are not supported")
		} else if isAlbum {
			err = errors.New("albums are not supported")
		}
		if err != nil {
			j.Error = err.Error()
			return
		}
		s.mu.Lock()
		s.uriCache[j.URI] = uri
		s.mu.Unlock()
	}

	setState("fetching")
	path, err := fetchTrack(uri)
	if err != nil {
		j.Error = err.Error()
		return
	}
	setState("identifying")
	id := newTrackIdentifier(path)
	for {
		res, err := identifyPath(path, id.currentParams())
		if err != nil {
			j.Error = err.Error()
			return
		} else if id.handleResult(res) == nil {
			break
		}
	}
	if id.sample == nil {
		j.Sample = sampleEntry{Found: false}
		return
	}
	setState("linking")
	links, _ := shazam.Links(id.sample.res.AppleID)
	j.Sample = sampleEntry{
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
	}
}

func (s *server) loopJobs() {
	for ; ; time.Sleep(time.Second) {
		s.mu.Lock()
		if len(s.jobQueue) == 0 {
			s.mu.Unlock()
			continue
		}
		jobID := s.jobQueue[0]
		s.jobQueue = s.jobQueue[1:]
		j, ok := s.jobs[jobID]
		if !ok {
			panic("unknown job in queue")
		}
		s.mu.Unlock()

		start := time.Now()
		s.doJob(j)
		ll := logLine{Type: "job", Job: &identifyJobLogLine{Start: start, End: time.Now(), Job: j}}
		json.NewEncoder(s.log).Encode(ll)
	}
}

func newServer(dir string) (http.Handler, error) {
	logFile, err := os.OpenFile(path.Join(dir, "barbershop.log"), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	jobs := make(map[string]*identifyJob)

	// scan log lines as json
	s := bufio.NewScanner(logFile)
	for s.Scan() {
		var ll logLine
		if err := json.Unmarshal(s.Bytes(), &ll); err != nil {
			return nil, err
		} else if ll.Job != nil {
			jobs[ll.Job.Job.ID] = ll.Job.Job
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	srv := &server{
		jobs:     jobs,
		uriCache: make(map[string]mediaURI),
		log:      logFile,
	}
	go srv.loopJobs()
	mux := httprouter.New()
	mux.GET("/", srv.handleRoot)
	mux.POST("/identify", srv.handleIdentify)
	mux.GET("/job/:id", srv.handleJob)
	mux.GET("/static/*path", func(w http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		http.ServeFile(w, req, path.Join("static", ps.ByName("path")))
	})
	return mux, nil
}
