package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
		ir identifyResult
	}
	msgLinks struct {
		links map[string]string
	}
)

func renderTime(offset time.Duration) string {
	return fmt.Sprintf("%02v:%02v", int(offset.Minutes()), int((offset % time.Minute).Seconds()))
}

func renderRatio(ratio float64) string {
	color := lipgloss.Color([]string{
		"129", "039", "050",
		"046", "190", "226",
		"208", "202", "196",
	}[min(max(int(math.Round(20*ratio)-14)/2, 0), 8)])
	return lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("%.2fx", ratio))
}

type spinnerModel struct {
	s  spinner.Model
	mu sync.Mutex
}

func (m *spinnerModel) setFPS(fps time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Spinner.FPS = fps
}

func (m *spinnerModel) tick() tea.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	return spinnerModel{s: spinner.New(spinner.WithSpinner(s))}
}

type identifyTrackModel struct {
	uri     mediaURI
	title   string
	status  string
	id      *trackIdentifier
	spinner spinnerModel
}

func newIdentifyTrackModel(uri mediaURI, title string) *identifyTrackModel {
	return &identifyTrackModel{
		uri:    uri,
		title:  title,
		status: "queued",
		spinner: newSpinner(spinner.Spinner{
			Frames: spinner.Line.Frames,
			FPS:    time.Second / 6,
		}),
	}
}

func (m *identifyTrackModel) init() tea.Cmd {
	m.status = "fetching"
	return tea.Batch(m.spinner.tick, cmdFetchTrack(m.uri))
}

func (m *identifyTrackModel) cmdStartIdentifying(path string) tea.Cmd {
	m.status = "identifying"
	m.id = newTrackIdentifier(path)
	return tea.Sequence(
		func() tea.Msg {
			if err := boomboxFadeIn(path); err != nil {
				return msgError{err}
			}
			return nil
		},
		m.cmdTryNextParams(m.id.currentParams()),
	)
}

func (m *identifyTrackModel) cmdHandleResult(r identifyResult) tea.Cmd {
	nextParams := m.id.handleResult(r)
	if nextParams == nil {
		m.status = "done"
		return nil
	}
	return m.cmdTryNextParams(*nextParams)
}

func (m *identifyTrackModel) cmdTryNextParams(p identifyParams) tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			boomboxChangeSpeed(p.ratio)
			return nil
		},
		func() tea.Msg {
			res, err := identifyPath(m.id.path, p)
			if err != nil {
				return msgError{err}
			}
			return msgIdentifyResult{res}
		},
	)
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
		p := m.id.currentParams()
		if p.offset == 36*time.Second {
			dots = "✔?∙"
		} else if p.offset == 60*time.Second {
			dots = "✔✔?"
		}
		fmt.Fprintf(&sb, "(%v)  Trying %v %v  (%v)", m.spinner.view(), renderRatio(p.ratio), dots, m.spinner.view())
	case "skipped":
		fmt.Fprintf(&sb, "<skipped>")
	case "done":
		if s := m.id.sample; s != nil {
			fmt.Fprintf(&sb, "✔  %v - %v (%.0f%% match @ %v speed)", s.res.Artist, s.res.Title, 100*(1-s.skew), s.params.ratio)
		} else {
			fmt.Fprintf(&sb, "X  Match not found :/")
		}
	}
	return sb.String()
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
		if m.trackIndex >= len(m.tracks) || msg.ir.params != m.tracks[m.trackIndex].id.currentParams() {
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

type cassetteModel struct {
	speedup float64
	offset  time.Duration
	gears   spinnerModel
	noise   spinnerModel
}

func newCassetteModel() *cassetteModel {
	cycle := func(a string) (frames []string) {
		for i := 0; i < runewidth.StringWidth(a); i++ {
			frames = append(frames, runewidth.Truncate(runewidth.TruncateLeft(a, i, "")+runewidth.Truncate(a, i, ""), 9, ""))
		}
		return
	}
	gears := spinner.Spinner{
		Frames: []string{"╱", "─", "╲", "│"},
		FPS:    time.Second / 5,
	}
	noise := spinner.Spinner{
		Frames: cycle(`"~-,._.,-~` + `"-,._.,-~` + "¯`·....·´" + "``'-.,_,.-'" + ".·''·..·''·." + ",-*~'`^`'~*-,._.,-*" + "¤ø,..,ø¤º°`°º"),
		FPS:    time.Second / 20,
	}
	return &cassetteModel{
		speedup: 1,
		offset:  0,
		gears:   newSpinner(gears),
		noise:   newSpinner(noise),
	}
}

func (m *cassetteModel) init() tea.Cmd {
	return tea.Batch(m.gears.tick, m.noise.tick)
}

func (m *cassetteModel) update(msg tea.Msg) tea.Cmd {
	return tea.Batch(m.gears.update(msg), m.noise.update(msg))
}

func (m *cassetteModel) render() string {
	reverse := func(s string) string {
		runes := []rune(s)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return string(runes)
	}

	pos, duration, ratio := boomboxState()
	seekbar := []rune(strings.Repeat("▱", 9))
	copy(seekbar[:min(8, int(9*float64(pos)/float64(duration)))], []rune(strings.Repeat("▰", 9)))
	return fmt.Sprintf(""+
		"   ▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁▁\n"+
		" \u2571│............................│\n"+
		"│ │: %v %v %v :│\n"+
		"│ │: ┌─%v↿%v⇂%v┐ :│\n"+
		"│ │: │   ,─.   ▁▁▁▁▁   ,─.  │ :│\n"+
		"│ │: │  ( %v)) [▁▁▁▁▁] ( %v)) │ :│\n"+
		"│v│: │   `─`   ' ' '   `─`  │ :│\n"+
		"│││: └──────────────────────┘ :│\n"+
		"│││.....\u2571::::o::::::o::::╲.....│\n"+
		"│^│....\u2571:::O::::::::::O:::╲....│\n"+
		"│\u2571`───\u2571────────────────────`───│\n"+
		"`.▁▁▁\u2571 \u2571====\u2571 \u2571=\u2571\u2571=\u2571 \u2571====\u2571▁▁▁\u2571\n"+
		"     `────────────────────'\n",
		m.noise.view(), renderRatio(ratio), runewidth.Truncate(reverse(m.noise.view()), 8, ""),
		renderTime(pos), string(seekbar), renderTime(duration),
		m.gears.view(), m.gears.view())
}

type historyModel struct {
	entries []identifyResult
}

func newHistoryModel() *historyModel {
	return &historyModel{}
}

func (m *historyModel) add(r identifyResult) {
	m.entries = append(m.entries, r)
}

func (m *historyModel) render(n int) string {
	italics := lipgloss.NewStyle().Italic(true).Render
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("046")).Render
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render
	var sb strings.Builder
	for i := max(0, len(m.entries)-n); i < len(m.entries); i++ {
		r := m.entries[i]
		if r.res.Found {
			fmt.Fprintf(&sb, "%v  %v @ %v: %v (%.0f%% match)\n", green("✔️"), renderTime(r.params.offset), renderRatio(r.params.ratio), italics(r.res.Artist+" - "+r.res.Title), 100*(1-r.skew))
		} else {
			fmt.Fprintf(&sb, "%v  %v @ %v: <no match>\n", red("X"), renderTime(r.params.offset), renderRatio(r.params.ratio))
		}
	}
	return sb.String()
}

type identifySingleModel struct {
	uri        mediaURI
	albumIndex int
	id         *trackIdentifier
	moon       spinnerModel
	ellipsis   spinnerModel
	cassette   *cassetteModel
	history    *historyModel
	links      map[string]string
	err        error
}

func newSingleModel(uri mediaURI, albumIndex int) *identifySingleModel {
	return &identifySingleModel{
		uri:        uri,
		albumIndex: albumIndex,
		moon:       newSpinner(spinner.Moon),
		ellipsis: newSpinner(spinner.Spinner{
			Frames: spinner.Ellipsis.Frames,
			FPS:    time.Second / 2,
		}),
		cassette: newCassetteModel(),
		history:  newHistoryModel(),
	}
}

func (m *identifySingleModel) cmdStartIdentifying(path string) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			if err := boomboxFadeIn(path); err != nil {
				return msgError{err}
			}
			return nil
		},
		m.cmdTryNextParams(m.id.currentParams()),
	)
}

func (m *identifySingleModel) cmdTryNextParams(p identifyParams) tea.Cmd {
	return tea.Batch(
		func() tea.Msg {
			boomboxChangeSpeed(p.ratio)
			return nil
		},
		func() tea.Msg {
			res, err := identifyPath(m.id.path, p)
			if err != nil {
				return msgError{err}
			}
			return msgIdentifyResult{res}
		},
	)
}

func (m *identifySingleModel) Init() tea.Cmd {
	fetch := cmdFetchTrack(m.uri)
	if m.albumIndex > 0 {
		fetch = cmdFetchPlaylistTrack(m.uri, m.albumIndex)
	}
	return tea.Batch(fetch, m.moon.tick, m.ellipsis.tick, m.cassette.init())
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
		if m.id != nil {
			_, _, ratio := boomboxState()
			scale := 5 - (4 * (ratio - 1))
			m.cassette.noise.setFPS(time.Duration(float64(time.Second) / 75 * scale))
			m.cassette.gears.setFPS(time.Duration(float64(time.Second) / 30 * scale))
		}
		cmds = append(cmds, m.moon.update(msg), m.ellipsis.update(msg), m.cassette.update(msg))

	case msgFetchedTrack:
		m.id = newTrackIdentifier(msg.path)
		cmds = append(cmds, m.cmdStartIdentifying(msg.path))

	case msgIdentifyResult:
		m.history.add(msg.ir)
		if nextParams := m.id.handleResult(msg.ir); nextParams == nil {
			if m.id.sample != nil {
				cmds = append(cmds, cmdFetchLinks(m.id.sample.res.AppleID))
			} else {
				m.err = fmt.Errorf("no match found")
				cmds = append(cmds, tea.Quit)
			}
		} else {
			cmds = append(cmds, m.cmdTryNextParams(*nextParams))
		}

	case msgLinks:
		m.links = msg.links
		boomboxFadeOut()
		cmds = append(cmds, tea.Quit)
	}
	return m, tea.Batch(cmds...)
}

func (m *identifySingleModel) View() string {
	var sb strings.Builder
	if m.id == nil {
		fmt.Fprintf(&sb, "%v Fetching track...", m.moon.view())
	} else {
		waiting := ""
		if m.id.sample == nil {
			p := m.id.currentParams()
			waiting = fmt.Sprintf("?  %v @ %v: %v\n", renderTime(p.offset), renderRatio(p.ratio), m.ellipsis.view())
		}
		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top,
			lipgloss.NewStyle().MarginLeft(4).MarginRight(4).Render(m.cassette.render()),
			lipgloss.JoinVertical(lipgloss.Left, lipgloss.NewStyle().Underline(true).Render("\nMatches:\n"), m.history.render(8)+waiting),
		))
		if m.id.sample != nil {
			italics := lipgloss.NewStyle().Italic(true).Render
			fmt.Fprintf(&sb, "\n  ✔️  %v\n", italics(m.id.sample.res.Artist+" - "+m.id.sample.res.Title))
			if m.id.sample.res.Album != "" {
				fmt.Fprintf(&sb, "     %v", italics(m.id.sample.res.Album))
				if m.id.sample.res.Year != "" {
					fmt.Fprintf(&sb, " (%v)", m.id.sample.res.Year)
				}
				fmt.Fprintf(&sb, "\n")
			}
			fmt.Fprintf(&sb, "\n")
			if m.links == nil {
				fmt.Fprintf(&sb, "%v Fetching links\n", m.moon.view())
			} else if len(m.links) == 0 {
				fmt.Fprintf(&sb, "   Streaming links not found :/\n")
			} else {
				sites := make([]string, 0, len(m.links))
				for site := range m.links {
					sites = append(sites, site)
				}
				sort.Strings(sites)
				for _, site := range sites {
					fmt.Fprintf(&sb, "  %v: %v\n", site, m.links[site])
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

type identifyManualModel struct {
	uri        mediaURI
	albumIndex int
	path       string
	params     identifyParams
	trying     *identifyParams
	moon       spinnerModel
	ellipsis   spinnerModel
	cassette   *cassetteModel
	history    *historyModel
	links      map[string]string
	err        error
}

func newManualModel(uri mediaURI, albumIndex int) *identifyManualModel {
	return &identifyManualModel{
		uri:        uri,
		albumIndex: albumIndex,
		params:     identifyParams{ratio: 1, offset: 0 * time.Second},
		moon:       newSpinner(spinner.Moon),
		ellipsis: newSpinner(spinner.Spinner{
			Frames: spinner.Ellipsis.Frames,
			FPS:    time.Second / 2,
		}),
		cassette: newCassetteModel(),
		history:  newHistoryModel(),
	}
}

func (m *identifyManualModel) cmdTryParams() tea.Cmd {
	path, params := m.path, m.params
	return func() tea.Msg {
		res, err := identifyPath(path, params)
		if err != nil {
			return msgError{err}
		}
		return msgIdentifyResult{res}
	}
}

func (m *identifyManualModel) Init() tea.Cmd {
	fetch := cmdFetchTrack(m.uri)
	if m.albumIndex > 0 {
		fetch = cmdFetchPlaylistTrack(m.uri, m.albumIndex)
	}
	return tea.Batch(fetch, m.moon.tick, m.ellipsis.tick, m.cassette.init())
}

func (m *identifyManualModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "down":
			delta := 0.01
			if msg.String() == "down" {
				delta *= -1
			}
			m.params.ratio = max(0.5, min(m.params.ratio+delta, 2))
			boomboxSetSpeed(m.params.ratio)
		case "left", "right":
			delta := time.Second
			if msg.String() == "left" {
				delta *= -1
			}
			boomboxSeek(delta)
		case "enter":
			m.params.offset, _, _ = boomboxState()
			p := m.params
			m.trying = &p
			m.links = nil
			cmds = append(cmds, m.cmdTryParams())
		case "l":
			if len(m.history.entries) > 0 && m.history.entries[len(m.history.entries)-1].res.Found {
				m.links = make(map[string]string)
				cmds = append(cmds, cmdFetchLinks(m.history.entries[len(m.history.entries)-1].res.AppleID))
			}
		case "ctrl+c", "q":
			cmds = append(cmds, tea.Quit)
		}

	case msgError:
		m.err = msg.err
		cmds = append(cmds, tea.Quit)

	case spinner.TickMsg:
		if m.path != "" {
			_, _, ratio := boomboxState()
			scale := 5 - (4 * (ratio - 1))
			m.cassette.noise.setFPS(time.Duration(float64(time.Second) / 75 * scale))
			m.cassette.gears.setFPS(time.Duration(float64(time.Second) / 30 * scale))
		}
		cmds = append(cmds, m.moon.update(msg), m.ellipsis.update(msg), m.cassette.update(msg))

	case msgFetchedTrack:
		m.path = msg.path
		cmds = append(cmds, func() tea.Msg {
			if err := boomboxFadeIn(msg.path); err != nil {
				return msgError{err}
			}
			return nil
		})

	case msgIdentifyResult:
		m.trying = nil
		m.history.add(msg.ir)

	case msgLinks:
		m.links = msg.links
		if len(m.links) == 0 {
			m.links["Error"] = "Streaming links not found :/"
		}
	}
	return m, tea.Batch(cmds...)
}

func (m *identifyManualModel) View() string {
	var sb strings.Builder
	if m.path == "" {
		fmt.Fprintf(&sb, "%v Fetching track...", m.moon.view())
	} else {
		waiting := ""
		if m.trying != nil {
			waiting = fmt.Sprintf("?  %v @ %v: %v\n", renderTime(m.trying.offset), renderRatio(m.trying.ratio), m.ellipsis.view())
		}
		links := ""
		if m.links != nil {
			if len(m.links) == 0 {
				links = "   " + m.moon.view() + " Fetching links...\n"
			} else {
				sites := make([]string, 0, len(m.links))
				for site := range m.links {
					sites = append(sites, site)
				}
				sort.Strings(sites)
				for _, site := range sites {
					links += fmt.Sprintf("   %v: %v\n", site, m.links[site])
				}
			}
		}
		sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top,
			lipgloss.NewStyle().MarginLeft(4).MarginRight(4).Render(m.cassette.render()),
			lipgloss.JoinVertical(lipgloss.Left, lipgloss.NewStyle().Underline(true).Render("\nMatches:\n"), m.history.render(8)+waiting+links),
		))
	}
	fmt.Fprint(&sb, "\n[⬆ / ⬇] adjust speed   [⬅ / ⮕] seek   [enter] submit   [l] links   [q] quit")
	if m.err != nil {
		fmt.Fprintf(&sb, "\nError: %v\n", m.err)
	}
	return sb.String()
}
