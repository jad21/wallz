// Package rotation coordinates catalog selection, durable batches, and DMS updates.
package rotation

import (
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/jad21/wallz/internal/catalog"
	"github.com/jad21/wallz/internal/dms"
	"github.com/jad21/wallz/internal/rotation/cycle"
)

const maxImageBytes = 100 * 1024 * 1024

var generationPattern = regexp.MustCompile(`^batch-[0-9a-f]{32}$`)

// Entry is a materialized image retained in the currently published gallery.
type Entry struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Source string `json:"source"`
	Epoch  int    `json:"epoch,omitempty"`
}

// State preserves cursor version 2 and its on-disk JSON names.
type State struct {
	Version            int         `json:"version"`
	Cycle              cycle.State `json:"cycle"`
	Generation         *string     `json:"generation"`
	Batch              []Entry     `json:"batch"`
	BatchPosition      int         `json:"batch_position"`
	CleanupRemote      *string     `json:"cleanup_remote"`
	SelectionPolicy    string      `json:"selection_policy,omitempty"`
	RetiredGeneration  *string     `json:"retired_generation,omitempty"`
	RetiredGenerations []string    `json:"retired_generations,omitempty"`
	NeedsRebalance     bool        `json:"needs_rebalance,omitempty"`
}

// Downloader is replaceable in tests; production downloads commit-pinned RAW URLs.
type Downloader func(catalog.SourceRecord, catalog.Source, string) error

// Options supplies user paths and the source materializer.
type Options struct {
	WallpaperDir string
	CacheFile    string
	DMSPath      string
	BatchSize    int
	Random       *mrand.Rand
	Downloader   Downloader
}

// Engine owns the durable wallpaper rotation workflow.
type Engine struct {
	wallpaperDir string
	cacheFile    string
	dms          dms.Client
	batchSize    int
	random       *mrand.Rand
	downloader   Downloader
	nextDir      string
	managedDir   string
}

// New validates options and supplies secure defaults for randomness and downloads.
func New(options Options) (*Engine, error) {
	if options.WallpaperDir == "" || options.CacheFile == "" || options.DMSPath == "" {
		return nil, errors.New("wallpaper, cache y ruta DMS son obligatorios")
	}
	if options.BatchSize < 1 || options.BatchSize > 100 {
		return nil, errors.New("el lote debe tener entre 1 y 100 imágenes")
	}
	if options.Random == nil {
		options.Random = mrand.New(mrand.NewSource(time.Now().UnixNano()))
	}
	if options.Downloader == nil {
		options.Downloader = downloadRemote
	}
	return &Engine{wallpaperDir: options.WallpaperDir, cacheFile: options.CacheFile, dms: dms.Client{Path: options.DMSPath}, batchSize: options.BatchSize, random: options.Random, downloader: options.Downloader, nextDir: filepath.Join(options.WallpaperDir, ".next"), managedDir: filepath.Join(filepath.Dir(options.CacheFile), "rotate-wallpaper")}, nil
}

// Prepare publishes a complete prepared batch without changing DMS selection.
func (e *Engine) Prepare() error {
	return e.withLock(true, func() error {
		records, err := e.readCatalog()
		if err != nil {
			return err
		}
		state, err := e.load(records)
		if err != nil {
			return err
		}
		_, err = e.prepareLocked(records, state)
		return err
	})
}

// Rotate applies one already-prepared item and commits the cursor after DMS succeeds.
func (e *Engine) Rotate() (int, error) {
	var remaining int
	err := e.withLock(false, func() error {
		state, err := e.load(nil)
		if err != nil {
			return err
		}
		previousPosition := state.BatchPosition
		state, err = e.syncDMSSelection(state)
		if err != nil {
			return err
		}
		if state.BatchPosition >= len(state.Batch) {
			if previousPosition < len(state.Batch) {
				remaining = 0
				return nil
			}
			return errors.New("lote agotado; espere la preparación en segundo plano")
		}
		entry := state.Batch[state.BatchPosition]
		selected := filepath.Join(e.nextDir, filepath.FromSlash(entry.File))
		if err := e.dms.Set(selected); err != nil {
			return err
		}
		state.Cycle, err = cycle.MarkApplied(state.Cycle, entry.Source)
		if err != nil {
			return err
		}
		state.BatchPosition++
		state.CleanupRemote = nil
		if strings.HasPrefix(entry.File, "downloads/") {
			state.CleanupRemote = stringPointer(entry.File)
		}
		if err := e.save(state); err != nil {
			return err
		}
		if err := e.writeCache(selected); err != nil {
			return err
		}
		if err := e.cleanupRetired(state); err != nil {
			return err
		}
		for _, old := range glob(filepath.Join(e.managedDir, "wallpaper-*")) {
			if isRealDir(old) {
				if err := os.RemoveAll(old); err != nil {
					return err
				}
			}
		}
		remaining = len(state.Batch) - state.BatchPosition
		return nil
	})
	return remaining, err
}

// Refill stages missing batch slots while preserving pending entries and visible DMS state.
func (e *Engine) Refill() error {
	if err := e.ensureDirectories(); err != nil {
		return err
	}
	refillLock, err := lockFile(filepath.Join(e.managedDir, ".refill.lock"), true)
	if err != nil {
		return err
	}
	defer unlockClose(refillLock)
	lockPath := filepath.Join(e.managedDir, ".lock")
	for attempt := 0; attempt < 3; attempt++ {
		lock, err := lockFile(lockPath, true)
		if err != nil {
			return err
		}
		records, err := e.readCatalog()
		if err != nil {
			unlockClose(lock)
			return err
		}
		state, err := e.load(records)
		if err != nil {
			unlockClose(lock)
			return err
		}
		if len(state.Batch)-state.BatchPosition >= e.batchSize {
			unlockClose(lock)
			return nil
		}
		cursorPath := filepath.Join(e.nextDir, ".cursor.json")
		snapshot, readErr := os.ReadFile(cursorPath)
		if os.IsNotExist(readErr) {
			snapshot = nil
		} else if readErr != nil {
			unlockClose(lock)
			return readErr
		}
		unlockClose(lock)
		staged, folder, err := e.stageRefill(records, state)
		if err != nil {
			return err
		}
		lock, err = lockFile(lockPath, true)
		if err != nil {
			os.RemoveAll(folder)
			return err
		}
		current, readErr := os.ReadFile(cursorPath)
		if os.IsNotExist(readErr) {
			current = nil
		} else if readErr != nil {
			unlockClose(lock)
			os.RemoveAll(folder)
			return readErr
		}
		if string(snapshot) == string(current) {
			err = e.save(staged)
			unlockClose(lock)
			if err != nil {
				os.RemoveAll(folder)
				return err
			}
			return nil
		}
		unlockClose(lock)
		os.RemoveAll(folder)
	}
	return errors.New("cursor modificado durante la reposición; reintente")
}

// RebindCurrent points DMS at the current gallery copy without consuming another item.
func (e *Engine) RebindCurrent() error {
	return e.withLock(true, func() error {
		state, err := e.load(nil)
		if err != nil {
			return err
		}
		if state.BatchPosition < 1 {
			return errors.New("no hay fondo actual dentro del lote")
		}
		selected := filepath.Join(e.nextDir, filepath.FromSlash(state.Batch[state.BatchPosition-1].File))
		current, err := e.dms.Current()
		if err != nil {
			return err
		}
		if current == "" {
			return errors.New("el fondo actual de DMS no coincide con el lote")
		}
		left, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		right, err := os.ReadFile(selected)
		if err != nil {
			return err
		}
		if GitBlobID(left) != GitBlobID(right) {
			return errors.New("el fondo actual de DMS no coincide con el lote")
		}
		if err := e.dms.Set(selected); err != nil {
			return err
		}
		if err := e.writeCache(selected); err != nil {
			return err
		}
		for _, old := range glob(filepath.Join(e.managedDir, "wallpaper-*")) {
			if isRealDir(old) {
				if err := os.RemoveAll(old); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// SkipAndPrepare retires pending items only after a replacement batch is fully prepared.
func (e *Engine) SkipAndPrepare() (int, error) {
	skipped := 0
	err := e.withLock(true, func() error {
		records, err := e.readCatalog()
		if err != nil {
			return err
		}
		state, err := e.load(records)
		if err != nil {
			return err
		}
		skipped = len(state.Batch) - state.BatchPosition
		for i := 0; i < skipped; i++ {
			state.Cycle, err = cycle.SkipCurrent(state.Cycle)
			if err != nil {
				return err
			}
			state.BatchPosition++
		}
		_, err = e.prepareLocked(records, state)
		return err
	})
	return skipped, err
}

func (e *Engine) withLock(block bool, fn func() error) error {
	if err := e.ensureDirectories(); err != nil {
		return err
	}
	lock, err := lockFile(filepath.Join(e.managedDir, ".lock"), block)
	if err != nil {
		return err
	}
	defer unlockClose(lock)
	return fn()
}

func (e *Engine) ensureDirectories() error {
	info, err := os.Lstat(e.nextDir)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New(".next no puede ser un enlace simbólico")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(e.nextDir, 0o755); err != nil {
		return err
	}
	return os.MkdirAll(e.managedDir, 0o755)
}

func lockFile(path string, block bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if !block {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func unlockClose(f *os.File) {
	if f != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func (e *Engine) readCatalog() ([]catalog.SourceRecord, error) {
	return catalog.ReadJSONL(filepath.Join(e.nextDir, "index.jsonl"))
}

func (e *Engine) load(records []catalog.SourceRecord) (State, error) {
	path := filepath.Join(e.nextDir, ".cursor.json")
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if records == nil {
			return State{}, errors.New("lote no preparado; espere al servicio de preparación")
		}
		ids := recordIDs(records)
		current := e.currentID(records)
		state, err := cycle.InitialState(ids, current, e.random)
		if err != nil {
			return State{}, err
		}
		return State{Version: 2, Cycle: state, Generation: nil, Batch: []Entry{}, BatchPosition: 0, CleanupRemote: nil}, nil
	}
	if err != nil {
		return State{}, err
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(content, &header); err != nil {
		return State{}, fmt.Errorf(".cursor.json inválido: %w", err)
	}
	if header.Version == 1 {
		if records == nil {
			return State{}, errors.New("lote v1 requiere preparación antes de usar el atajo")
		}
		backup := filepath.Join(e.nextDir, ".cursor.v1.backup.json")
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			if err := copyFile(path, backup); err != nil {
				return State{}, err
			}
		}
		ids := recordIDs(records)
		state, err := cycle.InitialState(ids, e.currentID(records), e.random)
		if err != nil {
			return State{}, err
		}
		var old map[string]any
		json.Unmarshal(content, &old)
		generation, _ := old["generation"].(string)
		return State{Version: 2, Cycle: state, Generation: pointerIf(generation), Batch: []Entry{}, BatchPosition: 0, CleanupRemote: nil, SelectionPolicy: "balanced-v1"}, nil
	}
	if header.Version != 2 {
		return State{}, errors.New("versión de .cursor.json desconocida")
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, err
	}
	if state.Cycle.SourceTurn != "local" && state.Cycle.SourceTurn != "remote" || state.Cycle.Position < 0 || state.Cycle.Position > len(state.Cycle.Order) {
		return State{}, errors.New(".cursor.json tiene un ciclo inválido")
	}
	if state.BatchPosition < 0 || state.BatchPosition > len(state.Batch) {
		return State{}, errors.New("posición del lote inválida")
	}
	for _, entry := range state.Batch[state.BatchPosition:] {
		parts := strings.Split(filepath.ToSlash(entry.File), "/")
		if len(parts) != 2 || parts[0] != value(state.Generation) && parts[0] != "downloads" {
			return State{}, errors.New("imagen pendiente ausente o fuera de .next")
		}
		p := filepath.Join(e.nextDir, filepath.FromSlash(entry.File))
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			return State{}, errors.New("imagen pendiente ausente o fuera de .next")
		}
	}
	if state.SelectionPolicy != "balanced-v1" && state.BatchPosition == 0 {
		state.NeedsRebalance = true
	}
	return state, nil
}

func (e *Engine) save(state State) error {
	if state.Version == 0 {
		state.Version = 2
	}
	if state.Batch == nil {
		state.Batch = []Entry{}
	}
	if state.Cycle.Skipped == nil {
		state.Cycle.Skipped = []string{}
	}
	dir := e.nextDir
	f, err := os.CreateTemp(dir, ".cursor-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(state); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(dir, ".cursor.json"))
}

func (e *Engine) currentID(records []catalog.SourceRecord) *string {
	content, err := os.ReadFile(e.cacheFile)
	if err != nil {
		return nil
	}
	path := strings.TrimSpace(string(content))
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	id, err := catalog.HashFile(path, info.Size())
	if err != nil {
		return nil
	}
	for _, record := range records {
		if record.ID == id {
			return &id
		}
	}
	return nil
}

func (e *Engine) prepareLocked(records []catalog.SourceRecord, state State) (State, error) {
	if state.BatchPosition < len(state.Batch) && !state.NeedsRebalance {
		return e.unifyGallery(records, state)
	}
	generation := newGeneration()
	folder := filepath.Join(e.nextDir, generation)
	if err := os.Mkdir(folder, 0o755); err != nil {
		return State{}, err
	}
	if err := os.MkdirAll(filepath.Join(e.nextDir, "downloads"), 0o755); err != nil {
		os.RemoveAll(folder)
		return State{}, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(folder)
			for _, p := range glob(filepath.Join(e.nextDir, "downloads", generation+"-*")) {
				if isRegular(p) {
					os.Remove(p)
				}
			}
		}
	}()
	cycleState := state.Cycle
	batch := []Entry{}
	failed := 0
	turn := cycleState.SourceTurn
	for len(batch) < e.batchSize {
		if failed > len(records)*2 {
			return State{}, errors.New("no hay suficientes fuentes utilizables para completar el lote")
		}
		var err error
		cycleState, err = cycle.EnsureUpcoming(cycleState, recordIDs(records), e.batchSize, e.random)
		if err != nil {
			return State{}, err
		}
		index := cycleState.Position + len(batch)
		epoch := cycleState.Order[index].Epoch
		preferred := index
		want := turn
		for candidate := index; candidate < len(cycleState.Order); candidate++ {
			item := cycleState.Order[candidate]
			if item.Epoch != epoch {
				continue
			}
			record := findRecord(records, item.ID)
			if record != nil && len(sourcesFor(record, want)) > 0 {
				preferred = candidate
				break
			}
		}
		if preferred != index {
			cycleState.Order[index], cycleState.Order[preferred] = cycleState.Order[preferred], cycleState.Order[index]
		}
		item := cycleState.Order[index]
		record := findRecord(records, item.ID)
		if record == nil {
			failed++
			cycleState = removeCycleItem(cycleState, index, item.ID)
			continue
		}
		entry, err := e.materialize(*record, turn, generation, len(batch))
		if err != nil {
			failed++
			cycleState = removeCycleItem(cycleState, index, item.ID)
			continue
		}
		entry.Epoch = item.Epoch
		batch = append(batch, entry)
		if entry.Source == cycle.SourceLocal {
			turn = cycle.SourceRemote
		} else {
			turn = cycle.SourceLocal
		}
	}
	prior := value(state.Generation)
	retired := append([]string{}, state.RetiredGenerations...)
	if state.RetiredGeneration != nil {
		retired = append(retired, *state.RetiredGeneration)
	}
	if prior != "" {
		retired = append(retired, prior)
	}
	retired = unique(retired)
	state.Cycle = cycleState
	state.Generation = &generation
	state.Batch = batch
	state.BatchPosition = 0
	state.SelectionPolicy = "balanced-v1"
	state.RetiredGeneration = stringPointer(prior)
	state.RetiredGenerations = retired
	state.NeedsRebalance = false
	if err := e.save(state); err != nil {
		return State{}, err
	}
	committed = true
	for _, entry := range batch {
		if strings.HasPrefix(entry.File, "downloads/") && prior != "" {
			p := filepath.Join(e.nextDir, filepath.FromSlash(entry.File))
			if isRegular(p) {
				os.Remove(p)
			}
		}
	}
	return state, nil
}

func (e *Engine) materialize(record catalog.SourceRecord, turn, generation string, slot int) (Entry, error) {
	choices := []string{turn}
	if turn == cycle.SourceLocal {
		choices = append(choices, cycle.SourceRemote)
	} else {
		choices = append(choices, cycle.SourceLocal)
	}
	var last error
	for _, kind := range choices {
		for _, source := range sourcesFor(&record, kind) {
			suffix, err := sourceSuffix(source)
			if err != nil {
				last = err
				continue
			}
			name := fmt.Sprintf("%04d%s", slot, suffix)
			target := filepath.Join(e.nextDir, generation, name)
			temp := target + ".part"
			if kind == cycle.SourceRemote {
				err = e.downloader(record, source, temp)
			} else {
				err = catalog.ExtractLocal(source, temp)
			}
			if err == nil {
				err = verifyImage(temp, record)
			}
			if err == nil {
				err = os.Rename(temp, target)
			}
			if err != nil {
				os.Remove(temp)
				last = err
				continue
			}
			return Entry{ID: record.ID, File: generation + "/" + name, Source: kind}, nil
		}
	}
	if last == nil {
		last = errors.New("sin fuentes")
	}
	return Entry{}, fmt.Errorf("ninguna fuente disponible para %s: %w", record.ID, last)
}

func (e *Engine) unifyGallery(records []catalog.SourceRecord, state State) (State, error) {
	if state.Generation == nil || !generationPattern.MatchString(*state.Generation) {
		return state, nil
	}
	changed := false
	for slot := range state.Batch {
		entry := &state.Batch[slot]
		source := filepath.Join(e.nextDir, filepath.FromSlash(entry.File))
		suffix := filepath.Ext(source)
		targetRel := fmt.Sprintf("%s/%04d%s", *state.Generation, slot, suffix)
		target := filepath.Join(e.nextDir, filepath.FromSlash(targetRel))
		if source == target {
			continue
		}
		if !isRegular(source) {
			return State{}, fmt.Errorf("no se puede recuperar imagen usada %s", entry.ID)
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if err := os.Link(source, target); err != nil {
				if err := copyFile(source, target); err != nil {
					return State{}, err
				}
			}
		}
		record := findRecord(records, entry.ID)
		if record != nil {
			if err := verifyImage(target, *record); err != nil {
				return State{}, err
			}
		}
		entry.File = targetRel
		changed = true
	}
	if changed {
		if err := e.save(state); err != nil {
			return State{}, err
		}
	}
	return state, nil
}

func (e *Engine) stageRefill(records []catalog.SourceRecord, state State) (State, string, error) {
	generation := newGeneration()
	folder := filepath.Join(e.nextDir, generation)
	if err := os.Mkdir(folder, 0o755); err != nil {
		return State{}, "", err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(folder)
		}
	}()
	cycleState := state.Cycle
	batch := []Entry{}
	turn := cycleState.SourceTurn
	for _, old := range state.Batch[state.BatchPosition:] {
		source := filepath.Join(e.nextDir, filepath.FromSlash(old.File))
		name := fmt.Sprintf("%04d%s", len(batch), filepath.Ext(source))
		target := filepath.Join(folder, name)
		if err := os.Link(source, target); err != nil {
			if err := copyFile(source, target); err != nil {
				return State{}, "", err
			}
		}
		old.File = generation + "/" + name
		batch = append(batch, old)
	}
	failed := 0
	for len(batch) < e.batchSize {
		if failed > len(records)*2 {
			return State{}, "", errors.New("no hay fuentes suficientes para completar el lote")
		}
		var err error
		cycleState, err = cycle.EnsureUpcoming(cycleState, recordIDs(records), e.batchSize, e.random)
		if err != nil {
			return State{}, "", err
		}
		index := cycleState.Position + len(batch)
		epoch := cycleState.Order[index].Epoch
		preferred := index
		for candidate := index; candidate < len(cycleState.Order); candidate++ {
			item := cycleState.Order[candidate]
			record := findRecord(records, item.ID)
			if item.Epoch == epoch && record != nil && len(sourcesFor(record, turn)) > 0 {
				preferred = candidate
				break
			}
		}
		if preferred != index {
			cycleState.Order[index], cycleState.Order[preferred] = cycleState.Order[preferred], cycleState.Order[index]
		}
		item := cycleState.Order[index]
		record := findRecord(records, item.ID)
		if record == nil {
			failed++
			cycleState = removeCycleItem(cycleState, index, item.ID)
			continue
		}
		entry, err := e.materialize(*record, turn, generation, len(batch))
		if err != nil {
			failed++
			cycleState = removeCycleItem(cycleState, index, item.ID)
			continue
		}
		entry.Epoch = item.Epoch
		batch = append(batch, entry)
		if entry.Source == cycle.SourceLocal {
			turn = cycle.SourceRemote
		} else {
			turn = cycle.SourceLocal
		}
	}
	retired := append([]string{}, state.RetiredGenerations...)
	if state.RetiredGeneration != nil {
		retired = append(retired, *state.RetiredGeneration)
	}
	if state.Generation != nil {
		retired = append(retired, *state.Generation)
	}
	state.Cycle = cycleState
	state.Generation = &generation
	state.Batch = batch
	state.BatchPosition = 0
	state.SelectionPolicy = "balanced-v1"
	state.RetiredGeneration = nil
	state.RetiredGenerations = unique(retired)
	state.NeedsRebalance = false
	committed = true
	return state, folder, nil
}

func (e *Engine) syncDMSSelection(state State) (State, error) {
	current, err := e.dms.Current()
	if err != nil {
		return State{}, err
	}
	chosen := -1
	for slot := state.BatchPosition; slot < len(state.Batch); slot++ {
		if current == filepath.Join(e.nextDir, filepath.FromSlash(state.Batch[slot].File)) {
			chosen = slot
			break
		}
	}
	if chosen < 0 {
		return state, nil
	}
	for state.BatchPosition < chosen {
		state.Cycle, err = cycle.SkipCurrent(state.Cycle)
		if err != nil {
			return State{}, err
		}
		state.BatchPosition++
	}
	entry := state.Batch[chosen]
	state.Cycle, err = cycle.MarkApplied(state.Cycle, entry.Source)
	if err != nil {
		return State{}, err
	}
	state.BatchPosition++
	if err := e.save(state); err != nil {
		return State{}, err
	}
	if err := e.writeCache(current); err != nil {
		return State{}, err
	}
	return state, nil
}

func (e *Engine) writeCache(selected string) error {
	if err := os.MkdirAll(filepath.Dir(e.cacheFile), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.cacheFile), ".current-wallpaper-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err := fmt.Fprintln(f, selected); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, e.cacheFile)
}

func (e *Engine) cleanupRetired(state State) error {
	retired := append([]string{}, state.RetiredGenerations...)
	if state.RetiredGeneration != nil {
		retired = append(retired, *state.RetiredGeneration)
	}
	for _, generation := range unique(retired) {
		if !generationPattern.MatchString(generation) {
			continue
		}
		old := filepath.Join(e.nextDir, generation)
		if isRealDir(old) {
			if err := os.RemoveAll(old); err != nil {
				return err
			}
		}
		for _, p := range glob(filepath.Join(e.nextDir, "downloads", generation+"-*")) {
			if isRegular(p) {
				if err := os.Remove(p); err != nil {
					return err
				}
			}
		}
	}
	if len(retired) > 0 {
		state.RetiredGeneration = nil
		state.RetiredGenerations = []string{}
		return e.save(state)
	}
	return nil
}

func verifyImage(path string, record catalog.SourceRecord) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > maxImageBytes || info.Size() != record.Size {
		return errors.New("imagen demasiado grande o de tamaño inesperado")
	}
	id, err := catalog.HashFile(path, record.Size)
	if err != nil {
		return err
	}
	if id != record.ID {
		return errors.New("hash de imagen distinto al index.jsonl")
	}
	return nil
}

func downloadRemote(record catalog.SourceRecord, source catalog.Source, target string) error {
	u, err := url.Parse(source.URL)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "https" || u.Host != "raw.githubusercontent.com" || len(parts) < 4 || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(parts[2]) {
		return errors.New("URL remota fuera de GitHub RAW o sin commit")
	}
	if record.Size > maxImageBytes {
		return errors.New("imagen remota excede 100 MiB")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != "raw.githubusercontent.com" || req.URL.Scheme != "https" {
			return errors.New("redirección remota inesperada")
		}
		return nil
	}}
	req, _ := http.NewRequest(http.MethodGet, source.URL, nil)
	req.Header.Set("User-Agent", "jad21-wallpaper-rotate/1")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("descarga remota respondió %s", response.Status)
	}
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, io.LimitReader(response.Body, maxImageBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxImageBytes {
		return errors.New("descarga remota demasiado grande")
	}
	return nil
}

func sourceSuffix(source catalog.Source) (string, error) {
	name := source.Member
	if name == "" {
		name = source.Path
	}
	if name == "" {
		u, err := url.Parse(source.URL)
		if err != nil {
			return "", err
		}
		name = u.Path
	}
	suffix := strings.ToLower(filepath.Ext(name))
	switch suffix {
	case ".jpg", ".jpeg", ".png", ".webp", ".avif":
		return suffix, nil
	}
	return "", fmt.Errorf("imagen sin extensión compatible: %s", name)
}
func sourcesFor(record *catalog.SourceRecord, kind string) []catalog.Source {
	if kind == cycle.SourceLocal {
		return record.Local
	}
	return record.Remote
}
func findRecord(records []catalog.SourceRecord, id string) *catalog.SourceRecord {
	for i := range records {
		if records[i].ID == id {
			return &records[i]
		}
	}
	return nil
}
func recordIDs(records []catalog.SourceRecord) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	return ids
}
func removeCycleItem(state cycle.State, index int, id string) cycle.State {
	state.Order = append(state.Order[:index], state.Order[index+1:]...)
	state.Skipped = append(state.Skipped, id)
	return state
}
func newGeneration() string {
	var bytes [16]byte
	if _, err := io.ReadFull(crand.Reader, bytes[:]); err != nil {
		panic(err)
	}
	return "batch-" + hex.EncodeToString(bytes[:])
}
func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func pointerIf(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
func value(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func unique(items []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, item := range items {
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}
func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
func glob(pattern string) []string { matches, _ := filepath.Glob(pattern); return matches }
func isRegular(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
func isRealDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}
func GitBlobID(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
