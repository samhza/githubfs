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

type gitHubFS struct {
	owner, repo, rev string

	trees map[string]tree
	files map[string][]byte

	treesLock sync.Mutex
	filesLock sync.Mutex
}

func New(owner, repo, revision string) fs.FS {
	return &gitHubFS{owner: owner, repo: repo, rev: revision,
		trees: make(map[string]tree), files: make(map[string][]byte)}
}

type tree struct {
	SHA  string       `json:"sha"`
	URL  string       `json:"url"`
	Tree []treeMember `json:"tree"`
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

type treeMember struct {
	Path string   `json:"path"`
	Mode string   `json:"mode"`
	Type fileType `json:"type"`
	Size int64    `json:"size"`
	URL  string   `json:"url"`
	data *bytes.Reader
}

func (f *treeMember) isDir() bool {
	return f.Mode == "040000"
}
func (f *treeMember) Close() error {
	return nil
}
func (f *treeMember) Read(p []byte) (int, error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.Read(p)
}
func (f *treeMember) ReadAt(p []byte, off int64) (n int, err error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.ReadAt(p, off)
}
func (f *treeMember) Seek(offset int64, whence int) (int64, error) {
	if f.isDir() {
		return 0, errors.New("is a directory")
	}
	return f.data.Seek(offset, whence)
}
func (f *treeMember) Stat() (fs.FileInfo, error) { return f.stat() }
func (f *treeMember) ReadDir(n int) ([]fs.DirEntry, error) {
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

func (f *treeMember) stat() (*treeMemberInfo, error) {
	return &treeMemberInfo{*f}, nil
}

type treeMemberInfo struct{ treeMember }

func (f treeMemberInfo) Name() string {
	return f.treeMember.Path
}
func (f treeMemberInfo) Size() int64 {
	return f.treeMember.Size
}
func (f treeMemberInfo) Mode() fs.FileMode {
	switch f.treeMember.Mode {
	case "040000":
		return fs.FileMode(fs.ModeDir | 0755)
	case "100644":
		return fs.FileMode(0644)
	case "100755":
		return fs.FileMode(0755)
	}
	return fs.FileMode(0644)
}
func (f treeMemberInfo) IsDir() bool {
	return f.treeMember.isDir()
}
func (f treeMemberInfo) ModTime() time.Time {
	return zeroTime
}
func (f treeMemberInfo) Sys() interface{} {
	return f.treeMember.data
}
func (f treeMemberInfo) Type() fs.FileMode {
	return f.Mode().Type()
}
func (f treeMemberInfo) Info() (fs.FileInfo, error) {
	return f, nil
}

func (f *gitHubFS) Open(path string) (fs.File, error) {
	f.filesLock.Lock()
	defer f.filesLock.Unlock()
	member, err := f.file(path)
	if err != nil {
		return nil, err
	}
	switch member.Type {
	case fileBlob:
		content, ok := f.files[path]
		if !ok {
			var blob blob
			err := reqJSON(member.URL, &blob)
			if err != nil {
				return nil, err
			}
			content = blob.Content
			f.files[path] = content
		}
		member.data = bytes.NewReader(content)
		return member, nil
	case fileTree:
		return member, nil
	}
	return nil, fmt.Errorf("%s: invalid file type", member.Type)
}

func (f *gitHubFS) tree(path string) (*tree, error) {
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
		var file *treeMember
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

func (f *gitHubFS) file(fpath string) (*treeMember, error) {
	if fpath == "." {
		tree, err := f.tree(".")
		if err != nil {
			return nil, err
		}
		return &treeMember{
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
	for _, member := range parent.Tree {
		if member.Path == file {
			return &member, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f gitHubFS) resURL(name string, filetype fileType) string {
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
