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
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/julienschmidt/httprouter"
	"lukechampine.com/barbershop/shazam"
)

var templRoot = template.Must(template.New("root").Parse(`
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <script src="/static/htmx.min.js"></script>
  <title>Barbershop</title>
  <link href="https://cdn.jsdelivr.net/npm/tailwindcss@2.2.16/dist/tailwind.min.css" rel="stylesheet">
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Righteous&family=Roboto:wght@400;700&display=swap" rel="stylesheet">
  <style>
    body {
      background: linear-gradient(135deg, #667eea, #764ba2);
      min-height: 100vh;
      display: flex;
      justify-content: center;
      align-items: center;
      font-family: 'Roboto', sans-serif;
    }

    .card {
      background-color: white;
      border-radius: 1rem;
      box-shadow: 0 20px 25px -5px rgba(0, 0, 0, 0.1), 0 10px 10px -5px rgba(0, 0, 0, 0.04);
      padding: 2rem;
    }

    .title {
      font-family: 'Righteous', sans-serif;
      font-weight: bold;
    }

    .link-container {
      display: flex;
      flex-direction: column;
    }

    .link-container a {
      display: flex;
      align-items: center;
      margin-bottom: 1rem;
    }

    .link-container a img {
      width: 24px;
      height: 24px;
      margin-right: 0.5rem;
    }

	.link-container iframe {
	  border-radius: 1em;
	  padding: 5px;
	}

    .fade-in {
      animation: fadeIn 0.3s ease-in-out;
    }

    @keyframes fadeIn {
      0% { opacity: 0; }
      100% { opacity: 1; }
    }

    .loading:after {
	  content: '';
	  animation: ellipsis 1.5s steps(4) infinite;
	  display: inline-block;
	  width: 1em;
	  text-align: left;
	}

    @keyframes ellipsis {
	  0% { content: ''; }
	  25% { content: '.'; }
	  50% { content: '..'; }
	  75% { content: '...'; }
	}
  </style>
</head>
<body>
  <div class="card">
    <h1 class="text-4xl font-bold text-gray-800 mb-4 title">Barbershop</h1>

    <form hx-post="/identify" hx-target="#result-container" class="mb-6">
      <div class="flex items-center border-b border-gray-400 pb-2">
        <input class="appearance-none bg-transparent border-none w-full text-gray-700 mr-3 py-1 px-2 leading-tight focus:outline-none" type="text" placeholder="YouTube or Bandcamp URL" aria-label="URL" name="uri" required>
        <button class="flex-shrink-0 bg-blue-500 hover:bg-blue-700 border-blue-500 hover:border-blue-700 text-sm border-4 text-white py-1 px-2 rounded" type="submit">
          Submit
        </button>
      </div>
    </form>

    <div id="result-container"></div>

	<div class="flex items-center space-x-4">
		<p>
			Did you know you can run Barbershop locally?<br>
			No queue, more features, plus it's got cool ASCII art!
		</p>
		<a href="https://github.com/lukechampine/barbershop" target="_" class="inline-flex items-center justify-center p-5 text-base font-medium text-gray-500 rounded-lg bg-gray-50 hover:text-gray-900 hover:bg-gray-100 dark:text-gray-400 dark:bg-gray-800 dark:hover:bg-gray-700 dark:hover:text-white">
			<img src="https://github.githubassets.com/assets/GitHub-Mark-ea2971cee799.png" width="25px" height="25px">
			<span class="w-full" style="padding-left:5px; padding-right:5px">Check out the project on GitHub</span>
			<svg class="w-4 h-4 ms-2 rtl:rotate-180" aria-hidden="true" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 14 10">
				<path stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M1 5h12m0 0L9 1m4 4L9 9"/>
			</svg>
		</a> 
	</div>
  </div>
</body>
</html>
`))

var templJob = template.Must(template.New("job").Funcs(template.FuncMap{
	"embed": func(url string) string {
		url = strings.Replace(url, "watch?v=", "embed/", 1)
		url = strings.Replace(url, "open.spotify.com", "embed.spotify.com", 1)
		return url
	},
}).Parse(`
{{ if ne .State "done" }}
	<div hx-get="/job/{{ .ID }}" hx-trigger="load delay:1s" hx-swap="outerHTML">
		<p><span class="loading font-bold">Status: {{ .State }}</span></p>
	</div>
{{ else if .Error }}
	<div class="text-red-800 font-bold">Error: {{ .Error }}!</div>
{{ else }}
	{{ with .Sample }}
		{{ if .Found }}
			<div class="fade-in">
				<h2 class="text-lg text-gray-800 mb-2">Original sample: <span class="font-bold">{{ .Artist }} — {{ .Title }}</span></h2>
				<div class="link-container">
					{{ with (index .Links "YouTube") }}
						<iframe src="{{ embed . }}" height="400px"></iframe>
					{{ end }}
					{{ with (index .Links "Spotify") }}
						<iframe src="{{ embed . }}" height="100px"></iframe>
					{{ end }}
				</div>
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
			err = errors.New("albums are not supported in the web UI")
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
	path, err := fetchTrack(uri, 10*(1<<20)) // 10 MiB
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
