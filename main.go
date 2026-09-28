// Objetivo: dejar una sola entrada productiva y delegar comandos al paquete CLI.
package main

import (
	"os"
	"path/filepath"

	"github.com/jad21/wallz/cmd/cli"
)

func main() { os.Exit(cli.Run(filepath.Base(os.Args[0]), os.Args[1:])) }
