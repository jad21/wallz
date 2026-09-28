package catalog

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalEntries indexes regular images and safe archive members without modifying sources.
func LocalEntries(root string) ([]SourceRecord, error) {
	result := []SourceRecord{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == root {
				for _, excluded := range []string{"index", ".next", "repositories"} {
					if entry.Name() == excluded {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if imageExtensions[ext] {
			id, err := hashPath(path, info.Size())
			if err != nil {
				return err
			}
			result = append(result, SourceRecord{ID: id, Size: info.Size(), Local: []Source{{Kind: "file", Path: path}}})
			return nil
		}
		var records []SourceRecord
		switch {
		case strings.HasSuffix(strings.ToLower(path), ".zip"):
			records, err = zipEntries(path)
		case strings.HasSuffix(strings.ToLower(path), ".tar"), strings.HasSuffix(strings.ToLower(path), ".tar.gz"), strings.HasSuffix(strings.ToLower(path), ".tgz"), strings.HasSuffix(strings.ToLower(path), ".tar.bz2"), strings.HasSuffix(strings.ToLower(path), ".tbz"), strings.HasSuffix(strings.ToLower(path), ".tbz2"):
			records, err = tarEntries(path)
		case strings.HasSuffix(strings.ToLower(path), ".7z"), strings.HasSuffix(strings.ToLower(path), ".tar.xz"), strings.HasSuffix(strings.ToLower(path), ".txz"):
			records, err = sevenZipEntries(path)
		}
		if err != nil {
			return err
		}
		result = append(result, records...)
		return nil
	})
	return result, err
}

// ExtractLocal copies exactly one already-validated source to a staging path.
func ExtractLocal(source Source, destination string) error {
	if source.Kind == "file" {
		in, err := os.Open(source.Path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(destination)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	}
	if !safeMember(source.Member) || !imageExtensions[strings.ToLower(filepath.Ext(source.Member))] {
		return fmt.Errorf("miembro local inválido")
	}
	if strings.HasSuffix(strings.ToLower(source.Path), ".zip") {
		archive, err := zip.OpenReader(source.Path)
		if err != nil {
			return err
		}
		defer archive.Close()
		for _, member := range archive.File {
			if member.Name != source.Member {
				continue
			}
			if member.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("miembro ZIP es enlace simbólico")
			}
			reader, err := member.Open()
			if err != nil {
				return err
			}
			defer reader.Close()
			out, err := os.Create(destination)
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, reader)
			return err
		}
		return fmt.Errorf("miembro ZIP ausente: %s", source.Member)
	}
	if strings.HasSuffix(strings.ToLower(source.Path), ".7z") || strings.HasSuffix(strings.ToLower(source.Path), ".xz") || strings.HasSuffix(strings.ToLower(source.Path), ".txz") {
		out, err := os.Create(destination)
		if err != nil {
			return err
		}
		cmd := exec.Command("7z", "x", "-so", "-spd", source.Path, source.Member)
		cmd.Stdout = out
		cmd.Stderr = io.Discard
		err = cmd.Run()
		closeErr := out.Close()
		if err != nil {
			os.Remove(destination)
			return err
		}
		return closeErr
	}
	reader, err := openTarMember(source.Path, source.Member)
	if err != nil {
		return err
	}
	defer reader.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, reader)
	return err
}

func zipEntries(path string) ([]SourceRecord, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	result := []SourceRecord{}
	for _, item := range archive.File {
		if !safeMember(item.Name) || !imageExtensions[strings.ToLower(filepath.Ext(item.Name))] || item.FileInfo().IsDir() || item.Mode()&os.ModeSymlink != 0 {
			continue
		}
		reader, err := item.Open()
		if err != nil {
			return nil, err
		}
		id, size, err := hashReader(reader, item.UncompressedSize64)
		reader.Close()
		if err != nil {
			return nil, err
		}
		result = append(result, SourceRecord{ID: id, Size: size, Local: []Source{{Kind: "zip", Path: path, Member: item.Name}}})
	}
	return result, nil
}

func tarEntries(path string) ([]SourceRecord, error) {
	reader, err := openTar(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	t := tar.NewReader(reader)
	result := []SourceRecord{}
	for {
		header, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || !safeMember(header.Name) || !imageExtensions[strings.ToLower(filepath.Ext(header.Name))] {
			continue
		}
		id, size, err := hashReader(io.LimitReader(t, header.Size), uint64(header.Size))
		if err != nil {
			return nil, err
		}
		result = append(result, SourceRecord{ID: id, Size: size, Local: []Source{{Kind: "tar", Path: path, Member: header.Name}}})
	}
	return result, nil
}

func sevenZipEntries(path string) ([]SourceRecord, error) {
	output, err := exec.Command("7z", "l", "-slt", "-ba", path).Output()
	if err != nil {
		return nil, fmt.Errorf("listar archivo %s: %w", path, err)
	}
	result := []SourceRecord{}
	for _, block := range strings.Split(string(output), "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, " = ")
			if ok {
				fields[key] = value
			}
		}
		name, sizeText := fields["Path"], fields["Size"]
		if !safeMember(name) || !imageExtensions[strings.ToLower(filepath.Ext(name))] || strings.HasSuffix(name, "/") || strings.HasPrefix(fields["Attributes"], "D") || sizeText == "" {
			continue
		}
		var size uint64
		if _, err := fmt.Sscan(sizeText, &size); err != nil {
			return nil, err
		}
		cmd := exec.Command("7z", "x", "-so", "-spd", path, name)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		id, gotSize, hashErr := hashReader(pipe, size)
		waitErr := cmd.Wait()
		if hashErr != nil {
			return nil, hashErr
		}
		if waitErr != nil {
			return nil, fmt.Errorf("leer %s desde %s: %w", name, path, waitErr)
		}
		result = append(result, SourceRecord{ID: id, Size: gotSize, Local: []Source{{Kind: "7z", Path: path, Member: name}}})
	}
	return result, nil
}

func openTar(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return &multiCloser{Reader: gz, closers: []io.Closer{gz, f}}, nil
	}
	if strings.HasSuffix(lower, ".bz2") || strings.HasSuffix(lower, ".tbz") || strings.HasSuffix(lower, ".tbz2") {
		return &multiCloser{Reader: bzip2.NewReader(f), closers: []io.Closer{f}}, nil
	}
	return f, nil
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var first error
	for _, closer := range m.closers {
		if err := closer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func openTarMember(path, name string) (io.ReadCloser, error) {
	reader, err := openTar(path)
	if err != nil {
		return nil, err
	}
	t := tar.NewReader(reader)
	for {
		header, err := t.Next()
		if err != nil {
			reader.Close()
			if err == io.EOF {
				return nil, fmt.Errorf("miembro TAR ausente: %s", name)
			}
			return nil, err
		}
		if header.Name == name {
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				reader.Close()
				return nil, fmt.Errorf("miembro TAR no es archivo regular")
			}
			return &limitedCloser{Reader: io.LimitReader(t, header.Size), closer: reader}, nil
		}
	}
}

type limitedCloser struct {
	io.Reader
	closer io.Closer
}

func (l *limitedCloser) Close() error { return l.closer.Close() }

func safeMember(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func hashPath(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	id, _, err := hashReader(f, uint64(size))
	return id, err
}

func hashReader(reader io.Reader, expected uint64) (string, int64, error) {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", expected)
	n, err := io.Copy(h, reader)
	if err != nil {
		return "", n, err
	}
	if uint64(n) != expected {
		return "", n, fmt.Errorf("tamaño de imagen cambió: esperado %d, obtenido %d", expected, n)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
