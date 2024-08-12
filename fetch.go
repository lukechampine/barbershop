package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"lukechampine.com/barbershop/shazam"
)

func execCmd(prog string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(prog, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New(prog + " not found")
		}
		return nil, errors.New(stderr.String())
	}
	return stdout.Bytes(), nil
}

func cmdFetchLinks(appleID string) tea.Cmd {
	return func() tea.Msg {
		if appleID == "" {
			return msgLinks{make(map[string]string)}
		}
		links, err := shazam.Links(appleID)
		if err != nil {
			return msgError{err}
		}
		return msgLinks{links}
	}
}

type mediaURI interface {
	isURI()
}

func (mediaFile) isURI()     {}
func (mediaBandcamp) isURI() {}
func (mediaYouTube) isURI()  {}

type mediaFile struct {
	Path string
}

type mediaBandcamp struct {
	ArtistID string
	Slug     string
}

type mediaYouTube struct {
	ID       string
	Title    string
	Chapters []struct {
		Title string
	}
}

func resolveURI(uri string) (mediaURI, bool, error) {
	if stat, err := os.Stat(uri); err == nil {
		return mediaFile{
			Path: path.Clean(uri),
		}, stat.IsDir(), nil
	} else if strings.Contains(uri, "bandcamp.com") {
		// normalize
		if !strings.Contains(uri, "://") {
			uri = "https://" + uri
		}
		u, err := url.Parse(uri)
		if err != nil {
			return nil, false, err
		}
		host := strings.Split(u.Hostname(), ".")
		var artistID string
		for i := range host {
			if host[i] == "bandcamp" {
				artistID = host[i-1]
			}
		}
		_, slug := path.Split(u.Path)
		return mediaBandcamp{
			ArtistID: artistID,
			Slug:     slug,
		}, strings.Contains(uri, "bandcamp.com/album"), nil
	}
	// assume YouTube; try fetching it
	var ytpl mediaYouTube
	if out, err := execCmd("yt-dlp", "-J", "--flat-playlist", uri); err != nil {
		return nil, false, errors.New("only YouTube and Bandcamp URLs are supported")
	} else if err := json.Unmarshal(out, &ytpl); err != nil {
		return nil, false, err
	}
	return ytpl, len(ytpl.Chapters) > 0, nil
}

func fetchTrack(uri mediaURI) (string, error) {
	switch uri := uri.(type) {
	case mediaFile:
		return uri.Path, nil
	case mediaBandcamp:
		path := os.TempDir() + "/barbershop_bandcamp_" + url.PathEscape(uri.ArtistID+"_"+uri.Slug) + ".wav"
		url := fmt.Sprintf("https://%v.bandcamp.com/track/%v", uri.ArtistID, uri.Slug)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if _, err := execCmd("yt-dlp", "-x", "--audio-format", "wav", "-o", path, "--", url); err != nil {
			return "", err
		}
		return path, nil
	case mediaYouTube:
		path := os.TempDir() + "/barbershop_youtube_" + url.PathEscape(uri.Title) + ".wav"
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if _, err := execCmd("yt-dlp", "-x", "--audio-format", "wav", "-o", path, "--", uri.ID); err != nil {
			return "", err
		}
		return path, nil
	default:
		panic(fmt.Sprintf("unhandled mediaURI type: %T", uri))
	}
}

type playlistEntry struct {
	Title string
	URI   mediaURI
}

type playlist struct {
	Title   string
	Entries []playlistEntry
}

func fetchPlaylist(uri mediaURI) (playlist, error) {
	switch uri := uri.(type) {
	case mediaFile:
		pl := playlist{
			Title: uri.Path,
		}
		files, err := os.ReadDir(uri.Path)
		if err != nil {
			return playlist{}, err
		}
		for _, file := range files {
			pl.Entries = append(pl.Entries, playlistEntry{
				Title: file.Name(),
				URI: mediaFile{
					Path: uri.Path + "/" + file.Name(),
				},
			})
		}
		return pl, nil

	case mediaBandcamp:
		var bcpl struct {
			ArtistID string `json:"uploader_id"`
			Title    string
			Entries  []struct {
				Title string
				URL   string
			}
		}
		url := fmt.Sprintf("https://%v.bandcamp.com/album/%v", uri.ArtistID, uri.Slug)
		if out, err := execCmd("yt-dlp", "-J", "--flat-playlist", url); err != nil {
			return playlist{}, err
		} else if err := json.Unmarshal(out, &bcpl); err != nil {
			return playlist{}, err
		}
		pl := playlist{
			Title:   bcpl.ArtistID + " - " + bcpl.Title, // TODO: fetch a nicer artist name
			Entries: make([]playlistEntry, len(bcpl.Entries)),
		}
		for i := range pl.Entries {
			_, slug, _ := strings.Cut(bcpl.Entries[i].URL, "track/")
			pl.Entries[i].Title = bcpl.Entries[i].Title
			pl.Entries[i].URI = mediaBandcamp{
				ArtistID: uri.ArtistID,
				Slug:     slug,
			}
		}
		return pl, nil

	case mediaYouTube:
		pl := playlist{
			Title:   uri.Title,
			Entries: make([]playlistEntry, len(uri.Chapters)),
		}
		dst := os.TempDir() + "/barbershop_youtube_" + uri.ID
		for i := range pl.Entries {
			pl.Entries[i].Title = uri.Chapters[i].Title
			pl.Entries[i].URI = mediaFile{
				Path: fmt.Sprintf("%v/%v - %03d %v [%v].wav", dst, pl.Title, i+1, uri.Chapters[i].Title, uri.ID),
			}
		}
		// download, if we haven't already necessary
		for _, e := range pl.Entries {
			if _, err := os.Stat(e.URI.(mediaFile).Path); err != nil {
				dst := os.TempDir() + "/barbershop_youtube_" + uri.ID
				if _, err := execCmd("yt-dlp", "-x", "--audio-format", "wav", "--split-chapters", "-P", dst, uri.ID); err != nil {
					return playlist{}, err
				}
				break
			}
		}
		return pl, nil

	default:
		panic(fmt.Sprintf("unhandled mediaURI type: %T", uri))
	}
}
