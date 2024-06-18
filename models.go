package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

type (
	msgError struct {
		err error
	}
	msgFetchedTrack struct {
		path string
	}
	msgFetchedPlaylist struct {
		pl playlist
	}
	msgIdentifyResult struct {
		ir    identifyResult
		model *identifyTrackModel // scuffed ID system
	}
	msgLinks struct {
		links map[string]string
	}
)

type spinnerModel struct {
	s spinner.Model
}

func (m *spinnerModel) tick() tea.Msg {
	return m.s.Tick()
}

func (m *spinnerModel) update(msg tea.Msg) tea.Cmd {
	s, cmd := m.s.Update(msg)
	if cmd != nil {
		m.s = s
	}
	return cmd
}

func (m *spinnerModel) view() string {
	return m.s.View()
}

func newSpinner(s spinner.Spinner) spinnerModel {
	return spinnerModel{spinner.New(spinner.WithSpinner(s))}
}

type identifyTrackModel struct {
	// constant
	uri   mediaURI
	title string

	// variable
	status  string
	path    string
	params  []identifyParams
	results []identifyResult
	sample  identifyResult

	// submodels
	spinner spinnerModel
}

type identifyParams struct {
	speedup float64
	offset  time.Duration
}

func newIdentifyTrackModel(uri mediaURI, title string) *identifyTrackModel {
	var params []identifyParams
	for _, speedup := range []float64{1.20, 1.30, 1.10, 1.25, 1.15, 1.40, 1.50, 0.90, 0.80, 1.60, 1.70, 1.80, 1.90, 2.00, 1.00} {
		for _, offset := range []time.Duration{24 * time.Second, 36 * time.Second, 60 * time.Second} {
			params = append(params, identifyParams{speedup, offset})
		}
	}

	cassette := spinner.Line
	cassette.FPS = time.Second / 6
	return &identifyTrackModel{
		uri:     uri,
		title:   title,
		status:  "queued",
		params:  params,
		spinner: newSpinner(cassette),
	}
}

func (m *identifyTrackModel) init() tea.Cmd {
	m.status = "fetching"
	return tea.Batch(m.spinner.tick, cmdFetchTrack(m.uri))
}

func (m *identifyTrackModel) cmdStartIdentifying(path string) tea.Cmd {
	m.status = "identifying"
	m.path = path
	return tea.Sequence(
		func() tea.Msg {
			if err := boomboxFadeIn(path); err != nil {
				return msgError{err}
			}
			return nil
		},
		m.cmdTryNextParams(),
	)
}

func (m *identifyTrackModel) cmdHandleResult(r identifyResult) tea.Cmd {
	m.params = m.params[1:]

	if !r.res.Found {
		// skip to the next speedup
		for len(m.params) > 0 && m.params[0].speedup == r.ratio {
			m.params = m.params[1:]
		}
	} else {
		// have we found a match?
		m.results = append(m.results, r)
		hits := 0
		for _, res := range m.results {
			if res.res.Artist == r.res.Artist && res.res.Title == r.res.Title {
				hits++
			}
		}
		if hits == 3 {
			m.status = "done"
			m.sample = r
			return nil
		}
	}

	// have we exhausted all params?
	if len(m.params) == 0 {
		m.status = "done"
		return nil
	}
	return m.cmdTryNextParams()
}

func (m *identifyTrackModel) skip() {
	m.status = "skipped"
}

func (m *identifyTrackModel) render() string {
	var sb strings.Builder
	switch m.status {
	case "queued":
		fmt.Fprintf(&sb, "...")
	case "fetching":
		s := m.spinner.s
		s.Spinner = spinner.Ellipsis
		fmt.Fprintf(&sb, "⬇  Fetching%-3v  ⬇", s.View())
	case "identifying":
		dots := "?∙∙"
		if m.params[0].offset == 36*time.Second {
			dots = "✔?∙"
		} else if m.params[0].offset == 60*time.Second {
			dots = "✔✔?"
		}
		fmt.Fprintf(&sb, "(%v)  Trying %.2fx %v  (%v)", m.spinner.view(), m.params[0].speedup, dots, m.spinner.view())
	case "skipped":
		fmt.Fprintf(&sb, "<skipped>")
	case "done":
		if m.sample.res.Found {
			fmt.Fprintf(&sb, "✔  %v - %v (%.0f%% match @ %.2fx speed)", m.sample.res.Artist, m.sample.res.Title, 100-m.sample.skew*100, m.sample.ratio)
		} else {
			fmt.Fprintf(&sb, "X  Match not found :/")
		}
	}
	return sb.String()
}

func (m *identifyTrackModel) cmdTryNextParams() tea.Cmd {
	speedup, offset := m.params[0].speedup, m.params[0].offset
	path := m.path
	return tea.Batch(
		func() tea.Msg {
			boomboxChangeSpeed(speedup)
			return nil
		},
		func() tea.Msg {
			res, err := identifyPath(path, speedup, offset)
			if err != nil {
				return msgError{err}
			}
			return msgIdentifyResult{res, m}
		},
	)
}

type identifyAlbumModel struct {
	uri   mediaURI
	title string
	width int
	err   error

	// submodels
	spinner    spinnerModel
	tracks     []*identifyTrackModel
	trackIndex int
}

func newAlbumModel(uri mediaURI) *identifyAlbumModel {
	return &identifyAlbumModel{
		uri:     uri,
		spinner: newSpinner(spinner.Moon),
	}
}

func (m *identifyAlbumModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.tick,
		cmdFetchPlaylist(m.uri),
	)
}

func (m *identifyAlbumModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			cmds = append(cmds, tea.Quit)
		case "s":
			if len(m.tracks) == 0 || m.trackIndex >= len(m.tracks) {
				return m, nil
			}
			m.tracks[m.trackIndex].skip()
			m.trackIndex++
			if m.trackIndex < len(m.tracks) {
				cmds = append(cmds, m.tracks[m.trackIndex].init())
			} else {
				boomboxFadeOut()
				cmds = append(cmds, tea.Quit)
			}
		}

	case msgError:
		m.err = msg.err
		cmds = append(cmds, tea.Quit)

	case spinner.TickMsg:
		cmds = append(cmds, m.spinner.update(msg))
		for _, t := range m.tracks {
			cmds = append(cmds, t.spinner.update(msg))
		}

	case msgFetchedPlaylist:
		m.title = msg.pl.Title
		for _, t := range msg.pl.Entries {
			if n := runewidth.StringWidth(t.Title); n > m.width {
				m.width = n
			}
		}
		m.tracks = make([]*identifyTrackModel, len(msg.pl.Entries))
		for i, t := range msg.pl.Entries {
			m.tracks[i] = newIdentifyTrackModel(t.URI, t.Title)
		}
		// TODO: handle empty playlists
		cmds = append(cmds, m.tracks[0].init())

	case msgFetchedTrack:
		cmds = append(cmds, m.tracks[m.trackIndex].cmdStartIdentifying(msg.path))

	case msgIdentifyResult:
		if m.trackIndex >= len(m.tracks) || msg.model != m.tracks[m.trackIndex] {
			break
		}
		cmds = append(cmds, m.tracks[m.trackIndex].cmdHandleResult(msg.ir))
		if m.tracks[m.trackIndex].status == "done" {
			m.trackIndex++
			if m.trackIndex < len(m.tracks) {
				cmds = append(cmds, m.tracks[m.trackIndex].init())
			} else {
				boomboxFadeOut()
				cmds = append(cmds, tea.Quit)
			}
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *identifyAlbumModel) View() string {
	if m.title == "" {
		return m.spinner.view() + " Fetching album\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "💿 %v\n\n", m.title)
	for i, t := range m.tracks {
		fmt.Fprintf(&sb, "%2v. %v   %v\n", i+1, runewidth.FillRight(t.title, m.width), t.render())
	}
	fmt.Fprint(&sb, "\n[s] skip    [q] quit")
	if m.err != nil {
		fmt.Fprintf(&sb, "\nError: %v", m.err)
	}
	return sb.String()
}

type identifySingleModel struct {
	uri     mediaURI
	track   int
	spinner spinnerModel
	m       *identifyTrackModel
	links   map[string]string
	err     error
}

func newSingleModel(uri mediaURI, track int) *identifySingleModel {
	return &identifySingleModel{
		uri:     uri,
		track:   track,
		spinner: newSpinner(spinner.Moon),
	}
}

func (m *identifySingleModel) Init() tea.Cmd {
	if m.track > 0 {
		return tea.Batch(m.spinner.tick, cmdFetchPlaylistTrack(m.uri, m.track))
	}
	return tea.Batch(m.spinner.tick, cmdFetchTrack(m.uri))
}

func (m *identifySingleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			cmds = append(cmds, tea.Quit)
		}

	case msgError:
		m.err = msg.err
		cmds = append(cmds, tea.Quit)

	case spinner.TickMsg:
		cmds = append(cmds, m.spinner.update(msg))
		if m.m != nil {
			cmds = append(cmds, m.m.spinner.update(msg))
		}

	case msgFetchedTrack:
		m.m = newIdentifyTrackModel(m.uri, "")
		cmds = append(cmds, m.m.spinner.tick, m.m.cmdStartIdentifying(msg.path))

	case msgIdentifyResult:
		cmds = append(cmds, m.m.cmdHandleResult(msg.ir))
		if m.m.status == "done" {
			cmds = append(cmds, cmdFetchLinks(m.m.sample.res.AppleID))
		}

	case msgLinks:
		m.links = msg.links
		cmds = append(cmds, tea.Quit)
	}
	return m, tea.Batch(cmds...)
}

func (m *identifySingleModel) View() string {
	var sb strings.Builder
	if m.m == nil {
		fmt.Fprintf(&sb, "%v Fetching track...", m.spinner.view())
	} else {
		fmt.Fprintf(&sb, "%v\n", m.m.render())
		if m.m.status == "done" {
			if m.links == nil {
				fmt.Fprintf(&sb, "%v Fetching links\n", m.spinner.view())
			} else if len(m.links) == 0 {
				fmt.Fprintf(&sb, "   Streaming links not found :/\n")
			} else {
				for site, link := range m.links {
					fmt.Fprintf(&sb, "  %v: %v\n", site, link)
				}
			}
		}
	}
	fmt.Fprint(&sb, "\n[q] quit")
	if m.err != nil {
		fmt.Fprintf(&sb, "\nError: %v\n", m.err)
	}
	return sb.String()
}
