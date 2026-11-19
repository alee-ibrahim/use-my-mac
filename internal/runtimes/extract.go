package runtimes

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/alee-ibrahim/use-my-mac/pkg/api"
)

// Extraction limits (a package is a tool, not a disk image).
const (
	MaxFiles          = 100000
	MaxExtractedBytes = 8 << 30
)

// extractor writes archive entries under root and never outside it: names
// must be clean relative paths, no parent of a written entry may be a
// symlink, symlinks must be relative, and hard links, devices and setuid
// bits are refused or dropped.
type extractor struct {
	root  string
	files int
	bytes int64
}

func entryName(name string) (string, error) {
	n := strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), "./")
	n = strings.TrimSuffix(n, "/")
	if n == "" || n == "." {
		return "", nil // the archive root itself
	}
	if strings.HasPrefix(n, "/") || strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("archive entry %q is absolute", name)
	}
	c := path.Clean(n)
	if c == ".." || strings.HasPrefix(c, "../") || c != n {
		return "", fmt.Errorf("archive entry %q escapes the package", name)
	}
	return c, nil
}

// parentOK makes sure every existing parent of rel inside root is a real
// directory (not a symlink), creating missing ones.
func (x *extractor) parentOK(rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	cur := x.root
	for _, part := range strings.Split(dir, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("archive writes through symlink %s", strings.TrimPrefix(cur, x.root))
		case !fi.IsDir():
			return fmt.Errorf("archive entry parent %s is not a directory", strings.TrimPrefix(cur, x.root))
		}
	}
	return nil
}

func (x *extractor) count(size int64) error {
	x.files++
	x.bytes += size
	if x.files > MaxFiles {
		return errors.New("archive has too many files")
	}
	if x.bytes > MaxExtractedBytes {
		return errors.New("archive expands too much")
	}
	return nil
}

func (x *extractor) dir(name string) error {
	rel, err := entryName(name)
	if err != nil || rel == "" {
		return err
	}
	if err := x.parentOK(rel); err != nil {
		return err
	}
	p := filepath.Join(x.root, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("archive entry %s is both a file and a directory", rel)
		}
		return nil
	}
	return os.Mkdir(p, 0o755)
}

func (x *extractor) file(name string, mode fs.FileMode, size int64, r io.Reader) error {
	rel, err := entryName(name)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("archive entry %q has no name", name)
	}
	if err := x.count(size); err != nil {
		return err
	}
	if err := x.parentOK(rel); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	p := filepath.Join(x.root, filepath.FromSlash(rel))
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("archive entry %s appears twice", rel)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("archive entry %s is truncated", rel)
	}
	if err != nil {
		return err
	}
	return os.Chmod(p, perm) // umask-proof
}

func (x *extractor) symlink(name, target string) error {
	rel, err := entryName(name)
	if err != nil {
		return err
	}
	if rel == "" || target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) {
		return fmt.Errorf("archive symlink %q -> %q is not allowed (targets must be relative)", name, target)
	}
	if err := x.count(0); err != nil {
		return err
	}
	if err := x.parentOK(rel); err != nil {
		return err
	}
	return os.Symlink(target, filepath.Join(x.root, filepath.FromSlash(rel)))
}

// Extract unpacks the verified download at src into root (which must exist
// and be empty).
func Extract(src string, a api.RuntimeArtifact, root string) error {
	x := &extractor{root: root}
	switch a.Format {
	case api.FormatFile:
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		return x.file(a.File, 0o755, st.Size(), f)
	case api.FormatTarGz:
		return x.tarGz(src)
	case api.FormatZip:
		return x.zip(src)
	}
	return fmt.Errorf("unknown format %q", a.Format)
}

func (x *extractor) tarGz(src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.dir(h.Name)
		case tar.TypeReg:
			err = x.file(h.Name, fs.FileMode(h.Mode), h.Size, tr)
		case tar.TypeSymlink:
			err = x.symlink(h.Name, h.Linkname)
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			err = fmt.Errorf("archive entry %q has an unsupported type (hard links and devices are refused)", h.Name)
		}
		if err != nil {
			return err
		}
	}
}

func (x *extractor) zip(src string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			err = x.dir(zf.Name)
		case mode&fs.ModeSymlink != 0:
			var b []byte
			b, err = readZip(zf, 4096)
			if err == nil {
				err = x.symlink(zf.Name, string(b))
			}
		case mode.IsRegular():
			if zf.UncompressedSize64 > MaxExtractedBytes {
				return errors.New("archive expands too much")
			}
			var rc io.ReadCloser
			rc, err = zf.Open()
			if err == nil {
				err = x.file(zf.Name, mode, int64(zf.UncompressedSize64), rc)
				rc.Close()
			}
		default:
			err = fmt.Errorf("archive entry %q has an unsupported type", zf.Name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func readZip(zf *zip.File, max int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("archive entry %q is too large", zf.Name)
	}
	return b, err
}
