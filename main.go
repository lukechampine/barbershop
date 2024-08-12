package main

import (
	"fmt"
	"log"
	"net/http"

	tea "github.com/charmbracelet/bubbletea"
	"lukechampine.com/flagg"
)

var (
	rootUsage = `Usage:
    barbershop [flags] [action]

Actions:
    id            identify a sample
    serve         run as a service
`
	versionUsage = rootUsage
	idUsage      = `Usage:
    barbershop id [flags] [uri]

Attempts to identify the original track(s) sampled in the provided URI,
which must be a filepath or a URL.
`
)

func main() {
	log.SetFlags(0)
	rootCmd := flagg.Root
	rootCmd.Usage = flagg.SimpleUsage(rootCmd, rootUsage)
	versionCmd := flagg.New("version", versionUsage)
	idCmd := flagg.New("id", idUsage)
	idCmd.BoolVar(&bb.silent, "silent", false, "don't play audio")
	track := idCmd.Int("track", 0, "identify the n-th track of the album")
	manual := idCmd.Bool("manual", false, "control speed and sample offset manually")
	srvCmd := flagg.New("serve", "run as a service")

	cmd := flagg.Parse(flagg.Tree{
		Cmd: rootCmd,
		Sub: []flagg.Tree{
			{Cmd: versionCmd},
			{Cmd: idCmd},
			{Cmd: srvCmd},
		},
	})
	args := cmd.Args()

	switch cmd {
	case rootCmd, versionCmd:
		if len(args) > 0 {
			cmd.Usage()
			return
		}
		fmt.Println("Barbershop v0.1.0")

	case idCmd:
		if len(args) != 1 {
			cmd.Usage()
			return
		}
		uri, isAlbum, err := resolveURI(args[0])
		if err != nil {
			log.Fatalln("Error:", err)
		} else if !isAlbum && *track != 0 {
			log.Fatalln("Error: --track flag is only valid for albums")
		} else if isAlbum && *manual && *track == 0 {
			log.Fatalln("Error: --manual flag is only valid for single tracks")
		}
		var m tea.Model
		if isAlbum && *track == 0 {
			m = newAlbumModel(uri)
		} else if *manual {
			m = newManualModel(uri, *track)
		} else {
			m = newSingleModel(uri, *track)
		}
		p := tea.NewProgram(m)
		if _, err := p.Run(); err != nil {
			log.Fatalln("Error:", err)
		}

	case srvCmd:
		srv, err := newServer(".")
		if err != nil {
			log.Fatalln("Error:", err)
		}
		log.Println("Listening on :8080...")
		if err := http.ListenAndServe(":8080", srv); err != nil {
			log.Fatalln(err)
		}
	}
}
