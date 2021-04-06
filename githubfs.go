package githubfs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"sync"
	"time"
)

var (
	zeroTime = time.Time{}
)

type fsys struct {
	owner, repo, rev string

	trees map[string]tree
	files map[string][]byte

	treesLock sync.Mutex
	filesLock sync.Mutex
}

func New(owner, repo, revision string) fs.FS {
	return &fsys{owner: owner, repo: repo, rev: revision,
		trees: make(map[string]tree), files: make(map[string][]byte)}
}

type tree struct {
	SHA  string      `json:"sha"`
	URL  string      `json:"url"`
	Tree []treeEntry `json:"tree"`
	name string
}

type blob struct {
	SHA     string `json:"sha"`
	URL     string `json:"url"`
	Content []byte `json:"content"`
	name    string
}

type fileType string

const (
	fileBlob fileType = "blob"
	fileTree fileType = "tree"
)

type treeEntry struct {
	Path string   `json:"path"`
	Mode string   `json:"mode"`
	Type fileType `json:"type"`
	Size int64    `json:"size"`
	URL  string   `json:"url"`
	data *bytes.Reader
}

func (f *treeEntry) isDir() bool {
	return f.Mode == "040000"
}
func (f *treeEntry) Close() error {
	return nil
}
func (f *treeEntry) Read(p []byte) (int, error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.Read(p)
}
func (f *treeEntry) ReadAt(p []byte, off int64) (n int, err error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.ReadAt(p, off)
}
func (f *treeEntry) Seek(offset int64, whence int) (int64, error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.Seek(offset, whence)
}
func (f *treeEntry) Stat() (fs.FileInfo, error) { return f.stat() }
func (f *treeEntry) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.isDir() {
		return nil, errors.New("not a directory")
	}
	var t tree
	err := reqJSON(f.URL, &t)
	if n <= 0 {
		n = len(t.Tree)
	}
	out := make([]fs.DirEntry, n)
	var i int
	for i := range out {
		if i > len(t.Tree)-1 {
			err = io.EOF
			break
		}
		var err error
		out[i], err = t.Tree[i].stat()
		if err != nil {
			break
		}
	}
	if err != nil {
		return out[:i], err
	}
	return out, nil
}

func (f *treeEntry) stat() (*entryInfo, error) {
	return &entryInfo{*f}, nil
}

type entryInfo struct{ treeEntry }

func (f entryInfo) Name() string {
	return f.treeEntry.Path
}
func (f entryInfo) Size() int64 {
	return f.treeEntry.Size
}
func (f entryInfo) Mode() fs.FileMode {
	switch f.treeEntry.Mode {
	case "040000":
		return fs.FileMode(fs.ModeDir | 0755)
	case "100644":
		return fs.FileMode(0644)
	case "100755":
		return fs.FileMode(0755)
	}
	return fs.FileMode(0644)
}
func (f entryInfo) IsDir() bool {
	return f.treeEntry.isDir()
}
func (f entryInfo) ModTime() time.Time {
	return zeroTime
}
func (f entryInfo) Sys() interface{} {
	return f.treeEntry.data
}
func (f entryInfo) Type() fs.FileMode {
	return f.Mode().Type()
}
func (f entryInfo) Info() (fs.FileInfo, error) {
	return f, nil
}

func (f *fsys) Open(path string) (fs.File, error) {
	f.filesLock.Lock()
	defer f.filesLock.Unlock()
	entry, err := f.file(path)
	if err != nil {
		return nil, err
	}
	switch entry.Type {
	case fileBlob:
		content, ok := f.files[path]
		if !ok {
			var blob blob
			err := reqJSON(entry.URL, &blob)
			if err != nil {
				return nil, err
			}
			content = blob.Content
			f.files[path] = content
		}
		entry.data = bytes.NewReader(content)
		return entry, nil
	case fileTree:
		return entry, nil
	}
	return nil, fmt.Errorf("%s: invalid file type", entry.Type)
}

func (f *fsys) tree(path string) (*tree, error) {
	f.treesLock.Lock()
	defer f.treesLock.Unlock()
	if tree, ok := f.trees[path]; ok {
		return &tree, nil
	}
	var tree tree
	var err error
	if path == "." {
		err = reqJSON(f.resURL(f.rev, fileTree), &tree)
	} else {
		var file *treeEntry
		file, err = f.file(path)
		if err == nil && file.Type != fileTree {
			err = fmt.Errorf("%s: not a directory", path)
		}
	}
	if err != nil {
		return nil, err
	}
	f.trees[path] = tree
	return &tree, nil
}

func (f *fsys) file(fpath string) (*treeEntry, error) {
	if fpath == "." {
		tree, err := f.tree(".")
		if err != nil {
			return nil, err
		}
		return &treeEntry{
			Path: f.repo,
			Mode: "04000",
			Type: fileTree,
			Size: 0,
			URL:  tree.URL,
		}, nil
	}
	dir, file := path.Split(fpath)
	if dir == "" {
		dir = "."
	}
	parent, err := f.tree(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range parent.Tree {
		if entry.Path == file {
			return &entry, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f fsys) resURL(name string, filetype fileType) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/git/%ss/%s",
		f.owner, f.repo, filetype, name)
}

func reqJSON(url string, v interface{}) error {
	fmt.Println("1")
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("non-2XX status code from GitHub: %d",
			resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(v)
	if err != nil {
		return err
	}
	return nil
}
