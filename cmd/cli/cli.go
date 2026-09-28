// Package cli adapts stable Wallz command lines to the domain workflows.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jad21/wallz/internal/catalog"
	"github.com/jad21/wallz/internal/config"
	"github.com/jad21/wallz/internal/indexer"
	"github.com/jad21/wallz/internal/rotation"
)

// Run dispatches either the unified wallz command or the compatibility binary aliases.
func Run(invokedAs string, args []string) int {
	if invokedAs == "rotate-wallpaper" {
		return runRotate(args)
	}
	if invokedAs == "wallpaper-index" {
		return runIndex(args)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "rotate":
		return runRotate(args[1:])
	case "index":
		return runIndex(args[1:])
	case "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "wallz: comando desconocido %q\n", args[0])
		usage()
		return 2
	}
}

func runRotate(args []string) int {
	flags := flag.NewFlagSet("rotate-wallpaper", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	next := flags.Bool("next", false, "aplicar el siguiente fondo preparado")
	prepare := flags.Bool("prepare", false, "preparar el lote completo")
	refill := flags.Bool("refill", false, "reponer los elementos pendientes")
	rebind := flags.Bool("rebind-gallery", false, "reenlazar la galería actual en DMS")
	nextBatch := flags.Bool("next-batch", false, "omitir pendientes y publicar otro lote")
	limit := flags.Int("limit", 0, "tamaño puntual del lote para --next-batch (1-100)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	actions := 0
	for _, active := range []bool{*next, *prepare, *refill, *rebind, *nextBatch} {
		if active {
			actions++
		}
	}
	if actions != 1 {
		fmt.Fprintln(os.Stderr, "rotate-wallpaper: se requiere exactamente una acción")
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "rotate-wallpaper: argumentos inesperados")
		return 2
	}
	limitSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "limit" {
			limitSet = true
		}
	})
	if limitSet && !*nextBatch {
		fmt.Fprintln(os.Stderr, "rotate-wallpaper: --limit solo se puede usar con --next-batch")
		return 2
	}
	if limitSet {
		if _, err := ParseLimit(strconv.Itoa(*limit)); err != nil {
			fmt.Fprintln(os.Stderr, "rotate-wallpaper: --limit debe estar entre 1 y 100")
			return 2
		}
	}
	paths, err := userPaths()
	if err != nil {
		return fail("rotate-wallpaper", err)
	}
	settings, err := config.Load(configPath(paths.executable))
	if err != nil {
		return fail("rotate-wallpaper", err)
	}
	batchSize := settings.BatchSize
	if limitSet {
		batchSize = *limit
	}
	engine, err := rotation.New(rotation.Options{WallpaperDir: paths.wallpaper, CacheFile: paths.cache, DMSPath: paths.dms, BatchSize: batchSize})
	if err != nil {
		return fail("rotate-wallpaper", err)
	}
	switch {
	case *prepare:
		err = engine.Prepare()
	case *refill:
		err = engine.Refill()
	case *rebind:
		err = engine.RebindCurrent()
	case *nextBatch:
		if _, err = engine.SkipAndPrepare(); err == nil {
			_, err = engine.Rotate()
		}
	default:
		_, err = engine.Rotate()
		if err != nil && (strings.Contains(err.Error(), "lote agotado") || strings.Contains(err.Error(), "lote no preparado")) {
			requestRefill()
		}
	}
	if err != nil {
		return fail("rotate-wallpaper", err)
	}
	return 0
}

func runIndex(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "wallpaper-index: se requiere folders o refresh")
		return 2
	}
	command := args[0]
	args = args[1:]
	configPathValue := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "wallpaper-index: --config requiere una ruta")
				return 2
			}
			configPathValue = args[i+1]
			args = append(args[:i], args[i+2:]...)
			i--
		}
	}
	paths, err := userPaths()
	if err != nil {
		return fail("wallpaper-index", err)
	}
	switch command {
	case "folders":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "wallpaper-index: folders requiere una URL GitHub")
			return 2
		}
		folders, err := indexer.Folders(args[0], catalog.FetchJSON)
		if err != nil {
			return fail("wallpaper-index", err)
		}
		for _, folder := range folders {
			fmt.Println(folder)
		}
	case "refresh":
		if len(args) != 0 {
			fmt.Fprintln(os.Stderr, "wallpaper-index: refresh no acepta URL")
			return 2
		}
		if configPathValue == "" {
			configPathValue = configPath(paths.executable)
		}
		settings, err := config.Load(configPathValue)
		if err != nil {
			return fail("wallpaper-index", err)
		}
		count, err := indexer.Refresh(paths.wallpaper, settings, catalog.FetchJSON)
		if err != nil {
			return fail("wallpaper-index", err)
		}
		fmt.Printf("index.jsonl: %d contenidos únicos\n", count)
	default:
		fmt.Fprintf(os.Stderr, "wallpaper-index: comando desconocido %q\n", command)
		return 2
	}
	return 0
}

type paths struct{ wallpaper, cache, dms, executable string }

func userPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	wallpaper := envOr("WALLPAPER_DIR", filepath.Join(home, "Imágenes", "wallpaper"))
	cache := envOr("CACHE_FILE", filepath.Join(home, ".cache", "current-wallpaper"))
	dms := envOr("DMS_BIN", filepath.Join(home, ".nix-profile", "bin", "dms"))
	executable, err := os.Executable()
	if err != nil {
		return paths{}, err
	}
	return paths{wallpaper: wallpaper, cache: cache, dms: dms, executable: executable}, nil
}

func configPath(executable string) string {
	if override := os.Getenv("WALLZ_CONFIG"); override != "" {
		return override
	}
	installed := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "share", "wallz", "config.toml"))
	if _, err := os.Stat(installed); err == nil {
		return installed
	}
	if current, err := os.Getwd(); err == nil {
		candidate := filepath.Join(current, "config.toml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(filepath.Dir(executable), "config.toml")
}

func requestRefill() {
	command := exec.Command("systemctl", "--user", "start", "--no-block", "refill-wallpaper.service")
	if err := command.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "rotate-wallpaper: no se pudo solicitar la reposición")
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func fail(name string, err error) int { fmt.Fprintf(os.Stderr, "%s: %v\n", name, err); return 1 }
func usage() {
	fmt.Fprintln(os.Stderr, "uso: wallz rotate [acción] | wallz index {folders URL|refresh}")
}

// ParseLimit exposes the documented range check for focused CLI tests.
func ParseLimit(value string) (int, error) {
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 100 {
		return 0, errors.New("--limit debe estar entre 1 y 100")
	}
	return limit, nil
}
