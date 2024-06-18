package main

import (
	"fmt"
	"log"

	tea "github.com/charmbracelet/bubbletea"
	"lukechampine.com/flagg"
)

var (
	rootUsage = `Usage:
    barbershop [flags] [action]

Actions:
    id            identify a sample
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
	idCmd.BoolVar(&bb.silent, "silent", false, "don't play audio during identification")

	cmd := flagg.Parse(flagg.Tree{
		Cmd: rootCmd,
		Sub: []flagg.Tree{
			{Cmd: versionCmd},
			{Cmd: idCmd},
		},
	})
	args := cmd.Args()

	switch cmd {
	case rootCmd, versionCmd:
		fmt.Println("Barbershop v0.1.0")

	case idCmd:
		if len(args) != 1 {
			cmd.Usage()
			return
		}
		uri, isAlbum, err := resolveURI(args[0])
		if err != nil {
			log.Fatalln("Error:", err)
		}
		var m tea.Model
		if isAlbum {
			m = newAlbumModel(uri)
		} else {
			m = newSingleModel(uri)
		}
		p := tea.NewProgram(m)
		if _, err := p.Run(); err != nil {
			log.Fatalln("Error:", err)
		}
	}
}
